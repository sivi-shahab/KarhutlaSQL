package flightsql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/flight"
	fsql "github.com/apache/arrow-go/v18/arrow/flight/flightsql"
	"github.com/apache/arrow-go/v18/arrow/flight/flightsql/schema_ref"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bankmega/karhutlasql/internal/accesslog"
	"github.com/bankmega/karhutlasql/internal/audit"
	"github.com/bankmega/karhutlasql/internal/auth"
	"github.com/bankmega/karhutlasql/internal/authz"
	"github.com/bankmega/karhutlasql/internal/cache"
	"github.com/bankmega/karhutlasql/internal/classification"
	"github.com/bankmega/karhutlasql/internal/dq"
	"github.com/bankmega/karhutlasql/internal/engine"
	"github.com/bankmega/karhutlasql/internal/metrics"
	"github.com/bankmega/karhutlasql/internal/objstore"
	"github.com/bankmega/karhutlasql/internal/quota"
	"github.com/bankmega/karhutlasql/internal/report"
	"github.com/bankmega/karhutlasql/internal/scheduler"
	"github.com/bankmega/karhutlasql/internal/session"
	"github.com/bankmega/karhutlasql/internal/sqlext"
)

// tracer span statement (no-op bila OpenTelemetry tidak dikonfigurasi).
// Atribut hanya hash SQL, tidak pernah teks SQL atau nilai data.
var tracer = otel.Tracer("github.com/bankmega/karhutlasql/internal/flightsql")

// ticketTTL batas umur statement handle yang belum di-DoGet.
const ticketTTL = 10 * time.Minute

// Options perilaku eksekusi statement.
type Options struct {
	// QueryTimeout batas eksekusi per statement; 0 = tanpa batas.
	QueryTimeout time.Duration
	// PrintQueries mencatat teks SQL ke log (bukan audit).
	PrintQueries bool
	Audit        *audit.Logger
	Metrics      *metrics.Metrics // nil = tanpa metrics
	Policy       *authz.Store     // wajib
	// Classification tag kolom & mask (G-03); nil = tanpa mask.
	Classification *classification.Store
	// Scheduler antrean statement per resource group (G-08); nil = baru.
	Scheduler *scheduler.Scheduler
	// DefaultGroup batas grup scheduler.Default bila policy tidak mendefinisikannya.
	DefaultGroup scheduler.Group
	// History riwayat statement (G-09); nil = tidak dicatat.
	History *session.History
	// AccessLog access-history per kolom (G-05); nil = tidak dicatat.
	AccessLog *accesslog.Log
	// Report run (G-10): definisi, kunci tanda tangan, arsip hasil.
	Reports map[string]report.Def
	Signer  *report.Signer
	Archive objstore.Store
	// DQ hasil validasi GX (Q-03..Q-05); nil = dq_publish ditolak & tanpa gate.
	DQ *dq.Store
	// MetadataChanged dipanggil setelah DDL sukses & dq_publish (pemicu
	// sinkron Unity Catalog, K-01); tidak boleh memblokir.
	MetadataChanged func()
	// DQMaxAge umur maksimum validasi untuk DQ gate report; 0 = gate mati.
	DQMaxAge time.Duration
	// Profiling isi peak memory & plan hash history dari profiling DuckDB
	// (+~1–5 ms per statement, ADR 26).
	Profiling bool
	// SlowQuery durasi eksekusi (di luar antre) yang dicatat ke log slow_query; 0 = mati.
	SlowQuery time.Duration
	// Cache result cache SELECT (ADR 36); nil = mati.
	Cache *cache.Cache
	// Quota kuota & chargeback per tim (policy quotas); nil = tidak dihitung.
	Quota *quota.Tracker
}

// Server mengimplementasikan flightsql.Server di atas engine.Engine.
type Server struct {
	fsql.BaseServer
	eng  engine.Engine
	log  *slog.Logger
	opts Options
	// writes jumlah statement non-SELECT yang dieksekusi: versi data catalog
	// DuckDB lokal untuk kunci result cache.
	writes atomic.Int64
}

// stmtHandle SQL statement yang menunggu DoGet (disimpan di sesi pemilik).
type stmtHandle string

type prepared struct {
	sql  string
	info engine.StatementInfo

	mu     sync.Mutex
	params []any // baris parameter terakhir yang di-bind (query)
}

