package dq

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
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
	"github.com/prometheus/client_golang/prometheus"

	"github.com/bankmega/karhutlasql/internal/objstore"
)

// Prefix awalan key objek hasil validasi di arsip.
const Prefix = "dq/"

// RoleRunner role identitas runner GX (Q-02); hanya role ini yang boleh
// dq_publish, dan identitasnya tidak boleh menjalankan report atas hasilnya.
const RoleRunner = "dq_runner"

var (
	// ErrInvalid payload Validation Result tidak bisa di-parse/tidak lengkap.
	ErrInvalid = errors.New("invalid validation result")
	// ErrBlocked DQ gate menolak report run (Q-05).
	ErrBlocked = errors.New("dq gate")
)

// Validation satu hasil validasi GX tersanitasi (baris karhutla_dq_results()).
type Validation struct {
	ID         string `json:"validation_id"`
	Table      string `json:"table_name"`
	Suite      string `json:"suite"`
	RunID      string `json:"run_id"`
	SnapshotID string `json:"snapshot_id"`
	// Success nilai success GX (false juga bila hanya expectation warning gagal).
	Success        bool      `json:"success"`
	Evaluated      int64     `json:"evaluated"`
	FailedCritical int64     `json:"failed_critical"`
	FailedWarning  int64     `json:"failed_warning"`
	ValidatedAt    time.Time `json:"validated_at"` // waktu diterima server
	RunBy          string    `json:"run_by"`       // source:subject
	// Results JSON []Expectation (hanya field allowlist).
	Results string `json:"results"`
}

// Expectation hasil satu expectation setelah sanitasi.
type Expectation struct {
	Type     string         `json:"type"`
	Column   string         `json:"column,omitempty"`
	Severity string         `json:"severity"`
	Success  bool           `json:"success"`
	Result   map[string]any `json:"result,omitempty"`
}

// Severity expectation (Q-01): warning bila meta.severity = warning atau field
// severity bawaan GX = warning/info; selain itu critical (fail-closed).
const (
	Critical = "critical"
	Warning  = "warning"
)

// numericResult field `result` GX yang boleh disimpan (ADR 12): hitungan &
// persentase, tidak pernah nilai baris. Hanya diterima bila berupa angka.
var numericResult = []string{"element_count", "unexpected_count", "unexpected_percent", "unexpected_percent_total",
	"unexpected_percent_nonmissing", "missing_count", "missing_percent"}

var ident = regexp.MustCompile(`^[a-z][a-z0-9_]{0,127}$`)

