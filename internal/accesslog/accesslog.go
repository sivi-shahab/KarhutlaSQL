package accesslog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/compress"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	"github.com/google/uuid"

	"github.com/bankmega/karhutlasql/internal/objstore"
)

// Prefix awalan key objek access-history.
const Prefix = "access/"

// ErrUnavailable buffer penuh karena penyimpanan gagal: statement baru
// ditolak (fail-closed) sampai flush berhasil.
var ErrUnavailable = errors.New("access history unavailable")

// Record akses satu kolom (atau satu objek bila Column kosong) oleh satu
// statement (G-05).
type Record struct {
	Time          time.Time `json:"ts"`
	RequestID     string    `json:"request_id"`
	SessionID     string    `json:"session_id"`
	User          string    `json:"user"`
	Source        string    `json:"source"`
	Kind          string    `json:"kind"`
	Action        string    `json:"action"` // read | write
	Status        string    `json:"status"`
	Rows          int64     `json:"rows"`
	Table         string    `json:"table_name"`
	Column        string    `json:"column_name,omitempty"`
	Tag           string    `json:"tag,omitempty"`
	Policy        string    `json:"policy,omitempty"`
	RowFilter     string    `json:"row_filter,omitempty"`
	SQLHash       string    `json:"sql_sha256"`
	RewrittenHash string    `json:"rewritten_sql_sha256,omitempty"`
	SQL           string    `json:"sql,omitempty"` // hanya bila --audit-log-sql
}

// Manifest satu objek Parquet; Prev = sha256 manifest sebelumnya dari
// instance yang sama (rantai hash: objek yang hilang/diganti terdeteksi).
type Manifest struct {
	Object   string `json:"object"`
	SHA256   string `json:"sha256"`
	Rows     int    `json:"rows"`
	Instance string `json:"instance"`
	Seq      int64  `json:"seq"`
	Prev     string `json:"prev_manifest_sha256"`
}

// Options konfigurasi Log.
type Options struct {
	Store objstore.Store
	// MaxBuffered batas record belum tersimpan; lewat → ErrUnavailable.
	MaxBuffered int
	Now         func() time.Time
}

// Log access-history append-only.
type Log struct {
	o        Options
	instance string

	mu  sync.Mutex
	buf []Record

	flushMu sync.Mutex // serialisasi Flush & Query (tidak ada celah saat flush)
	seq     int64
	prev    string
}

// New membuat Log; MaxBuffered ≤ 0 → 100.000.
func New(o Options) *Log {
	if o.MaxBuffered <= 0 {
		o.MaxBuffered = 100_000
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Log{o: o, instance: uuid.NewString()[:8]}
}

// Healthy error bila buffer penuh (penyimpanan gagal terlalu lama).
func (l *Log) Healthy() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.buf) >= l.o.MaxBuffered {
		return fmt.Errorf("%w: %d records not persisted to %s", ErrUnavailable, len(l.buf), l.o.Store)
	}
	return nil
}

// Append menambah record ke buffer.
func (l *Log) Append(rs ...Record) {
	l.mu.Lock()
	l.buf = append(l.buf, rs...)
	l.mu.Unlock()
}

// Run mem-flush tiap every sampai ctx selesai, lalu flush terakhir.
func (l *Log) Run(ctx context.Context, every time.Duration, onErr func(error)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			if err := l.Flush(fctx); err != nil {
				onErr(err)
			}
			cancel()
			return
		case <-t.C:
			if err := l.Flush(ctx); err != nil {
				onErr(err)
			}
		}
	}
}

// Flush menulis buffer sebagai satu objek Parquet per hari (UTC) + manifest.
// Gagal → record kembali ke buffer (dicoba lagi berikutnya).
func (l *Log) Flush(ctx context.Context) error {
	l.flushMu.Lock()
	defer l.flushMu.Unlock()
	l.mu.Lock()
	recs := l.buf
	l.buf = nil
	l.mu.Unlock()
	if len(recs) == 0 {
		return nil
	}
	byDay := map[string][]Record{}
	var days []string
	for _, r := range recs {
		d := r.Time.UTC().Format(time.DateOnly)
		if _, ok := byDay[d]; !ok {
			days = append(days, d)
		}
		byDay[d] = append(byDay[d], r)
	}
	for i, d := range days {
		if err := l.write(ctx, d, byDay[d]); err != nil {
			var rest []Record
			for _, dd := range days[i:] {
				rest = append(rest, byDay[dd]...)
			}
			l.mu.Lock()
			l.buf = append(rest, l.buf...)
			l.mu.Unlock()
			return fmt.Errorf("accesslog.Flush: %w", err)
		}
	}
	return nil
}