// New membuat Server. opts.Audit wajib.
func New(eng engine.Engine, log *slog.Logger, opts Options) (*Server, error) {
	if opts.Audit == nil || opts.Policy == nil {
		return nil, errors.New("flightsql.New: audit logger and policy are required")
	}
	if opts.Classification == nil {
		opts.Classification, _ = classification.Load("") // path kosong: tidak pernah error
	}
	if opts.Scheduler == nil {
		opts.Scheduler = scheduler.New()
	}
	s := &Server{eng: eng, log: log, opts: opts}
	s.Alloc = memory.DefaultAllocator
	for id, v := range sqlInfo(eng, opts.QueryTimeout) {
		if err := s.RegisterSqlInfo(id, v); err != nil {
			return nil, fmt.Errorf("flightsql.New: %w", err)
		}
	}
	return s, nil
}

// --- statement ---

// sess sesi pemanggil; tidak ada = interceptor sesi tidak terpasang (fail-closed).
func sess(ctx context.Context) (*session.Session, error) {
	if ss := session.FromContext(ctx); ss != nil {
		return ss, nil
	}
	return nil, status.Error(codes.Unauthenticated, "no session")
}

func (s *Server) GetFlightInfoStatement(ctx context.Context, cmd fsql.StatementQuery, desc *flight.FlightDescriptor) (*flight.FlightInfo, error) {
	if len(cmd.GetTransactionId()) > 0 {
		return nil, status.Error(codes.Unimplemented, "transactions are not supported")
	}
	ss, err := sess(ctx)
	if err != nil {
		return nil, err
	}
	handle := uuid.NewString()
	ss.Put(handle, stmtHandle(cmd.GetQuery()))
	time.AfterFunc(ticketTTL, func() { ss.Delete(handle) })

	tkt, err := fsql.CreateStatementQueryTicket([]byte(handle))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "flightsql.GetFlightInfoStatement: %v", err)
	}
	return flightInfo(desc, tkt), nil
}

func (s *Server) DoGetStatement(ctx context.Context, tkt fsql.StatementQueryTicket) (*arrow.Schema, <-chan flight.StreamChunk, error) {
	ss, err := sess(ctx)
	if err != nil {
		return nil, nil, err
	}
	v, _ := ss.Take(string(tkt.GetStatementHandle()))
	q, ok := v.(stmtHandle)
	if !ok {
		return nil, nil, status.Error(codes.NotFound, "unknown or expired statement handle")
	}
	return s.query(ctx, string(q), nil)
}

func (s *Server) DoPutCommandStatementUpdate(ctx context.Context, cmd fsql.StatementUpdate) (int64, error) {
	if len(cmd.GetTransactionId()) > 0 {
		return 0, status.Error(codes.Unimplemented, "transactions are not supported")
	}
	return s.update(ctx, cmd.GetQuery(), [][]any{nil})
}

// --- prepared statement ---

func (s *Server) CreatePreparedStatement(ctx context.Context, req fsql.ActionCreatePreparedStatementRequest) (fsql.ActionCreatePreparedStatementResult, error) {
	if len(req.GetTransactionId()) > 0 {
		return fsql.ActionCreatePreparedStatementResult{}, status.Error(codes.Unimplemented, "transactions are not supported")
	}
	ss, err := sess(ctx)
	if err != nil {
		return fsql.ActionCreatePreparedStatementResult{}, err
	}
	pl, err := s.authorize(ctx, req.GetQuery())
	if err != nil {
		return fsql.ActionCreatePreparedStatementResult{}, err
	}
	// Schema dari SQL hasil rewrite (kolom ter-mask); eksekusi meng-otorisasi ulang.
	info, err := s.eng.Describe(sqlext.WithMaskNonce(ctx, pl.nonce), pl.sql)
	if err != nil {
		return fsql.ActionCreatePreparedStatementResult{}, toStatus(ctx, err)
	}
	handle := uuid.NewString()
	ss.Put(handle, &prepared{sql: req.GetQuery(), info: info})
	// DatasetSchema wajib bagi driver JDBC: tanpa itu statement dianggap update.
	isUpdate := !info.ReturnsRows
	return fsql.ActionCreatePreparedStatementResult{Handle: []byte(handle), DatasetSchema: info.Schema, ParameterSchema: info.ParamSchema, IsUpdate: &isUpdate}, nil
}

func (s *Server) ClosePreparedStatement(ctx context.Context, req fsql.ActionClosePreparedStatementRequest) error {
	ss, err := sess(ctx)
	if err != nil {
		return err
	}
	ss.Delete(string(req.GetPreparedStatementHandle()))
	return nil
}

// getPrepared mengambil prepared statement milik sesi pemanggil.
func getPrepared(ctx context.Context, handle []byte) (*prepared, error) {
	ss, err := sess(ctx)
	if err != nil {
		return nil, err
	}
	v, _ := ss.Get(string(handle))
	p, ok := v.(*prepared)
	if !ok {
		return nil, status.Error(codes.NotFound, "unknown prepared statement handle")
	}
	return p, nil
}

