// Package pgwire listener protokol PostgreSQL (backlog fase 9c) agar alat
// yang hanya bicara Postgres (psql, Power BI/Npgsql, ODBC, JDBC pg, pgx,
// psycopg) bisa memakai KarhutlaSQL. Setiap statement diteruskan sebagai
// panggilan Flight SQL in-process dengan token pemanggil dan kanal "pgwire",
// sehingga autentikasi, governance, antrean, kuota, audit, dan access
// history sama persis dengan klien Flight SQL. Package ini hanya
// menerjemahkan protokol; tidak ada keputusan akses di sini.
package pgwire

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/flight"
	"github.com/apache/arrow-go/v18/arrow/flight/flightsql"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/jackc/pgx/v5/pgproto3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Channel nilai header kanal (auth.ChannelHeader) untuk statement dari pgwire.
const Channel = "pgwire"

// Config dependensi Server.
type Config struct {
	// TLS: nil = hanya plaintext (dev); selain itu klien wajib TLS (SSLRequest).
	TLS *tls.Config
	// Login memverifikasi user + password (password user lokal, atau access
	// token OIDC/lokal sebagai password) dan mengembalikan bearer token.
	Login func(ctx context.Context, user, password string) (string, error)
	// Flight klien Flight SQL ke server ini (in-process).
	Flight *flightsql.Client
	// ChannelHeader nama header gRPC kanal (auth.ChannelHeader).
	ChannelHeader string
	Log           *slog.Logger
}

// Server listener protokol PostgreSQL v3.
type Server struct {
	cfg Config

	mu    sync.Mutex
	conns map[uint32]*conn      // process id → koneksi (CancelRequest)
	open  map[net.Conn]struct{} // semua socket klien (ditutup saat shutdown)
}

// New membuat Server.
func New(cfg Config) *Server {
	return &Server{cfg: cfg, conns: map[uint32]*conn{}, open: map[net.Conn]struct{}{}}
}

// Serve menerima koneksi sampai ctx selesai; saat itu semua koneksi klien
// ditutup (statement berjalan dibatalkan) dan Serve kembali setelah semua
// handler selesai — baru setelahnya klien Flight boleh ditutup.
func (s *Server) Serve(ctx context.Context, lis net.Listener) error {
	go func() {
		<-ctx.Done()
		_ = lis.Close()
		s.mu.Lock()
		for nc := range s.open {
			_ = nc.Close()
		}
		s.mu.Unlock()
	}()
	s.cfg.Log.Info("pgwire listening", "addr", lis.Addr().String(), "tls", s.cfg.TLS != nil)
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		nc, err := lis.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("pgwire.Serve: %w", err)
		}
		s.mu.Lock()
		if ctx.Err() != nil {
			s.mu.Unlock()
			_ = nc.Close()
			return nil
		}
		s.open[nc] = struct{}{}
		s.mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.open, nc)
				s.mu.Unlock()
			}()
			s.handle(ctx, nc)
		}()
	}
}

// conn keadaan satu koneksi klien.
type conn struct {
	s       *Server
	nc      net.Conn
	be      *pgproto3.Backend
	pid     uint32
	secret  []byte // kunci CancelRequest (4 byte, protokol 3.0)
	token   string
	stmts   map[string]*stmt
	portals map[string]*portal

	mu     sync.Mutex
	cancel context.CancelFunc // statement yang sedang berjalan (CancelRequest)
	closed bool               // klien sudah putus: panggilan baru langsung batal
}

// stmt hasil Parse. prep nil untuk statement yang dijawab lokal (local).
type stmt struct {
	sql       string
	prep      *flightsql.PreparedStatement
	paramOIDs []uint32
	local     *localResult
}

// portal hasil Bind; rdr/rec/row menyimpan posisi bila Execute dibatasi
// MaxRows (PortalSuspended).
type portal struct {
	st      *stmt
	params  []any
	formats []int16
	cols    []column
	rdr     *flight.Reader
	done    func() // melepas ctx panggilan stream
	rec     arrow.RecordBatch
	row     int
	sent    int64
}

func randUint32() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}