func (l *Log) write(ctx context.Context, day string, recs []Record) error {
	data, err := encode(recs)
	if err != nil {
		return err
	}
	seq := l.seq + 1
	base := fmt.Sprintf("%sdt=%s/%s-%s-%06d", Prefix, day, l.o.Now().UTC().Format("20060102T150405.000000000Z"), l.instance, seq)
	sum := sha256.Sum256(data)
	m, _ := json.Marshal(Manifest{Object: base + ".parquet", SHA256: hex.EncodeToString(sum[:]), Rows: len(recs),
		Instance: l.instance, Seq: seq, Prev: l.prev}) // struct string/int: tidak pernah error
	if err := l.o.Store.Put(ctx, base+".parquet", data); err != nil {
		return err
	}
	if err := l.o.Store.Put(ctx, base+".manifest.json", m); err != nil {
		return err
	}
	msum := sha256.Sum256(m)
	l.seq, l.prev = seq, hex.EncodeToString(msum[:])
	return nil
}

// Query record dengan Time di [since, until) dari penyimpanan (partisi hari
// yang tercakup) dan buffer.
func (l *Log) Query(ctx context.Context, since, until time.Time) ([]Record, error) {
	l.flushMu.Lock()
	defer l.flushMu.Unlock()
	var out []Record
	keep := func(r Record) {
		if !r.Time.Before(since) && r.Time.Before(until) {
			out = append(out, r)
		}
	}
	for d := since.UTC().Truncate(24 * time.Hour); d.Before(until); d = d.Add(24 * time.Hour) {
		keys, err := l.o.Store.List(ctx, Prefix+"dt="+d.Format(time.DateOnly)+"/")
		if err != nil {
			return nil, fmt.Errorf("accesslog.Query: %w", err)
		}
		for _, k := range keys {
			if !strings.HasSuffix(k, ".parquet") {
				continue
			}
			b, err := l.o.Store.Get(ctx, k)
			if err != nil {
				return nil, fmt.Errorf("accesslog.Query: %w", err)
			}
			recs, err := decode(ctx, b)
			if err != nil {
				return nil, fmt.Errorf("accesslog.Query: %s: %w", k, err)
			}
			for _, r := range recs {
				keep(r)
			}
		}
	}
	l.mu.Lock()
	for _, r := range l.buf {
		keep(r)
	}
	l.mu.Unlock()
	return out, nil
}

// --- Parquet ---