func (s *Server) DoPutPreparedStatementQuery(ctx context.Context, cmd fsql.PreparedStatementQuery, rdr flight.MessageReader, _ flight.MetadataWriter) ([]byte, error) {
	p, err := getPrepared(ctx, cmd.GetPreparedStatementHandle())
	if err != nil {
		return nil, err
	}
	rows, err := paramRows(rdr)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "bind parameters: %v", err)
	}
	if len(rows) > 1 {
		return nil, status.Error(codes.InvalidArgument, "query prepared statement accepts exactly one parameter row")
	}
	p.mu.Lock()
	p.params = nil
	if len(rows) == 1 {
		p.params = rows[0]
	}
	p.mu.Unlock()
	return cmd.GetPreparedStatementHandle(), nil
}

// PollFlightInfoStatement Flight PollFlightInfo (eksekusi inkremental ADBC):
// eksekusi KarhutlaSQL terjadi di DoGet dengan hasil di-stream, jadi poll
// pertama langsung selesai (progress 1, tanpa descriptor lanjutan) dan
// tiketnya sama dengan GetFlightInfo.
func (s *Server) PollFlightInfoStatement(ctx context.Context, cmd fsql.StatementQuery, desc *flight.FlightDescriptor) (*flight.PollInfo, error) {
	info, err := s.GetFlightInfoStatement(ctx, cmd, desc)
	return done(info), err
}

// PollFlightInfoPreparedStatement lihat PollFlightInfoStatement.
func (s *Server) PollFlightInfoPreparedStatement(ctx context.Context, cmd fsql.PreparedStatementQuery, desc *flight.FlightDescriptor) (*flight.PollInfo, error) {
	info, err := s.GetFlightInfoPreparedStatement(ctx, cmd, desc)
	return done(info), err
}

func done(info *flight.FlightInfo) *flight.PollInfo {
	if info == nil {
		return nil
	}
	p := 1.0
	return &flight.PollInfo{Info: info, Progress: &p}
}

func (s *Server) GetFlightInfoPreparedStatement(ctx context.Context, cmd fsql.PreparedStatementQuery, desc *flight.FlightDescriptor) (*flight.FlightInfo, error) {
	if _, err := getPrepared(ctx, cmd.GetPreparedStatementHandle()); err != nil {
		return nil, err
	}
	return flightInfo(desc, desc.Cmd), nil
}

func (s *Server) DoGetPreparedStatement(ctx context.Context, cmd fsql.PreparedStatementQuery) (*arrow.Schema, <-chan flight.StreamChunk, error) {
	p, err := getPrepared(ctx, cmd.GetPreparedStatementHandle())
	if err != nil {
		return nil, nil, err
	}
	p.mu.Lock()
	params := p.params
	p.mu.Unlock()
	return s.query(ctx, p.sql, params)
}

func (s *Server) DoPutPreparedStatementUpdate(ctx context.Context, cmd fsql.PreparedStatementUpdate, rdr flight.MessageReader) (int64, error) {
	p, err := getPrepared(ctx, cmd.GetPreparedStatementHandle())
	if err != nil {
		return 0, err
	}
	rows, err := paramRows(rdr)
	if err != nil {
		return 0, status.Errorf(codes.InvalidArgument, "bind parameters: %v", err)
	}
	if len(rows) == 0 {
		rows = [][]any{nil}
	}
	return s.update(ctx, p.sql, rows)
}

// --- eksekusi + audit ---

// begin mencatat statement_start, mendaftarkan statement di sesi (kill
// membatalkannya), menegakkan policy (gagal → statement_end + error), menunggu
// slot resource group, dan mengembalikan ctx eksekusi (timeout
// server/role/grup, nonce mask, sink statistik), plan (SQL yang dieksekusi)
// serta end yang wajib dipanggil tepat sekali bila err nil.
// ddlKinds jenis statement yang mengubah metadata katalog.
var ddlKinds = []string{"CREATE", "DROP", "ALTER", "ATTACH", "DETACH"}

func (s *Server) begin(ctx context.Context, kind, sql string) (context.Context, plan, func(rows, bytes int64, err error), error) {
	return s.beginWith(ctx, kind, sql, func(ctx context.Context) (plan, error) { return s.authorize(ctx, sql) })
}