func (s *Server) handle(ctx context.Context, nc net.Conn) {
	defer func() { _ = nc.Close() }()
	c := &conn{s: s, nc: nc, be: pgproto3.NewBackend(nc, nc), stmts: map[string]*stmt{}, portals: map[string]*portal{}}
	_ = nc.SetDeadline(time.Now().Add(time.Minute)) // startup + auth
	startup, err := c.startup(ctx)
	if err != nil || startup == nil {
		return
	}
	_ = nc.SetDeadline(time.Time{})
	user := startup.Parameters["user"]
	if c.s.cfg.TLS != nil {
		if _, ok := c.nc.(*tls.Conn); !ok {
			c.fatal("28000", "SSL is required")
			return
		}
	}
	c.be.Send(&pgproto3.AuthenticationCleartextPassword{})
	if err := c.be.Flush(); err != nil {
		return
	}
	if err := c.be.SetAuthType(pgproto3.AuthTypeCleartextPassword); err != nil {
		return
	}
	msg, err := c.be.Receive()
	pw, ok := msg.(*pgproto3.PasswordMessage)
	if err != nil || !ok {
		return
	}
	if c.token, err = s.cfg.Login(ctx, user, pw.Password); err != nil {
		c.fatal("28P01", "password authentication failed for user \""+user+"\"")
		return
	}
	c.pid, c.secret = randUint32(), binary.BigEndian.AppendUint32(nil, randUint32())
	s.mu.Lock()
	s.conns[c.pid] = c
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, c.pid)
		s.mu.Unlock()
		c.closeAll(ctx)
	}()
	// Mulai sekarang socket dibaca terus oleh pump (deteksi klien putus).
	// Klien tidak mengirim apa pun sebelum ReadyForQuery, jadi tidak ada byte
	// tertinggal di buffer backend lama.
	p := newPump()
	go p.run(c.nc, c.disconnected)
	c.be = pgproto3.NewBackend(p, c.nc)
	c.be.Send(&pgproto3.AuthenticationOk{})
	for _, p := range [][2]string{
		{"server_version", "16.0"}, {"server_encoding", "UTF8"}, {"client_encoding", "UTF8"},
		{"DateStyle", "ISO, MDY"}, {"IntervalStyle", "postgres"}, {"TimeZone", "UTC"},
		{"integer_datetimes", "on"}, {"standard_conforming_strings", "on"}, {"is_superuser", "off"},
		{"session_authorization", user}, {"application_name", startup.Parameters["application_name"]},
	} {
		c.be.Send(&pgproto3.ParameterStatus{Name: p[0], Value: p[1]})
	}
	c.be.Send(&pgproto3.BackendKeyData{ProcessID: c.pid, SecretKey: c.secret})
	c.be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if c.be.Flush() != nil {
		return
	}
	c.loop(ctx)
}

// startup menangani SSLRequest/GSSEncRequest/CancelRequest; nil tanpa error
// = koneksi selesai (mis. cancel).
func (c *conn) startup(ctx context.Context) (*pgproto3.StartupMessage, error) {
	for {
		msg, err := c.be.ReceiveStartupMessage()
		if err != nil {
			return nil, err
		}
		switch m := msg.(type) {
		case *pgproto3.SSLRequest:
			if c.s.cfg.TLS == nil {
				if _, err := c.nc.Write([]byte{'N'}); err != nil {
					return nil, err
				}
				continue
			}
			if _, err := c.nc.Write([]byte{'S'}); err != nil {
				return nil, err
			}
			tc := tls.Server(c.nc, c.s.cfg.TLS)
			if err := tc.HandshakeContext(ctx); err != nil {
				return nil, err
			}
			c.nc = tc
			c.be = pgproto3.NewBackend(tc, tc)
		case *pgproto3.GSSEncRequest:
			if _, err := c.nc.Write([]byte{'N'}); err != nil {
				return nil, err
			}
		case *pgproto3.CancelRequest:
			c.s.cancelRequest(m.ProcessID, m.SecretKey)
			return nil, nil
		case *pgproto3.StartupMessage:
			return m, nil
		default:
			return nil, fmt.Errorf("unexpected startup message %T", msg)
		}
	}
}

func (s *Server) cancelRequest(pid uint32, secret []byte) {
	s.mu.Lock()
	c := s.conns[pid]
	s.mu.Unlock()
	if c == nil || subtle.ConstantTimeCompare(c.secret, secret) != 1 {
		return
	}
	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
	}
	c.mu.Unlock()
}

