package flightsql

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

	"github.com/bankmega/karhutlasql/internal/engine"
	"github.com/bankmega/karhutlasql/internal/version"
)

// ticketTTL batas umur statement handle yang belum di-DoGet.
// ponytail: map global + timer per handle; pindah ke session registry di Fase 4.
const ticketTTL = 10 * time.Minute

// Server mengimplementasikan flightsql.Server di atas engine.Engine.
type Server struct {
	fsql.BaseServer
	eng engine.Engine
	log *slog.Logger

	mu       sync.Mutex
	stmts    map[string]string // handle → SQL, dikonsumsi DoGet
	prepared map[string]*prepared
}

type prepared struct {
	sql  string
	info engine.StatementInfo

	mu     sync.Mutex
	params []any // baris parameter terakhir yang di-bind (query)
}

// New membuat Server.
func New(eng engine.Engine, log *slog.Logger) (*Server, error) {
	s := &Server{eng: eng, log: log, stmts: map[string]string{}, prepared: map[string]*prepared{}}
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

func (s *Server) GetFlightInfoStatement(_ context.Context, cmd fsql.StatementQuery, desc *flight.FlightDescriptor) (*flight.FlightInfo, error) {
	if len(cmd.GetTransactionId()) > 0 {
		return nil, status.Error(codes.Unimplemented, "transactions are not supported")
	}
	handle := uuid.NewString()
	s.mu.Lock()
	s.stmts[handle] = cmd.GetQuery()
	s.mu.Unlock()
	time.AfterFunc(ticketTTL, func() { s.takeStmt(handle) })

	tkt, err := fsql.CreateStatementQueryTicket([]byte(handle))
	if err != nil {
		return nil, status.Errorf(codes.Internal, "flightsql.GetFlightInfoStatement: %v", err)
	}
	return flightInfo(desc, tkt), nil
}

func (s *Server) takeStmt(handle string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	q, ok := s.stmts[handle]
	delete(s.stmts, handle)
	return q, ok
}

func (s *Server) DoGetStatement(ctx context.Context, tkt fsql.StatementQueryTicket) (*arrow.Schema, <-chan flight.StreamChunk, error) {
	q, ok := s.takeStmt(string(tkt.GetStatementHandle()))
	if !ok {
		return nil, nil, status.Error(codes.NotFound, "unknown or expired statement handle")
	}
	return s.stream(ctx, func() (array.RecordReader, error) { return s.eng.Query(ctx, q) })
}

func (s *Server) DoPutCommandStatementUpdate(ctx context.Context, cmd fsql.StatementUpdate) (int64, error) {
	if len(cmd.GetTransactionId()) > 0 {
		return 0, status.Error(codes.Unimplemented, "transactions are not supported")
	}
	n, err := s.eng.Exec(ctx, cmd.GetQuery())
	if err != nil {
		return 0, toStatus(ctx, err)
	}
	return n, nil
}

// --- prepared statement ---

func (s *Server) CreatePreparedStatement(ctx context.Context, req fsql.ActionCreatePreparedStatementRequest) (fsql.ActionCreatePreparedStatementResult, error) {
	if len(req.GetTransactionId()) > 0 {
		return fsql.ActionCreatePreparedStatementResult{}, status.Error(codes.Unimplemented, "transactions are not supported")
	}
	info, err := s.eng.Describe(ctx, req.GetQuery())
	if err != nil {
		return fsql.ActionCreatePreparedStatementResult{}, toStatus(ctx, err)
	}
	handle := uuid.NewString()
	s.mu.Lock()
	s.prepared[handle] = &prepared{sql: req.GetQuery(), info: info}
	s.mu.Unlock()
	// DatasetSchema wajib bagi driver JDBC: tanpa itu statement dianggap update.
	isUpdate := !info.ReturnsRows
	return fsql.ActionCreatePreparedStatementResult{Handle: []byte(handle), DatasetSchema: info.Schema, IsUpdate: &isUpdate}, nil
}

func (s *Server) ClosePreparedStatement(_ context.Context, req fsql.ActionClosePreparedStatementRequest) error {
	s.mu.Lock()
	delete(s.prepared, string(req.GetPreparedStatementHandle()))
	s.mu.Unlock()
	return nil
}

func (s *Server) getPrepared(handle []byte) (*prepared, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.prepared[string(handle)]
	if !ok {
		return nil, status.Error(codes.NotFound, "unknown prepared statement handle")
	}
	return p, nil
}

func (s *Server) DoPutPreparedStatementQuery(_ context.Context, cmd fsql.PreparedStatementQuery, rdr flight.MessageReader, _ flight.MetadataWriter) ([]byte, error) {
	p, err := s.getPrepared(cmd.GetPreparedStatementHandle())
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

func (s *Server) GetFlightInfoPreparedStatement(_ context.Context, cmd fsql.PreparedStatementQuery, desc *flight.FlightDescriptor) (*flight.FlightInfo, error) {
	if _, err := s.getPrepared(cmd.GetPreparedStatementHandle()); err != nil {
		return nil, err
	}
	return flightInfo(desc, desc.Cmd), nil
}

func (s *Server) DoGetPreparedStatement(ctx context.Context, cmd fsql.PreparedStatementQuery) (*arrow.Schema, <-chan flight.StreamChunk, error) {
	p, err := s.getPrepared(cmd.GetPreparedStatementHandle())
	if err != nil {
		return nil, nil, err
	}
	p.mu.Lock()
	params := p.params
	p.mu.Unlock()
	return s.stream(ctx, func() (array.RecordReader, error) { return s.eng.Query(ctx, p.sql, params...) })
}

func (s *Server) DoPutPreparedStatementUpdate(ctx context.Context, cmd fsql.PreparedStatementUpdate, rdr flight.MessageReader) (int64, error) {
	p, err := s.getPrepared(cmd.GetPreparedStatementHandle())
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
	// ponytail: satu Exec per baris parameter, tanpa transaksi; bulk ingest di Fase 8.
	var total int64
	for _, args := range rows {
		n, err := s.eng.Exec(ctx, p.sql, args...)
		if err != nil {
			return total, toStatus(ctx, err)
		}
		total += n
	}
	return total, nil
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
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded):
		return status.Error(codes.DeadlineExceeded, err.Error())
	case errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled):
		return status.Error(codes.Canceled, err.Error())
	}
	return status.Error(codes.InvalidArgument, err.Error())
}