// beginWith seperti begin dengan otorisasi az (mis. bulk ingest tanpa SQL).
func (s *Server) beginWith(ctx context.Context, kind, sql string, az func(context.Context) (plan, error)) (context.Context, plan, func(rows, bytes int64, err error), error) {
	ss, err := sess(ctx)
	if err != nil {
		return nil, plan{}, nil, err
	}
	id, _ := auth.FromContext(ctx)
	ev := audit.Event{User: id.Subject, Source: id.Source, Kind: kind, Channel: Channel(ctx)}
	s.opts.Audit.SQL(&ev, sql)
	start := ev
	start.Type = audit.StatementStart
	s.opts.Audit.Emit(ctx, start)
	if s.opts.PrintQueries {
		s.log.Info("statement", "request_id", audit.RequestID(ctx), "user", id.Subject, "kind", kind, "sql", sql)
	}

	qctx, done := ss.Begin(ctx, sql)
	qctx, span := tracer.Start(qctx, "statement", trace.WithAttributes(attribute.String("karhutla.kind", kind),
		attribute.String("karhutla.request_id", audit.RequestID(ctx)), attribute.String("enduser.id", id.Subject),
		attribute.String("karhutla.sql_sha256", ev.SQLHash)))
	cancel, release := context.CancelFunc(func() {}), func() {}
	t0 := time.Now()
	hq := session.Query{RequestID: audit.RequestID(ctx), SessionID: ss.ID, User: id.Subject, Kind: kind, SQL: s.opts.Audit.Redact(sql), StartedAt: t0}
	var st engine.Stats
	var tExec time.Time
	var pl plan
	end := func(rows, bytes int64, err error) {
		cancel()
		release()
		done()
		ss.Rows.Add(rows)
		ss.Bytes.Add(bytes)
		fin := ev
		fin.Type, fin.Rows, fin.DurationMS = audit.StatementEnd, rows, time.Since(t0).Milliseconds()
		if pl.hit != nil && *pl.hit {
			fin.Detail = "result_cache_hit"
		}
		if !tExec.IsZero() && pl.final.Kind != "SELECT" {
			s.writes.Add(1) // sebelum/ sesudah sukses: invalidasi konservatif
		}
		if s.opts.Quota != nil && !tExec.IsZero() && !pl.control {
			s.opts.Quota.Add(id, quota.Usage{Queries: 1, ExecSeconds: time.Since(tExec).Seconds(), Rows: rows, Bytes: bytes})
		}
		switch {
		case err != nil:
			fin.Status, fin.Error = status.Code(err).String(), status.Convert(err).Message()
		case ctx.Err() != nil:
			fin.Status = codes.Canceled.String() // klien memutus stream
		default:
			fin.Status = codes.OK.String()
		}
		s.opts.Audit.Emit(ctx, fin)
		span.SetAttributes(attribute.String("karhutla.status", fin.Status), attribute.Int64("karhutla.rows", rows),
			attribute.Int64("karhutla.bytes", bytes), attribute.String("karhutla.resource_group", pl.limits.Group),
			attribute.Bool("karhutla.result_cache_hit", fin.Detail == "result_cache_hit"))
		if fin.Status != codes.OK.String() {
			span.SetStatus(otelcodes.Error, fin.Status)
		}
		span.End()
		s.opts.Metrics.Statement(id.Subject, fin.Status, time.Since(t0))
		if kind == "query" {
			s.opts.Metrics.Sent(id.Subject, rows, bytes)
		}
		hq.Status, hq.Error, hq.Rows, hq.Bytes, hq.PeakMemory, hq.PlanHash = fin.Status, fin.Error, rows, bytes, st.PeakMemory, st.PlanHash
		if !tExec.IsZero() {
			hq.Duration = time.Since(tExec)
		}
		s.opts.History.Add(hq)
		if s.opts.MetadataChanged != nil && fin.Status == codes.OK.String() && slices.Contains(ddlKinds, pl.final.Kind) {
			s.opts.MetadataChanged()
		}
		if s.opts.AccessLog != nil && !tExec.IsZero() {
			base := accesslog.Record{Time: t0, RequestID: hq.RequestID, SessionID: ss.ID, User: id.Subject, Source: id.Source,
				Kind: kind, Status: fin.Status, Rows: rows, SQLHash: ev.SQLHash, SQL: ev.SQL}
			if pl.sql != sql {
				sum := sha256.Sum256([]byte(pl.sql))
				base.RewrittenHash = hex.EncodeToString(sum[:])
			}
			s.opts.AccessLog.Append(pl.access(base)...)
		}
		if s.opts.SlowQuery > 0 && hq.Duration >= s.opts.SlowQuery {
			s.log.Warn("slow_query", "request_id", hq.RequestID, "session_id", hq.SessionID, "user", hq.User, "resource_group", hq.Group,
				"status", hq.Status, "queue_ms", hq.QueueWait.Milliseconds(), "duration_ms", hq.Duration.Milliseconds(),
				"rows", rows, "bytes", bytes, "peak_memory", st.PeakMemory, "plan_hash", st.PlanHash, "sql", hq.SQL)
		}
	}
	if s.opts.AccessLog != nil {
		if err := s.opts.AccessLog.Healthy(); err != nil { // fail-closed: akses harus tercatat
			s.log.Error("access history unavailable", "err", err)
			err = status.Error(codes.Unavailable, "access history unavailable; statements are refused until it recovers")
			end(0, 0, err)
			return nil, plan{}, nil, err
		}
	}
	actx, aspan := tracer.Start(qctx, "authorize")
	pl, err = az(actx)
	aspan.End()
	if err != nil {
		end(0, 0, err)
		return nil, plan{}, nil, err
	}
	pl.hit = new(bool)
	if s.opts.Quota != nil && !pl.control { // kontrol admin (kill) tidak terkena kuota
		if err := s.opts.Quota.Check(id); err != nil {
			err = status.Error(codes.ResourceExhausted, err.Error())
			end(0, 0, err)
			return nil, plan{}, nil, err
		}
	}
	hq.Group = pl.limits.Group
	g, ok := s.opts.Policy.Policy().ResourceGroups[pl.limits.Group]
	if !ok {
		g = s.opts.DefaultGroup
	}
	rel, wait, err := func() (func(), time.Duration, error) {
		if pl.control {
			return func() {}, 0, nil
		}
		sctx, sspan := tracer.Start(qctx, "queue", trace.WithAttributes(attribute.String("karhutla.resource_group", pl.limits.Group)))
		defer sspan.End()
		return s.opts.Scheduler.Acquire(sctx, pl.limits.Group, g)
	}()
	hq.QueueWait = wait
	if !pl.control {
		s.opts.Metrics.QueueWait(pl.limits.Group, wait)
	}
	if err != nil {
		if errors.Is(err, scheduler.ErrQueueFull) || errors.Is(err, scheduler.ErrQueueTimeout) {
			err = status.Error(codes.ResourceExhausted, err.Error())
		} else {
			err = toStatus(qctx, err)
		}
		end(0, 0, err)
		return nil, plan{}, nil, err
	}
	release, tExec = rel, time.Now()
	timeout := s.opts.QueryTimeout
	for _, t := range []time.Duration{pl.limits.Timeout, g.Timeout} {
		if t > 0 && (timeout == 0 || t < timeout) {
			timeout = t
		}
	}
	if timeout > 0 {
		tctx, tcancel := context.WithTimeout(qctx, timeout)
		qctx, cancel = tctx, tcancel // dipanggil oleh end
	}
	if pl.nonce != "" {
		qctx = sqlext.WithMaskNonce(qctx, pl.nonce)
	}
	if s.opts.Profiling {
		qctx = engine.WithStats(qctx, &st)
	}
	return qctx, pl, end, nil
}