// disconnected: klien putus — batalkan statement yang berjalan.
func (c *conn) disconnected() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.cancel != nil {
		c.cancel()
	}
}

func (c *conn) fatal(code, msg string) {
	c.be.Send(&pgproto3.ErrorResponse{Severity: "FATAL", Code: code, Message: msg})
	_ = c.be.Flush()
}

// call ctx panggilan Flight dengan token pemanggil & kanal pgwire; dapat
// dibatalkan lewat CancelRequest.
func (c *conn) call(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	c.mu.Lock()
	c.cancel = cancel
	if c.closed {
		cancel()
	}
	c.mu.Unlock()
	md := []string{"authorization", "Bearer " + c.token}
	if c.s.cfg.ChannelHeader != "" {
		md = append(md, c.s.cfg.ChannelHeader, Channel)
	}
	return metadata.AppendToOutgoingContext(ctx, md...), func() {
		c.mu.Lock()
		c.cancel = nil
		c.mu.Unlock()
		cancel()
	}
}

func (c *conn) loop(ctx context.Context) {
	failed := false // extended protocol: abaikan pesan sampai Sync setelah error
	for {
		msg, err := c.be.Receive()
		if err != nil {
			return
		}
		if failed {
			if _, ok := msg.(*pgproto3.Sync); !ok {
				if _, ok := msg.(*pgproto3.Terminate); ok {
					return
				}
				continue
			}
			failed = false
		}
		switch m := msg.(type) {
		case *pgproto3.Query:
			c.simpleQuery(ctx, m.String)
			c.be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		case *pgproto3.Parse:
			failed = !c.ok(c.parse(ctx, m))
		case *pgproto3.Bind:
			failed = !c.ok(c.bind(m))
		case *pgproto3.Describe:
			failed = !c.ok(c.describe(m))
		case *pgproto3.Execute:
			failed = !c.ok(c.execute(ctx, m))
		case *pgproto3.Close:
			c.closeObject(ctx, m)
			c.be.Send(&pgproto3.CloseComplete{})
		case *pgproto3.Sync:
			c.be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		case *pgproto3.Flush:
		case *pgproto3.Terminate:
			return
		default:
			c.be.Send(errorResponse(status.Errorf(codes.Unimplemented, "message %T is not supported", msg)))
		}
		switch msg.(type) {
		case *pgproto3.Query, *pgproto3.Sync, *pgproto3.Flush:
			if c.be.Flush() != nil {
				return
			}
		}
	}
}

// ok mengirim ErrorResponse bila err; false = error.
func (c *conn) ok(err error) bool {
	if err != nil {
		c.be.Send(errorResponse(err))
		return false
	}
	return true
}

func (c *conn) simpleQuery(ctx context.Context, q string) {
	q = trimSQL(q)
	if q == "" {
		c.be.Send(&pgproto3.EmptyQueryResponse{})
		return
	}
	st, err := c.prepare(ctx, q, nil)
	if err != nil {
		c.be.Send(errorResponse(err))
		return
	}
	defer c.closeStmt(ctx, st)
	p := &portal{st: st}
	if err := c.run(ctx, p, 0, true); err != nil {
		c.be.Send(errorResponse(err))
	}
}

func (c *conn) parse(ctx context.Context, m *pgproto3.Parse) error {
	if old := c.stmts[m.Name]; old != nil {
		if m.Name != "" {
			return status.Errorf(codes.AlreadyExists, "prepared statement %q already exists", m.Name)
		}
		c.closeStmt(ctx, old)
	}
	st, err := c.prepare(ctx, trimSQL(m.Query), m.ParameterOIDs)
	if err != nil {
		return err
	}
	c.stmts[m.Name] = st
	c.be.Send(&pgproto3.ParseComplete{})
	return nil
}