// Sanitize mem-parse Validation Result JSON GX Core 1.x dan hanya menyimpan
// field allowlist: semua field pembawa nilai baris (partial_unexpected_*,
// unexpected_list, unexpected_index_*, traceback exception, kwargs selain
// column) terbuang, termasuk field baru di versi GX berikutnya. observed_value
// disimpan hanya bila angka/boolean dan kolomnya tidak berklasifikasi
// (tagged); min/max kolom PII tetap nilai baris.
func Sanitize(raw []byte, tagged func(column string) bool) (Validation, error) {
	var r struct {
		Success   *bool  `json:"success"`
		SuiteName string `json:"suite_name"`
		Meta      struct {
			RunID json.RawMessage `json:"run_id"`
		} `json:"meta"`
		Results []struct {
			Success *bool `json:"success"`
			Config  struct {
				Type     string         `json:"type"`
				Kwargs   map[string]any `json:"kwargs"`
				Meta     map[string]any `json:"meta"`
				Severity string         `json:"severity"` // bawaan GX ≥1.x: critical (default) | warning | info
			} `json:"expectation_config"`
			Result map[string]json.RawMessage `json:"result"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return Validation{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if r.Success == nil || len(r.Results) == 0 {
		return Validation{}, fmt.Errorf("%w: success and results are required", ErrInvalid)
	}
	v := Validation{Suite: clip(r.SuiteName), RunID: runID(r.Meta.RunID), Success: *r.Success, Evaluated: int64(len(r.Results))}
	exps := make([]Expectation, 0, len(r.Results))
	for i, x := range r.Results {
		if x.Success == nil || !ident.MatchString(x.Config.Type) {
			return Validation{}, fmt.Errorf("%w: results[%d]: success and expectation_config.type are required", ErrInvalid, i)
		}
		e := Expectation{Type: x.Config.Type, Severity: Critical, Success: *x.Success}
		if sev, _ := x.Config.Meta["severity"].(string); sev == Warning || x.Config.Severity == Warning || x.Config.Severity == "info" {
			e.Severity = Warning
		}
		e.Column, _ = x.Config.Kwargs["column"].(string)
		e.Column = clip(e.Column)
		for _, k := range numericResult {
			if f, ok := scalar[float64](x.Result[k]); ok {
				setResult(&e, k, f)
			}
		}
		if e.Column == "" || tagged == nil || !tagged(e.Column) {
			if f, ok := scalar[float64](x.Result["observed_value"]); ok {
				setResult(&e, "observed_value", f)
			} else if b, ok := scalar[bool](x.Result["observed_value"]); ok {
				setResult(&e, "observed_value", b)
			}
		}
		if !e.Success {
			if e.Severity == Warning {
				v.FailedWarning++
			} else {
				v.FailedCritical++
			}
		}
		exps = append(exps, e)
	}
	b, _ := json.Marshal(exps) // tipe sederhana: tidak pernah gagal
	v.Results = string(b)
	return v, nil
}

// scalar raw sebagai T bila JSON-nya memang T (bukan null/string/list).
func scalar[T float64 | bool](raw json.RawMessage) (T, bool) {
	var v T
	return v, len(raw) > 0 && string(raw) != "null" && json.Unmarshal(raw, &v) == nil
}

func setResult(e *Expectation, k string, v any) {
	if e.Result == nil {
		e.Result = map[string]any{}
	}
	e.Result[k] = v
}

// runID meta.run_id GX: objek {run_name, run_time} (checkpoint) atau string.
func runID(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return clip(s)
	}
	var o struct {
		RunName string `json:"run_name"`
		RunTime string `json:"run_time"`
	}
	if json.Unmarshal(raw, &o) == nil {
		return clip(strings.Trim(o.RunName+"@"+o.RunTime, "@"))
	}
	return ""
}

func clip(s string) string {
	if len(s) > 256 {
		return s[:256]
	}
	return s
}

// Store hasil validasi: Parquet append-only di arsip (satu objek per publish,
// partisi harian) + indeks di memori untuk gate & karhutla_dq_results().
// ponytail: seluruh riwayat dimuat saat start; batasi ke jendela waktu bila
// jumlah objek dq/ besar.
type Store struct {
	st  objstore.Store
	now func() time.Time

	mu  sync.RWMutex
	all []Validation // urut ValidatedAt
}

// Open memuat riwayat validasi dari st.
func Open(ctx context.Context, st objstore.Store) (*Store, error) {
	s := &Store{st: st, now: time.Now}
	keys, err := st.List(ctx, Prefix)
	if err != nil {
		return nil, fmt.Errorf("dq.Open: %w", err)
	}
	for _, k := range keys {
		if !strings.HasSuffix(k, ".parquet") {
			continue
		}
		b, err := st.Get(ctx, k)
		if err != nil {
			return nil, fmt.Errorf("dq.Open: %w", err)
		}
		vs, err := decode(ctx, b)
		if err != nil {
			return nil, fmt.Errorf("dq.Open: %s: %w", k, err)
		}
		s.all = append(s.all, vs...)
	}
	slices.SortStableFunc(s.all, func(a, b Validation) int { return a.ValidatedAt.Compare(b.ValidatedAt) })
	return s, nil
}

// Publish memberi ID & waktu, menyimpan v (append-only) lalu mengindeksnya.
func (s *Store) Publish(ctx context.Context, v Validation) (Validation, error) {
	v.ID, v.ValidatedAt = uuid.NewString(), s.now().UTC().Truncate(time.Microsecond)
	data, err := encode(v)
	if err != nil {
		return Validation{}, err
	}
	key := fmt.Sprintf("%sdt=%s/%s-%s.parquet", Prefix, v.ValidatedAt.Format(time.DateOnly), v.ValidatedAt.Format("20060102T150405.000000Z"), v.ID)
	if err := s.st.Put(ctx, key, data); err != nil {
		return Validation{}, fmt.Errorf("dq.Publish: %w", err)
	}
	s.mu.Lock()
	s.all = append(s.all, v)
	s.mu.Unlock()
	return v, nil
}

// All seluruh validasi, terlama dulu.
func (s *Store) All() []Validation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return slices.Clone(s.all)
}

// latest validasi terbaru per tabel.
func (s *Store) latest() map[string]Validation {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := map[string]Validation{}
	for _, v := range s.all {
		out[v.Table] = v
	}
	return out
}

// Gate (Q-05): untuk tiap tabel (→ snapshot_id saat ini), validasi terbarunya
// harus ada, pada snapshot_id yang sama, tanpa kegagalan critical, tidak lebih
// tua dari maxAge, dan tidak dijalankan oleh runner (pemisahan tugas). Hasil:
// ID validasi yang dipakai, urut nama tabel.
func (s *Store) Gate(snapshots map[string]string, maxAge time.Duration, runner string) ([]string, error) {
	latest := s.latest()
	tables := make([]string, 0, len(snapshots))
	for t := range snapshots {
		tables = append(tables, t)
	}
	slices.Sort(tables)
	var ids, problems []string
	for _, t := range tables {
		v, ok := latest[t]
		switch {
		case !ok:
			problems = append(problems, t+": no validation")
		case v.SnapshotID != snapshots[t]:
			problems = append(problems, fmt.Sprintf("%s: latest validation %s is for snapshot %s, data is at %s", t, v.ID, v.SnapshotID, snapshots[t]))
		case v.FailedCritical > 0:
			problems = append(problems, fmt.Sprintf("%s: validation %s has %d failed critical expectations", t, v.ID, v.FailedCritical))
		case s.now().Sub(v.ValidatedAt) > maxAge:
			problems = append(problems, fmt.Sprintf("%s: validation %s is older than %s", t, v.ID, maxAge))
		case v.RunBy == runner:
			problems = append(problems, fmt.Sprintf("%s: validation %s was run by the caller (separation of duties)", t, v.ID))
		default:
			ids = append(ids, v.ID)
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrBlocked, strings.Join(problems, "; "))
	}
	return ids, nil
}

var (
	failedDesc  = prometheus.NewDesc("karhutla_dq_expectations_failed", "Expectation gagal pada validasi terbaru per tabel.", []string{"table", "severity"}, nil)
	successDesc = prometheus.NewDesc("karhutla_dq_last_success_timestamp", "Waktu (unix) validasi terakhir tanpa kegagalan critical per tabel.", []string{"table"}, nil)
)

// Describe prometheus.Collector.
func (s *Store) Describe(ch chan<- *prometheus.Desc) {
	ch <- failedDesc
	ch <- successDesc
}

// Collect prometheus.Collector.
func (s *Store) Collect(ch chan<- prometheus.Metric) {
	for t, v := range s.latest() {
		ch <- prometheus.MustNewConstMetric(failedDesc, prometheus.GaugeValue, float64(v.FailedCritical), t, Critical)
		ch <- prometheus.MustNewConstMetric(failedDesc, prometheus.GaugeValue, float64(v.FailedWarning), t, Warning)
	}
	s.mu.RLock()
	last := map[string]time.Time{}
	for _, v := range s.all {
		if v.FailedCritical == 0 {
			last[v.Table] = v.ValidatedAt
		}
	}
	s.mu.RUnlock()
	for t, ts := range last {
		ch <- prometheus.MustNewConstMetric(successDesc, prometheus.GaugeValue, float64(ts.Unix()), t)
	}
}

// --- Parquet (kolom = tag JSON Validation) ---

var schema = arrow.NewSchema([]arrow.Field{
	{Name: "validation_id", Type: arrow.BinaryTypes.String},
	{Name: "table_name", Type: arrow.BinaryTypes.String},
	{Name: "suite", Type: arrow.BinaryTypes.String},
	{Name: "run_id", Type: arrow.BinaryTypes.String},
	{Name: "snapshot_id", Type: arrow.BinaryTypes.String},
	{Name: "success", Type: arrow.FixedWidthTypes.Boolean},
	{Name: "evaluated", Type: arrow.PrimitiveTypes.Int64},
	{Name: "failed_critical", Type: arrow.PrimitiveTypes.Int64},
	{Name: "failed_warning", Type: arrow.PrimitiveTypes.Int64},
	{Name: "validated_at", Type: &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"}},
	{Name: "run_by", Type: arrow.BinaryTypes.String},
	{Name: "results", Type: arrow.BinaryTypes.String},
}, nil)

func encode(vs ...Validation) ([]byte, error) {
	js, _ := json.Marshal(vs) // tipe sederhana: tidak pernah gagal
	rec, _, err := array.RecordFromJSON(memory.DefaultAllocator, schema, bytes.NewReader(js))
	if err != nil {
		return nil, fmt.Errorf("dq.encode: %w", err)
	}
	defer rec.Release()
	var out bytes.Buffer
	w, err := pqarrow.NewFileWriter(schema, &out,
		parquet.NewWriterProperties(parquet.WithCompression(compress.Codecs.Zstd)), pqarrow.DefaultWriterProps())
	if err != nil {
		return nil, fmt.Errorf("dq.encode: %w", err)
	}
	if err := errors.Join(w.Write(rec), w.Close()); err != nil {
		return nil, fmt.Errorf("dq.encode: %w", err)
	}
	return out.Bytes(), nil
}

func decode(ctx context.Context, data []byte) ([]Validation, error) {
	tbl, err := pqarrow.ReadTable(ctx, bytes.NewReader(data), nil, pqarrow.ArrowReadProperties{}, memory.DefaultAllocator)
	if err != nil {
		return nil, err
	}
	defer tbl.Release()
	tr := array.NewTableReader(tbl, 0)
	defer tr.Release()
	var out []Validation
	for tr.Next() {
		var js bytes.Buffer
		if err := array.RecordToJSON(tr.RecordBatch(), &js); err != nil {
			return nil, err
		}
		dec := json.NewDecoder(&js)
		for dec.More() {
			var v Validation
			if err := dec.Decode(&v); err != nil {
				return nil, err
			}
			out = append(out, v)
		}
	}
	return out, nil
}