// query mengeksekusi SELECT dan mengalirkan hasilnya. Timeout berlaku untuk
// engine; stream memakai ctx RPC agar timeout muncul sebagai error ke klien,
// bukan stream yang terpotong diam-diam.
func (s *Server) query(ctx context.Context, sql string, params []any) (*arrow.Schema, <-chan flight.StreamChunk, error) {
	qctx, pl, end, err := s.begin(ctx, "query", sql)
	if err != nil {
		return nil, nil, err
	}
	key := s.cacheKey(qctx, pl, params)
	var src array.RecordReader
	if key != "" {
		if r, ok := s.opts.Cache.Get(key); ok {
			*pl.hit, src = true, r
		}
	}
	if src == nil {
		rdr, err := s.eng.Query(qctx, pl.sql, params...)
		if err != nil {
			err = toStatus(qctx, err)
			end(0, 0, err)
			return nil, nil, err
		}
		src = &batcher{RecordReader: rdr}
		if key != "" {
			src = s.opts.Cache.Writer(key, src)
		}
	}
	ar := &auditedReader{RecordReader: src, qctx: qctx, end: end, maxBytes: pl.limits.MaxBytes}
	ch := make(chan flight.StreamChunk)
	go flight.StreamChunksFromReader(ctx, ar, ch)
	return src.Schema(), ch, nil
}

// pureFunctions table function yang hasilnya hanya bergantung pada argumen.
var pureFunctions = []string{"RANGE", "GENERATE_SERIES", "UNNEST"}