// prepare membuat stmt: statement sesi yang dijawab lokal, atau prepared
// statement Flight SQL (otorisasi pertama terjadi di sini).
func (c *conn) prepare(ctx context.Context, q string, oids []uint32) (*stmt, error) {
	st := &stmt{sql: q, paramOIDs: oids}
	if lr, ok := local(q); ok {
		st.local = &lr
		return st, nil
	}
	cctx, done := c.call(ctx)
	defer done()
	prep, err := c.s.cfg.Flight.Prepare(cctx, q)
	if err != nil {
		return nil, err
	}
	st.prep = prep
	if ps := prep.ParameterSchema(); ps != nil {
		for i := len(st.paramOIDs); i < ps.NumFields(); i++ {
			st.paramOIDs = append(st.paramOIDs, 0)
		}
		for i := range ps.NumFields() {
			if st.paramOIDs[i] == 0 {
				st.paramOIDs[i] = paramOID(ps.Field(i))
			}
		}
	}
	return st, nil
}

func (c *conn) bind(m *pgproto3.Bind) error {
	st := c.stmts[m.PreparedStatement]
	if st == nil {
		return status.Errorf(codes.NotFound, "prepared statement %q does not exist", m.PreparedStatement)
	}
	if len(m.Parameters) != len(st.paramOIDs) {
		return status.Errorf(codes.InvalidArgument, "bind message supplies %d parameters, but prepared statement requires %d",
			len(m.Parameters), len(st.paramOIDs))
	}
	params := make([]any, len(m.Parameters))
	for i, b := range m.Parameters {
		var err error
		if params[i], err = decodeParam(st.paramOIDs[i], formatAt(m.ParameterFormatCodes, i), b); err != nil {
			return status.Errorf(codes.InvalidArgument, "parameter $%d: %v", i+1, err)
		}
	}
	if old := c.portals[m.DestinationPortal]; old != nil {
		old.release()
	}
	c.portals[m.DestinationPortal] = &portal{st: st, params: params, formats: m.ResultFormatCodes}
	c.be.Send(&pgproto3.BindComplete{})
	return nil
}

func formatAt(codes []int16, i int) int16 {
	switch len(codes) {
	case 0:
		return 0
	case 1:
		return codes[0]
	}
	if i < len(codes) {
		return codes[i]
	}
	return 0
}

func (c *conn) describe(m *pgproto3.Describe) error {
	if m.ObjectType == 'S' {
		st := c.stmts[m.Name]
		if st == nil {
			return status.Errorf(codes.NotFound, "prepared statement %q does not exist", m.Name)
		}
		c.be.Send(&pgproto3.ParameterDescription{ParameterOIDs: st.paramOIDs})
		c.sendRowDescription(st, nil)
		return nil
	}
	p := c.portals[m.Name]
	if p == nil {
		return status.Errorf(codes.NotFound, "portal %q does not exist", m.Name)
	}
	c.sendRowDescription(p.st, p.formats)
	return nil
}

// sendRowDescription dari schema hasil prepared statement (NoData bila
// statement tidak menghasilkan baris).
func (c *conn) sendRowDescription(st *stmt, formats []int16) {
	var sch *arrow.Schema
	if st.local != nil {
		sch = st.local.schema
	} else if isUpdate, ok := st.prep.IsUpdate(); !ok || !isUpdate {
		sch = st.prep.DatasetSchema()
	}
	if sch == nil {
		c.be.Send(&pgproto3.NoData{})
		return
	}
	c.be.Send(rowDescription(columns(sch), formats))
}

func columns(sch *arrow.Schema) []column {
	cols := make([]column, sch.NumFields())
	for i, f := range sch.Fields() {
		cols[i] = columnFor(f)
	}
	return cols
}

func rowDescription(cols []column, formats []int16) *pgproto3.RowDescription {
	rd := &pgproto3.RowDescription{Fields: make([]pgproto3.FieldDescription, len(cols))}
	for i, col := range cols {
		rd.Fields[i] = pgproto3.FieldDescription{Name: []byte(col.name), DataTypeOID: col.oid, DataTypeSize: col.size,
			TypeModifier: -1, Format: formatAt(formats, i)}
	}
	return rd
}

func (c *conn) execute(ctx context.Context, m *pgproto3.Execute) error {
	p := c.portals[m.Portal]
	if p == nil {
		return status.Errorf(codes.NotFound, "portal %q does not exist", m.Portal)
	}
	return c.run(ctx, p, int64(m.MaxRows), false)
}