var schema = arrow.NewSchema([]arrow.Field{
	{Name: "ts", Type: &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"}},
	{Name: "request_id", Type: arrow.BinaryTypes.String},
	{Name: "session_id", Type: arrow.BinaryTypes.String},
	{Name: "user", Type: arrow.BinaryTypes.String},
	{Name: "source", Type: arrow.BinaryTypes.String},
	{Name: "kind", Type: arrow.BinaryTypes.String},
	{Name: "action", Type: arrow.BinaryTypes.String},
	{Name: "status", Type: arrow.BinaryTypes.String},
	{Name: "rows", Type: arrow.PrimitiveTypes.Int64},
	{Name: "table_name", Type: arrow.BinaryTypes.String},
	{Name: "column_name", Type: arrow.BinaryTypes.String, Nullable: true},
	{Name: "tag", Type: arrow.BinaryTypes.String, Nullable: true},
	{Name: "policy", Type: arrow.BinaryTypes.String, Nullable: true},
	{Name: "row_filter", Type: arrow.BinaryTypes.String, Nullable: true},
	{Name: "sql_sha256", Type: arrow.BinaryTypes.String},
	{Name: "rewritten_sql_sha256", Type: arrow.BinaryTypes.String, Nullable: true},
	{Name: "sql", Type: arrow.BinaryTypes.String, Nullable: true},
}, nil)

// Schema kolom Parquet / karhutla_access_history().
func Schema() *arrow.Schema { return schema }

// Values record sebagai nilai kolom Schema (string kosong → nil untuk kolom nullable).
func (r Record) Values() []any {
	opt := func(s string) any {
		if s == "" {
			return nil
		}
		return s
	}
	return []any{r.Time.UTC(), r.RequestID, r.SessionID, r.User, r.Source, r.Kind, r.Action, r.Status, r.Rows,
		r.Table, opt(r.Column), opt(r.Tag), opt(r.Policy), opt(r.RowFilter), r.SQLHash, opt(r.RewrittenHash), opt(r.SQL)}
}

func encode(recs []Record) ([]byte, error) {
	b := array.NewRecordBuilder(memory.DefaultAllocator, schema)
	defer b.Release()
	for _, r := range recs {
		for i, v := range r.Values() {
			switch f := b.Field(i).(type) {
			case *array.TimestampBuilder:
				ts, _ := arrow.TimestampFromTime(v.(time.Time), arrow.Microsecond) //nolint:forcetypeassert // kolom 0 = time.Time
				f.Append(ts)
			case *array.Int64Builder:
				f.Append(v.(int64)) //nolint:forcetypeassert // kolom rows = int64
			case *array.StringBuilder:
				if v == nil {
					f.AppendNull()
				} else {
					f.Append(v.(string)) //nolint:forcetypeassert // kolom string
				}
			}
		}
	}
	rec := b.NewRecordBatch()
	defer rec.Release()
	var out bytes.Buffer
	w, err := pqarrow.NewFileWriter(schema, &out,
		parquet.NewWriterProperties(parquet.WithCompression(compress.Codecs.Zstd)), pqarrow.DefaultWriterProps())
	if err != nil {
		return nil, fmt.Errorf("accesslog.encode: %w", err)
	}
	if err := errors.Join(w.Write(rec), w.Close()); err != nil {
		return nil, fmt.Errorf("accesslog.encode: %w", err)
	}
	return out.Bytes(), nil
}

func decode(ctx context.Context, data []byte) ([]Record, error) {
	tbl, err := pqarrow.ReadTable(ctx, bytes.NewReader(data), nil, pqarrow.ArrowReadProperties{}, memory.DefaultAllocator)
	if err != nil {
		return nil, err
	}
	defer tbl.Release()
	tr := array.NewTableReader(tbl, 0)
	defer tr.Release()
	idx := map[string]int{}
	for i, f := range tbl.Schema().Fields() {
		idx[f.Name] = i
	}
	var out []Record
	for tr.Next() {
		rec := tr.RecordBatch()
		str := func(name string, row int) string {
			i, ok := idx[name]
			if !ok {
				return "" // kolom ditambah di versi lebih baru / lebih lama
			}
			c, ok := rec.Column(i).(*array.String)
			if !ok || c.IsNull(row) {
				return ""
			}
			return strings.Clone(c.Value(row))
		}
		ts, _ := rec.Column(idx["ts"]).(*array.Timestamp)
		rows, _ := rec.Column(idx["rows"]).(*array.Int64)
		if ts == nil || rows == nil {
			return nil, errors.New("unexpected schema")
		}
		for i := range int(rec.NumRows()) {
			out = append(out, Record{
				Time: ts.Value(i).ToTime(arrow.Microsecond).UTC(), RequestID: str("request_id", i), SessionID: str("session_id", i),
				User: str("user", i), Source: str("source", i), Kind: str("kind", i), Action: str("action", i),
				Status: str("status", i), Rows: rows.Value(i), Table: str("table_name", i), Column: str("column_name", i),
				Tag: str("tag", i), Policy: str("policy", i), RowFilter: str("row_filter", i),
				SQLHash: str("sql_sha256", i), RewrittenHash: str("rewritten_sql_sha256", i), SQL: str("sql", i),
			})
		}
	}
	return out, nil
}