// cacheKey kunci result cache untuk pl, atau "" bila tidak boleh di-cache
// (ADR 36): hanya SELECT tanpa table function dinamis, tanpa fungsi volatil
// (termasuk di dalam view), atas catalog yang versinya diketahui. Kunci =
// identitas + SQL yang dieksekusi setelah governance (mask/RLS/LIMIT dengan
// nilai claim, nonce dinormalisasi) + parameter + versi data, sehingga
// perubahan policy/classification/claim/data otomatis meleset.
// ponytail: hit tetap melewati antrean resource group (slot dilepas segera).
func (s *Server) cacheKey(ctx context.Context, pl plan, params []any) string {
	if s.opts.Cache == nil || pl.final.Kind != "SELECT" || pl.control {
		return ""
	}
	for _, f := range pl.final.Functions {
		if !slices.Contains(pureFunctions, f) {
			return ""
		}
	}
	ver, ok, err := s.eng.DataVersion(ctx, pl.final.Catalogs)
	if err != nil || !ok {
		return ""
	}
	if det, err := s.eng.Deterministic(ctx, pl.sql); err != nil || !det {
		return ""
	}
	id, _ := auth.FromContext(ctx)
	sql := pl.sql
	if pl.nonce != "" {
		sql = strings.ReplaceAll(sql, pl.nonce, "")
	}
	parts := []string{id.Source, id.Subject, sql, ver, strconv.FormatInt(s.writes.Load(), 10)}
	for _, p := range params {
		parts = append(parts, fmt.Sprintf("%T:%v", p, p))
	}
	return cache.Key(parts...)
}

// update mengeksekusi statement untuk setiap baris parameter.
// ponytail: satu Exec per baris, tanpa transaksi; bulk ingest di Fase 8.
func (s *Server) update(ctx context.Context, sql string, rows [][]any) (int64, error) {
	qctx, pl, end, err := s.begin(ctx, "update", sql)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, args := range rows {
		n, err := s.eng.Exec(qctx, pl.sql, args...)
		if err != nil {
			err = toStatus(qctx, err)
			end(total, 0, err)
			return total, err
		}
		total += n
	}
	end(total, 0, nil)
	return total, nil
}

// auditedReader menghitung baris, memetakan error ke status gRPC, dan
// memanggil end saat dilepas (StreamChunksFromReader me-Release sekali).
type auditedReader struct {
	array.RecordReader
	qctx     context.Context
	rows     int64
	bytes    int64
	maxBytes int64 // batas byte role (G-06); 0 = tanpa batas
	capErr   error
	end      func(rows, bytes int64, err error)
	once     sync.Once
}

func (r *auditedReader) Next() bool {
	if r.capErr != nil || !r.RecordReader.Next() {
		return false
	}
	rec := r.RecordBatch()
	var n int64
	for _, c := range rec.Columns() {
		n += dataSize(c.Data())
	}
	if r.maxBytes > 0 && r.bytes+n > r.maxBytes {
		r.capErr = status.Errorf(codes.ResourceExhausted, "result exceeds %d bytes allowed for this role", r.maxBytes)
		return false
	}
	r.rows += rec.NumRows()
	r.bytes += n
	return true
}

// dataSize total byte buffer Arrow (termasuk child), perkiraan byte terkirim.
func dataSize(d arrow.ArrayData) int64 {
	var n int64
	for _, b := range d.Buffers() {
		if b != nil {
			n += int64(b.Len())
		}
	}
	for _, c := range d.Children() {
		n += dataSize(c)
	}
	return n
}

func (r *auditedReader) Err() error {
	if r.capErr != nil {
		return r.capErr
	}
	if err := r.RecordReader.Err(); err != nil {
		return toStatus(r.qctx, err)
	}
	return nil
}

func (r *auditedReader) Release() {
	err := r.Err()
	r.RecordReader.Release()
	r.once.Do(func() { r.end(r.rows, r.bytes, err) })
}

// batchRows target baris per batch Arrow yang dikirim (spec §6: 64k).
const batchRows = 64 * 1024

// batcher menggabungkan record kecil dari engine (DuckDB: 2048 baris per
// vektor) menjadi batch ±batchRows baris: jauh lebih sedikit pesan gRPC dan
// overhead per batch di klien, dengan satu salinan tambahan.
type batcher struct {
	array.RecordReader
	cur arrow.RecordBatch
	err error
}

func (b *batcher) Next() bool {
	if b.cur != nil {
		b.cur.Release()
		b.cur = nil
	}
	var recs []arrow.RecordBatch
	defer func() {
		for _, r := range recs {
			r.Release()
		}
	}()
	var n int64
	for n < batchRows && b.RecordReader.Next() {
		r := b.RecordReader.RecordBatch()
		r.Retain()
		recs = append(recs, r)
		n += r.NumRows()
	}
	switch len(recs) {
	case 0:
		return false
	case 1:
		b.cur, recs = recs[0], nil
		return true
	}
	cols := make([]arrow.Array, len(recs[0].Columns()))
	defer func() {
		for _, c := range cols {
			if c != nil {
				c.Release()
			}
		}
	}()
	for i := range cols {
		parts := make([]arrow.Array, len(recs))
		for j, r := range recs {
			parts[j] = r.Column(i)
		}
		c, err := array.Concatenate(parts, memory.DefaultAllocator)
		if err != nil {
			b.err = fmt.Errorf("flightsql.batcher: %w", err)
			return false
		}
		cols[i] = c
	}
	b.cur = array.NewRecordBatch(b.Schema(), cols, n)
	return true
}