// run mengeksekusi portal (atau melanjutkan yang tersuspensi). describe true
// (simple query) mengirim RowDescription sebelum baris.
func (c *conn) run(ctx context.Context, p *portal, maxRows int64, describe bool) error {
	if p.st.local != nil {
		lr := p.st.local
		if lr.err != nil {
			return lr.err
		}
		if lr.notice != "" {
			c.be.Send(&pgproto3.NoticeResponse{Severity: "WARNING", Code: "25P01", Message: lr.notice})
		}
		if lr.schema != nil {
			cols := columns(lr.schema)
			if describe {
				c.be.Send(rowDescription(cols, nil))
			}
			for _, row := range lr.rows {
				c.be.Send(&pgproto3.DataRow{Values: [][]byte{[]byte(row)}})
			}
		}
		c.be.Send(&pgproto3.CommandComplete{CommandTag: []byte(lr.tag)})
		return nil
	}
	if p.rdr == nil {
		if err := c.start(ctx, p, describe); err != nil || p.rdr == nil {
			return err
		}
	}
	return c.stream(p, maxRows)
}

// start mengikat parameter lalu mengeksekusi; update langsung selesai.
func (c *conn) start(ctx context.Context, p *portal, describe bool) error {
	prep := p.st.prep
	if len(p.params) > 0 {
		rec, err := paramRecord(prep.ParameterSchema(), p.params)
		if err != nil {
			return status.Errorf(codes.InvalidArgument, "%v", err)
		}
		prep.SetParameters(rec)
		rec.Release()
	}
	cctx, done := c.call(ctx)
	if isUpdate, ok := prep.IsUpdate(); ok && isUpdate {
		defer done()
		n, err := prep.ExecuteUpdate(cctx)
		if err != nil {
			return err
		}
		c.be.Send(&pgproto3.CommandComplete{CommandTag: []byte(commandTag(p.st.sql, n))})
		return nil
	}
	info, err := prep.Execute(cctx)
	if err == nil && len(info.Endpoint) != 1 {
		err = status.Errorf(codes.Internal, "want 1 endpoint, got %d", len(info.Endpoint))
	}
	if err != nil {
		done()
		return err
	}
	rdr, err := c.s.cfg.Flight.DoGet(cctx, info.Endpoint[0].Ticket)
	if err != nil {
		done()
		return err
	}
	p.rdr, p.done, p.cols = rdr, done, columns(rdr.Schema())
	if describe {
		c.be.Send(rowDescription(p.cols, nil))
	}
	return nil
}

// stream mengirim baris sampai habis atau maxRows (> 0) tercapai.
func (c *conn) stream(p *portal, maxRows int64) error {
	var n int64
	values := make([][]byte, len(p.cols))
	for {
		if p.rec == nil || p.row >= int(p.rec.NumRows()) {
			if !p.rdr.Next() {
				err := p.rdr.Err()
				p.release()
				if err != nil && !errors.Is(err, io.EOF) {
					return err
				}
				c.be.Send(&pgproto3.CommandComplete{CommandTag: []byte(fmt.Sprintf("SELECT %d", p.sent))})
				return nil
			}
			p.rec, p.row = p.rdr.RecordBatch(), 0
			continue
		}
		if maxRows > 0 && n == maxRows {
			c.be.Send(&pgproto3.PortalSuspended{})
			return nil
		}
		for j, col := range p.cols {
			arr := p.rec.Column(j)
			if arr.IsNull(p.row) {
				values[j] = nil
				continue
			}
			values[j] = col.encode(arr, p.row, formatAt(p.formats, j) == 1)
		}
		c.be.Send(&pgproto3.DataRow{Values: values})
		p.row++
		n++
		p.sent++
	}
}

func (p *portal) release() {
	if p.rdr != nil {
		p.rdr.Release()
		p.rdr, p.rec = nil, nil
	}
	if p.done != nil {
		p.done()
		p.done = nil
	}
}

