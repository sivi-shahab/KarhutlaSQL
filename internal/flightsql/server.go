package flightsql

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/flight"
	fsql "github.com/apache/arrow-go/v18/arrow/flight/flightsql"
	"github.com/apache/arrow-go/v18/arrow/flight/flightsql/schema_ref"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/google/uuid"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bankmega/karhutlasql/internal/audit"
	"github.com/bankmega/karhutlasql/internal/auth"
	"github.com/bankmega/karhutlasql/internal/engine"
	"github.com/bankmega/karhutlasql/internal/metrics"
	"github.com/bankmega/karhutlasql/internal/session"
	"github.com/bankmega/karhutlasql/internal/sqlext"
	"github.com/bankmega/karhutlasql/internal/version"
)

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
}

// Server mengimplementasikan flightsql.Server di atas engine.Engine.
type Server struct {
	fsql.BaseServer
	eng  engine.Engine
	log  *slog.Logger
	opts Options
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
	if opts.Audit == nil {
		return nil, errors.New("flightsql.New: audit logger is required")
	}
	s := &Server{eng: eng, log: log, opts: opts}
	s.Alloc = memory.DefaultAllocator
	for id, v := range map[fsql.SqlInfo]any{
		fsql.SqlInfoFlightSqlServerName:     "KarhutlaSQL",
		fsql.SqlInfoFlightSqlServerVersion:  version.Version,
		fsql.SqlInfoFlightSqlServerReadOnly: false,
		fsql.SqlInfoFlightSqlServerSql:      true,
		fsql.SqlInfoQuotedIdentifierCase:    int32(fsql.SqlCaseSensitivityCaseInsensitive),
		fsql.SqlInfoIdentifierQuoteChar:     `"`,
	} {
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
	info, err := s.eng.Describe(ctx, req.GetQuery())
	if err != nil {
		return fsql.ActionCreatePreparedStatementResult{}, toStatus(ctx, err)
	}
	handle := uuid.NewString()
	ss.Put(handle, &prepared{sql: req.GetQuery(), info: info})
	// DatasetSchema wajib bagi driver JDBC: tanpa itu statement dianggap update.
	isUpdate := !info.ReturnsRows
	return fsql.ActionCreatePreparedStatementResult{Handle: []byte(handle), DatasetSchema: info.Schema, IsUpdate: &isUpdate}, nil
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

// begin mendaftarkan statement di sesi (kill membatalkannya), mencatat
// statement_start, dan mengembalikan ctx eksekusi (dengan QueryTimeout) serta
// end yang wajib dipanggil tepat sekali bila err nil.
func (s *Server) begin(ctx context.Context, kind, sql string) (context.Context, func(rows, bytes int64, err error), error) {
	ss, err := sess(ctx)
	if err != nil {
		return nil, nil, err
	}
	id, _ := auth.FromContext(ctx)
	ev := audit.Event{User: id.Subject, Source: id.Source, Kind: kind}
	s.opts.Audit.SQL(&ev, sql)
	start := ev
	start.Type = audit.StatementStart
	s.opts.Audit.Emit(ctx, start)
	if s.opts.PrintQueries {
		s.log.Info("statement", "request_id", audit.RequestID(ctx), "user", id.Subject, "kind", kind, "sql", sql)
	}

	qctx, done := ss.Begin(ctx, sql)
	cancel := context.CancelFunc(func() {})
	if s.opts.QueryTimeout > 0 {
		qctx, cancel = context.WithTimeout(qctx, s.opts.QueryTimeout)
	}
	t0 := time.Now()
	return qctx, func(rows, bytes int64, err error) {
		cancel()
		done()
		ss.Rows.Add(rows)
		ss.Bytes.Add(bytes)
		end := ev
		end.Type, end.Rows, end.DurationMS = audit.StatementEnd, rows, time.Since(t0).Milliseconds()
		switch {
		case err != nil:
			end.Status, end.Error = status.Code(err).String(), status.Convert(err).Message()
		case ctx.Err() != nil:
			end.Status = codes.Canceled.String() // klien memutus stream
		default:
			end.Status = codes.OK.String()
		}
		s.opts.Audit.Emit(ctx, end)
		s.opts.Metrics.Statement(id.Subject, end.Status, time.Since(t0))
		if kind == "query" {
			s.opts.Metrics.Sent(id.Subject, rows, bytes)
		}
	}, nil
}

// query mengeksekusi SELECT dan mengalirkan hasilnya. Timeout berlaku untuk
// engine; stream memakai ctx RPC agar timeout muncul sebagai error ke klien,
// bukan stream yang terpotong diam-diam.
func (s *Server) query(ctx context.Context, sql string, params []any) (*arrow.Schema, <-chan flight.StreamChunk, error) {
	qctx, end, err := s.begin(ctx, "query", sql)
	if err != nil {
		return nil, nil, err
	}
	rdr, err := s.eng.Query(qctx, sql, params...)
	if err != nil {
		err = toStatus(qctx, err)
		end(0, 0, err)
		return nil, nil, err
	}
	ar := &auditedReader{RecordReader: rdr, qctx: qctx, end: end}
	ch := make(chan flight.StreamChunk)
	go flight.StreamChunksFromReader(ctx, ar, ch)
	return rdr.Schema(), ch, nil
}

// update mengeksekusi statement untuk setiap baris parameter.
// ponytail: satu Exec per baris, tanpa transaksi; bulk ingest di Fase 8.
func (s *Server) update(ctx context.Context, sql string, rows [][]any) (int64, error) {
	qctx, end, err := s.begin(ctx, "update", sql)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, args := range rows {
		n, err := s.eng.Exec(qctx, sql, args...)
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
	qctx  context.Context
	rows  int64
	bytes int64
	end   func(rows, bytes int64, err error)
	once  sync.Once
}

func (r *auditedReader) Next() bool {
	if !r.RecordReader.Next() {
		return false
	}
	rec := r.RecordBatch()
	r.rows += rec.NumRows()
	for _, c := range rec.Columns() {
		r.bytes += dataSize(c.Data())
	}
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

// --- metadata ---

func (s *Server) GetFlightInfoCatalogs(_ context.Context, desc *flight.FlightDescriptor) (*flight.FlightInfo, error) {
	return flightInfo(desc, desc.Cmd), nil
}

func (s *Server) DoGetCatalogs(ctx context.Context) (*arrow.Schema, <-chan flight.StreamChunk, error) {
	return s.metadata(ctx, schema_ref.Catalogs, func() (array.RecordReader, error) { return s.eng.Catalogs(ctx) })
}

func (s *Server) GetFlightInfoSchemas(_ context.Context, _ fsql.GetDBSchemas, desc *flight.FlightDescriptor) (*flight.FlightInfo, error) {
	return flightInfo(desc, desc.Cmd), nil
}

func (s *Server) DoGetDBSchemas(ctx context.Context, cmd fsql.GetDBSchemas) (*arrow.Schema, <-chan flight.StreamChunk, error) {
	return s.metadata(ctx, schema_ref.DBSchemas, func() (array.RecordReader, error) {
		return s.eng.Schemas(ctx, cmd.GetCatalog(), cmd.GetDBSchemaFilterPattern())
	})
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
		return s.metadata(ctx, schema_ref.Tables, query)
	}
	return s.tablesWithSchema(ctx, query)
}

func (s *Server) GetFlightInfoTableTypes(_ context.Context, desc *flight.FlightDescriptor) (*flight.FlightInfo, error) {
	return flightInfo(desc, desc.Cmd), nil
}

func (s *Server) DoGetTableTypes(ctx context.Context) (*arrow.Schema, <-chan flight.StreamChunk, error) {
	return s.metadata(ctx, schema_ref.TableTypes, func() (array.RecordReader, error) { return s.eng.TableTypes(ctx) })
}

// tablesWithSchema menambahkan kolom table_schema (schema IPC per tabel).
func (s *Server) tablesWithSchema(ctx context.Context, query func() (array.RecordReader, error)) (*arrow.Schema, <-chan flight.StreamChunk, error) {
	recs, err := collect(query)
	if err != nil {
		return nil, nil, toStatus(ctx, err)
	}
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
func (s *Server) metadata(ctx context.Context, sch *arrow.Schema, query func() (array.RecordReader, error)) (*arrow.Schema, <-chan flight.StreamChunk, error) {
	recs, err := collect(query)
	if err != nil {
		return nil, nil, toStatus(ctx, err)
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
	// Error UDF melewati DuckDB sebagai teks; keputusan izin sudah dibuat di
	// sqlext, di sini hanya pemetaan kode.
	if strings.Contains(err.Error(), sqlext.ErrPermissionDenied.Error()+":") {
		return status.Error(codes.PermissionDenied, err.Error())
	}
	return status.Error(codes.InvalidArgument, err.Error())
}