func (b *batcher) RecordBatch() arrow.RecordBatch { return b.cur }

//nolint:staticcheck // Record wajib ada di array.RecordReader (deprecated alias)
func (b *batcher) Record() arrow.RecordBatch { return b.cur }

func (b *batcher) Err() error {
	if b.err != nil {
		return b.err
	}
	return b.RecordReader.Err()
}

func (b *batcher) Release() {
	if b.cur != nil {
		b.cur.Release()
		b.cur = nil
	}
	b.RecordReader.Release()
}

// --- metadata ---

func (s *Server) GetFlightInfoCatalogs(_ context.Context, desc *flight.FlightDescriptor) (*flight.FlightInfo, error) {
	return flightInfo(desc, desc.Cmd), nil
}

func (s *Server) DoGetCatalogs(ctx context.Context) (*arrow.Schema, <-chan flight.StreamChunk, error) {
	return s.metadata(ctx, schema_ref.Catalogs, func() (array.RecordReader, error) { return s.eng.Catalogs(ctx) }, 1)
}

func (s *Server) GetFlightInfoSchemas(_ context.Context, _ fsql.GetDBSchemas, desc *flight.FlightDescriptor) (*flight.FlightInfo, error) {
	return flightInfo(desc, desc.Cmd), nil
}

func (s *Server) DoGetDBSchemas(ctx context.Context, cmd fsql.GetDBSchemas) (*arrow.Schema, <-chan flight.StreamChunk, error) {
	return s.metadata(ctx, schema_ref.DBSchemas, func() (array.RecordReader, error) {
		return s.eng.Schemas(ctx, cmd.GetCatalog(), cmd.GetDBSchemaFilterPattern())
	}, 2)
}

func (s *Server) GetFlightInfoTables(_ context.Context, _ fsql.GetTables, desc *flight.FlightDescriptor) (*flight.FlightInfo, error) {
	return flightInfo(desc, desc.Cmd), nil
}

func (s *Server) DoGetTables(ctx context.Context, cmd fsql.GetTables) (*arrow.Schema, <-chan flight.StreamChunk, error) {
	query := func() (array.RecordReader, error) {
		return s.eng.Tables(ctx, engine.TableFilter{
			Catalog:       cmd.GetCatalog(),
			SchemaPattern: cmd.GetDBSchemaFilterPattern(),
			TablePattern:  cmd.GetTableNameFilterPattern(),
			TableTypes:    cmd.GetTableTypes(),
		})
	}
	if !cmd.GetIncludeSchema() {
		return s.metadata(ctx, schema_ref.Tables, query, 3)
	}
	return s.tablesWithSchema(ctx, query)
}

func (s *Server) GetFlightInfoTableTypes(_ context.Context, desc *flight.FlightDescriptor) (*flight.FlightInfo, error) {
	return flightInfo(desc, desc.Cmd), nil
}

func (s *Server) DoGetTableTypes(ctx context.Context) (*arrow.Schema, <-chan flight.StreamChunk, error) {
	return s.metadata(ctx, schema_ref.TableTypes, func() (array.RecordReader, error) { return s.eng.TableTypes(ctx) }, 0)
}

// tablesWithSchema menambahkan kolom table_schema (schema IPC per tabel).
func (s *Server) tablesWithSchema(ctx context.Context, query func() (array.RecordReader, error)) (*arrow.Schema, <-chan flight.StreamChunk, error) {
	all, err := collect(query)
	if err != nil {
		return nil, nil, toStatus(ctx, err)
	}
	recs := s.filterObjects(ctx, all, 3)
	releaseAll(all)
	defer releaseAll(recs)
	out := make([]arrow.RecordBatch, 0, len(recs))
	defer func() { releaseAll(out) }()
	for _, rec := range recs {
		cat, sch, tbl := rec.Column(0).(*array.String), rec.Column(1).(*array.String), rec.Column(2).(*array.String)
		b := array.NewBinaryBuilder(s.Alloc, arrow.BinaryTypes.Binary)
		for i := 0; i < int(rec.NumRows()); i++ {
			ts, err := s.eng.TableSchema(ctx, cat.Value(i), sch.Value(i), tbl.Value(i))
			if err != nil {
				b.Release()
				return nil, nil, toStatus(ctx, err)
			}
			b.Append(flight.SerializeSchema(ts, s.Alloc))
		}
		col := b.NewArray()
		b.Release()
		cols := append(append([]arrow.Array{}, rec.Columns()...), col)
		out = append(out, array.NewRecordBatch(schema_ref.TablesWithIncludedSchema, cols, rec.NumRows()))
		col.Release()
	}
	rdr, err := array.NewRecordReader(schema_ref.TablesWithIncludedSchema, out)
	if err != nil {
		return nil, nil, status.Errorf(codes.Internal, "flightsql.DoGetTables: %v", err)
	}
	return s.stream(ctx, func() (array.RecordReader, error) { return rdr, nil })
}