// paramRecord satu baris parameter: kolom bertipe dari nilai yang diterima
// (biner bertipe, teks → utf8; null → tipe ParameterSchema).
func paramRecord(ps *arrow.Schema, params []any) (arrow.RecordBatch, error) {
	mem := memory.DefaultAllocator
	fields := make([]arrow.Field, len(params))
	cols := make([]arrow.Array, len(params))
	defer func() {
		for _, a := range cols {
			if a != nil {
				a.Release()
			}
		}
	}()
	for i, v := range params {
		var b array.Builder
		switch x := v.(type) {
		case nil:
			t := arrow.DataType(arrow.BinaryTypes.String)
			if ps != nil && i < ps.NumFields() {
				t = ps.Field(i).Type
			}
			b = array.NewBuilder(mem, t)
			b.AppendNull()
		case string:
			sb := array.NewStringBuilder(mem)
			sb.Append(x)
			b = sb
		case int64:
			ib := array.NewInt64Builder(mem)
			ib.Append(x)
			b = ib
		case float64:
			fb := array.NewFloat64Builder(mem)
			fb.Append(x)
			b = fb
		case bool:
			bb := array.NewBooleanBuilder(mem)
			bb.Append(x)
			b = bb
		case []byte:
			bb := array.NewBinaryBuilder(mem, arrow.BinaryTypes.Binary)
			bb.Append(x)
			b = bb
		case time.Time:
			tb := array.NewTimestampBuilder(mem, &arrow.TimestampType{Unit: arrow.Microsecond, TimeZone: "UTC"})
			tb.Append(arrow.Timestamp(x.UnixMicro()))
			b = tb
		default:
			return nil, fmt.Errorf("parameter $%d: unsupported value %T", i+1, v)
		}
		cols[i] = b.NewArray()
		b.Release()
		fields[i] = arrow.Field{Name: fmt.Sprintf("$%d", i+1), Type: cols[i].DataType(), Nullable: true}
	}
	return array.NewRecordBatch(arrow.NewSchema(fields, nil), cols, 1), nil
}

func (c *conn) closeObject(ctx context.Context, m *pgproto3.Close) {
	if m.ObjectType == 'S' {
		if st := c.stmts[m.Name]; st != nil {
			c.closeStmt(ctx, st)
			delete(c.stmts, m.Name)
		}
		return
	}
	if p := c.portals[m.Name]; p != nil {
		p.release()
		delete(c.portals, m.Name)
	}
}

func (c *conn) closeStmt(ctx context.Context, st *stmt) {
	for name, p := range c.portals {
		if p.st == st {
			p.release()
			delete(c.portals, name)
		}
	}
	if st.prep != nil {
		cctx, done := c.call(context.WithoutCancel(ctx))
		_ = st.prep.Close(cctx)
		done()
	}
}

func (c *conn) closeAll(ctx context.Context) {
	for _, st := range c.stmts {
		c.closeStmt(ctx, st)
	}
}

// trimSQL membuang spasi dan titik koma penutup.
func trimSQL(q string) string {
	return strings.TrimRight(strings.TrimSpace(q), "; \t\r\n")
}

// commandTag tag CommandComplete untuk statement tanpa hasil baris.
func commandTag(q string, n int64) string {
	verb := strings.ToUpper(firstWord(q))
	switch verb {
	case "INSERT":
		return fmt.Sprintf("INSERT 0 %d", n)
	case "UPDATE", "DELETE", "MERGE", "COPY":
		return fmt.Sprintf("%s %d", verb, n)
	case "CREATE", "DROP", "ALTER":
		w := strings.Fields(strings.ToUpper(q))
		if len(w) > 1 {
			return verb + " " + strings.TrimSuffix(w[1], ";")
		}
	}
	return verb
}

func firstWord(q string) string {
	f := strings.Fields(q)
	if len(f) == 0 {
		return ""
	}
	return f[0]
}

// errorResponse status gRPC → ErrorResponse dengan SQLSTATE yang setara.
func errorResponse(err error) *pgproto3.ErrorResponse {
	st, _ := status.FromError(err)
	code := map[codes.Code]string{
		codes.Unauthenticated:    "28000",
		codes.PermissionDenied:   "42501",
		codes.InvalidArgument:    "42000",
		codes.NotFound:           "26000",
		codes.AlreadyExists:      "42P05",
		codes.ResourceExhausted:  "53000",
		codes.DeadlineExceeded:   "57014",
		codes.Canceled:           "57014",
		codes.FailedPrecondition: "55000",
		codes.Unimplemented:      "0A000",
		codes.Unavailable:        "08006",
	}[st.Code()]
	if code == "" {
		code = "XX000"
	}
	return &pgproto3.ErrorResponse{Severity: "ERROR", Code: code, Message: st.Message()}
}