// metadata menjalankan query metadata engine lalu memberi label schema protokol
// (urutan & tipe kolom dijamin kontrak engine.Engine).
// depth > 0: baris yang objeknya (depth kolom pertama: catalog[.schema[.table]])
// tidak boleh dilihat pemanggil dibuang (E-03/G-02).
func (s *Server) metadata(ctx context.Context, sch *arrow.Schema, query func() (array.RecordReader, error), depth int) (*arrow.Schema, <-chan flight.StreamChunk, error) {
	recs, err := collect(query)
	if err != nil {
		return nil, nil, toStatus(ctx, err)
	}
	if depth > 0 {
		all := recs
		recs = s.filterObjects(ctx, all, depth)
		releaseAll(all)
	}
	defer releaseAll(recs)
	out := make([]arrow.RecordBatch, len(recs))
	for i, rec := range recs {
		out[i] = array.NewRecordBatch(sch, rec.Columns(), rec.NumRows())
	}
	defer releaseAll(out)
	rdr, err := array.NewRecordReader(sch, out)
	if err != nil {
		return nil, nil, status.Errorf(codes.Internal, "flightsql.metadata: %v", err)
	}
	return s.stream(ctx, func() (array.RecordReader, error) { return rdr, nil })
}

// collect menguras reader menjadi batch yang di-retain (metadata kecil).
func collect(query func() (array.RecordReader, error)) ([]arrow.RecordBatch, error) {
	rdr, err := query()
	if err != nil {
		return nil, err
	}
	defer rdr.Release()
	var recs []arrow.RecordBatch
	for rdr.Next() {
		rec := rdr.RecordBatch()
		rec.Retain()
		recs = append(recs, rec)
	}
	if err := rdr.Err(); err != nil {
		releaseAll(recs)
		return nil, err
	}
	return recs, nil
}

func releaseAll(recs []arrow.RecordBatch) {
	for _, r := range recs {
		r.Release()
	}
}

// --- helpers ---

// stream membuka reader dan mengalirkannya; reader dilepas oleh StreamChunksFromReader.
func (s *Server) stream(ctx context.Context, open func() (array.RecordReader, error)) (*arrow.Schema, <-chan flight.StreamChunk, error) {
	rdr, err := open()
	if err != nil {
		return nil, nil, toStatus(ctx, err)
	}
	ch := make(chan flight.StreamChunk)
	go flight.StreamChunksFromReader(ctx, rdr, ch)
	return rdr.Schema(), ch, nil
}

func flightInfo(desc *flight.FlightDescriptor, ticket []byte) *flight.FlightInfo {
	return &flight.FlightInfo{
		FlightDescriptor: desc,
		Endpoint:         []*flight.FlightEndpoint{{Ticket: &flight.Ticket{Ticket: ticket}}},
		TotalRecords:     -1,
		TotalBytes:       -1,
	}
}

// toStatus memetakan error engine ke kode gRPC (CLAUDE.md: Canceled, DeadlineExceeded, ...).
func toStatus(ctx context.Context, err error) error {
	switch {
	case errors.Is(context.Cause(ctx), session.ErrKilled):
		return status.Error(codes.Canceled, session.ErrKilled.Error())
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	}
	if errors.Is(err, engine.ErrPermission) {
		return status.Error(codes.PermissionDenied, err.Error())
	}
	// Error UDF melewati DuckDB sebagai teks; keputusan izin sudah dibuat di
	// sqlext, di sini hanya pemetaan kode.
	if strings.Contains(err.Error(), sqlext.ErrPermissionDenied.Error()+":") {
		return status.Error(codes.PermissionDenied, err.Error())
	}
	if strings.Contains(err.Error(), sqlext.ErrFailedPrecondition.Error()+":") {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	return status.Error(codes.InvalidArgument, err.Error())
}
