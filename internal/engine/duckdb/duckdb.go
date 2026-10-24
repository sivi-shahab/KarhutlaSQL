//go:build duckdb_arrow

// Package duckdb mengimplementasikan engine.Engine di atas DuckDB (duckdb-go, Arrow interface).
package duckdb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	duckdbdrv "github.com/duckdb/duckdb-go/v2"

	"github.com/bankmega/karhutlasql/internal/engine"
)

// Config konfigurasi engine DuckDB.
type Config struct {
	// Path file .duckdb; kosong = in-memory.
	Path string
	// InitSQL dieksekusi sekali saat Open (INSTALL/LOAD extension, ATTACH, ...).
	InitSQL string
	// Setup dipanggil sekali saat Open dengan satu koneksi, mis. untuk
	// mendaftarkan UDF (berlaku di seluruh database). Engine tidak tahu isinya.
	Setup func(ctx context.Context, conn *sql.Conn) error
	// Guardrail residency (G-07), diterapkan setelah InitSQL & Secrets: akses
	// file/URL hanya di AllowedDirectories (path atau prefix URL, mis.
	// "s3://lake/"); INSTALL/LOAD/ATTACH/COPY di luar itu dan persistent
	// secret ditolak DuckDB sendiri. Tidak bisa dibalik selama proses hidup
	// (DuckDB menolak enable_external_access=true dan perubahan
	// allowed_directories), jadi lock_configuration tidak dipakai — itu juga
	// mengunci SET per koneksi yang dipakai server (explain/profiling).
	AllowedDirectories []string
	// Secrets TEMPORARY SECRET yang diinjeksi server; SCOPE wajib di dalam
	// AllowedDirectories.
	Secrets []Secret
	// Unlocked mematikan guardrail (test engine saja).
	Unlocked bool
	// StreamingBufferSize batas memori buffer hasil streaming per statement
	// (setting DuckDB streaming_buffer_size, mis. "64MB"); kosong = hasil
	// dimaterialisasi lewat duckdb-go (ADR 17/34).
	StreamingBufferSize string
}

// Secret satu TEMPORARY SECRET DuckDB.
type Secret struct {
	Name, Type, Scope string
	Options           map[string]string // nilai rahasia sudah diambil dari env
}

// Engine adalah engine.Engine berbasis DuckDB. Setiap statement memakai koneksi
// sendiri dari connector (semua koneksi berbagi satu database instance), sehingga
// cancel satu statement tidak memengaruhi statement lain.
type Engine struct {
	connector *duckdbdrv.Connector
	closed    atomic.Bool
	streamErr error // nil = SELECT tanpa parameter di-stream (stream.go)

	// Fungsi ber-stability CONSISTENT (Deterministic), dimuat sekali.
	consistentOnce sync.Once
	consistent     map[string]bool
	consistentErr  error
}

var _ engine.Engine = (*Engine)(nil)

// Open membuka database dan menjalankan InitSQL.
func Open(ctx context.Context, cfg Config) (*Engine, error) {
	var initConn func(driver.ExecerContext) error
	if cfg.StreamingBufferSize != "" {
		initConn = setStreamingBuffer(cfg.StreamingBufferSize) //nolint:contextcheck // connInitFn duckdb-go tanpa ctx
	}
	c, err := duckdbdrv.NewConnector(cfg.Path, initConn)
	if err != nil {
		return nil, wrap("duckdb.Open", err)
	}
	e := &Engine{connector: c, streamErr: errors.New("disabled")}
	if cfg.StreamingBufferSize != "" {
		dc, err := c.Connect(ctx)
		if err != nil {
			return nil, errors.Join(wrap("duckdb.Open", err), c.Close())
		}
		e.streamErr = probeStreaming(dc.(*duckdbdrv.Conn)) //nolint:forcetypeassert // connector duckdb-go
		_ = dc.Close()
	}
	if cfg.Setup != nil {
		if err := setup(ctx, c, cfg.Setup); err != nil {
			return nil, errors.Join(fmt.Errorf("duckdb.Open: setup: %w", err), c.Close())
		}
	}
	if strings.TrimSpace(cfg.InitSQL) != "" {
		if _, err := e.Exec(ctx, cfg.InitSQL); err != nil {
			return nil, errors.Join(fmt.Errorf("duckdb.Open: init sql: %w", err), c.Close())
		}
	}
	if err := e.harden(ctx, cfg); err != nil {
		return nil, errors.Join(wrap("duckdb.Open", err), c.Close())
	}
	return e, nil
}

// setStreamingBuffer init koneksi: streaming_buffer_size tidak bisa global.
// duckdb-go tidak meneruskan ctx ke init koneksi; SET ini instan.
func setStreamingBuffer(size string) func(driver.ExecerContext) error {
	set := "SET streaming_buffer_size = " + quoteLiteral(size)
	return func(x driver.ExecerContext) error {
		_, err := x.ExecContext(context.Background(), set, nil)
		return err
	}
}

// Streaming nil bila hasil SELECT di-stream; selain itu alasan jalur
// materialisasi dipakai (dimatikan, atau struktur duckdb-go tidak dikenali).
func (e *Engine) Streaming() error { return e.streamErr }

// harden menginjeksi secret lalu mengunci akses eksternal (G-07).
func (e *Engine) harden(ctx context.Context, cfg Config) error {
	for _, s := range cfg.Secrets {
		q, err := secretSQL(s, cfg.AllowedDirectories)
		if err != nil {
			return err
		}
		if _, err := e.Exec(ctx, q); err != nil {
			msg := err.Error()
			for _, v := range s.Options {
				if v != "" && strings.Contains(msg, v) {
					msg = "(error text withheld: contains secret value)"
				}
			}
			return fmt.Errorf("secret %s: %s", s.Name, msg)
		}
	}
	if cfg.Unlocked {
		return nil
	}
	dirs := make([]string, len(cfg.AllowedDirectories))
	for i, d := range cfg.AllowedDirectories {
		dirs[i] = quoteLiteral(d)
	}
	q := fmt.Sprintf("SET allowed_directories = [%s]; SET enable_external_access = false", strings.Join(dirs, ", "))
	if _, err := e.Exec(ctx, q); err != nil {
		return fmt.Errorf("guardrail: %w", err)
	}
	return nil
}

// secretSQL CREATE TEMPORARY SECRET; scope di luar allowlist → error.
func secretSQL(s Secret, allowed []string) (string, error) {
	inScope := false
	for _, a := range allowed {
		inScope = inScope || (a != "" && strings.HasPrefix(s.Scope, a))
	}
	if s.Name == "" || s.Type == "" || !inScope {
		return "", fmt.Errorf("secret %q: name, type and a scope inside allowed directories are required", s.Name)
	}
	parts := []string{"TYPE " + quoteIdent(s.Type), "SCOPE " + quoteLiteral(s.Scope)}
	keys := make([]string, 0, len(s.Options))
	for k := range s.Options {
		if strings.EqualFold(k, "scope") || strings.EqualFold(k, "type") || strings.EqualFold(k, "persistent") {
			return "", fmt.Errorf("secret %q: option %s not allowed", s.Name, k)
		}
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		parts = append(parts, quoteIdent(k)+" "+quoteLiteral(s.Options[k]))
	}
	return "CREATE TEMPORARY SECRET " + quoteIdent(s.Name) + " (" + strings.Join(parts, ", ") + ")", nil
}

// wrap membungkus error dengan nama operasi; error izin DuckDB (guardrail
// G-07) ditandai engine.ErrPermission.
func wrap(op string, err error) error {
	var de *duckdbdrv.Error
	if errors.As(err, &de) && de.Type == duckdbdrv.ErrorTypePermission {
		return fmt.Errorf("%s: %w: %w", op, engine.ErrPermission, err)
	}
	return fmt.Errorf("%s: %w", op, err)
}

func quoteLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// noClose menyembunyikan Close connector agar sql.DB.Close tidak menutup database.
type noClose struct{ driver.Connector }

func setup(ctx context.Context, c *duckdbdrv.Connector, fn func(context.Context, *sql.Conn) error) error {
	db := sql.OpenDB(noClose{c})
	conn, err := db.Conn(ctx)
	if err != nil {
		return errors.Join(err, db.Close())
	}
	return errors.Join(fn(ctx, conn), conn.Close(), db.Close())
}

func (e *Engine) conn(ctx context.Context) (*duckdbdrv.Conn, error) {
	if e.closed.Load() {
		return nil, errors.New("engine closed")
	}
	dc, err := e.connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return dc.(*duckdbdrv.Conn), nil
}

// Query lihat engine.Engine.
func (e *Engine) Query(ctx context.Context, query string, args ...any) (array.RecordReader, error) {
	c, err := e.conn(ctx)
	if err != nil {
		return nil, wrap("duckdb.Query", err)
	}
	st := engine.StatsFrom(ctx)
	if err := enableProfiling(ctx, c, st); err != nil {
		return nil, errors.Join(wrap("duckdb.Query", err), c.Close())
	}
	if e.streamErr == nil && len(args) == 0 {
		rdr, err := stream(ctx, c, query)
		switch {
		case err == nil:
			return &connReader{RecordReader: rdr, conn: c, st: st}, nil
		case !errors.Is(err, errPrepare):
			return nil, errors.Join(wrap("duckdb.Query", err), c.Close())
		}
		// prepare gagal / butuh parameter: jalur duckdb-go memberi error bertipe
	}
	ar, err := duckdbdrv.NewArrowFromConn(c)
	if err != nil {
		return nil, errors.Join(wrap("duckdb.Query", err), c.Close())
	}
	rdr, err := ar.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, errors.Join(wrap("duckdb.Query", err), c.Close())
	}
	return &connReader{RecordReader: rdr, conn: c, st: st}, nil
}

// connReader menutup koneksi (setelah mengisi statistik) saat reader dilepas
// terakhir kali.
type connReader struct {
	array.RecordReader
	conn *duckdbdrv.Conn
	st   *engine.Stats
	refs atomic.Int64 // tambahan di atas referensi awal
}

func (r *connReader) Retain() { r.refs.Add(1); r.RecordReader.Retain() }

func (r *connReader) Release() {
	r.RecordReader.Release()
	if r.refs.Add(-1) < 0 {
		closeConn(context.Background(), r.conn, r.st)
	}
}

// Snapshot lihat engine.Engine.
func (e *Engine) Snapshot(ctx context.Context) (engine.Snapshot, error) {
	c, err := e.conn(ctx)
	if err != nil {
		return nil, wrap("duckdb.Snapshot", err)
	}
	if _, err := c.ExecContext(ctx, "BEGIN TRANSACTION", nil); err != nil {
		return nil, errors.Join(wrap("duckdb.Snapshot", err), c.Close())
	}
	return &snapshot{c: c}, nil
}

type snapshot struct{ c *duckdbdrv.Conn }

func (s *snapshot) Query(ctx context.Context, query string, args ...any) (array.RecordReader, error) {
	ar, err := duckdbdrv.NewArrowFromConn(s.c)
	if err != nil {
		return nil, wrap("duckdb.Snapshot.Query", err)
	}
	rdr, err := ar.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, wrap("duckdb.Snapshot.Query", err)
	}
	return rdr, nil
}

func (s *snapshot) Close() error {
	_, err := s.c.ExecContext(context.Background(), "ROLLBACK", nil)
	return errors.Join(err, s.c.Close())
}

// Exec lihat engine.Engine.
func (e *Engine) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	c, err := e.conn(ctx)
	if err != nil {
		return 0, wrap("duckdb.Exec", err)
	}
	st := engine.StatsFrom(ctx)
	defer closeConn(context.WithoutCancel(ctx), c, st)
	if err := enableProfiling(ctx, c, st); err != nil {
		return 0, wrap("duckdb.Exec", err)
	}
	res, err := c.ExecContext(ctx, query, namedArgs(args))
	if err != nil {
		return 0, wrap("duckdb.Exec", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, wrap("duckdb.Exec", err)
	}
	return n, nil
}

// Describe lihat engine.Engine.
func (e *Engine) Describe(ctx context.Context, query string) (engine.StatementInfo, error) {
	c, err := e.conn(ctx)
	if err != nil {
		return engine.StatementInfo{}, wrap("duckdb.Describe", err)
	}
	defer func() { _ = c.Close() }()
	// Prepare, bukan PrepareContext: PrepareContext duckdb-go mengeksekusi
	// semua statement kecuali yang terakhir pada input multi-statement.
	ds, err := c.Prepare(query)
	if err != nil {
		return engine.StatementInfo{}, wrap("duckdb.Describe", err)
	}
	defer func() { _ = ds.Close() }()
	st := ds.(*duckdbdrv.Stmt)
	typ, err := st.StatementType()
	if err != nil {
		return engine.StatementInfo{}, wrap("duckdb.Describe", err)
	}
	info := engine.StatementInfo{NumParams: st.NumInput(), ReturnsRows: returnsRows(typ)}
	if typ == duckdbdrv.STATEMENT_TYPE_SELECT {
		info.Schema = e.selectSchema(ctx, query, info.NumParams)
	}
	return info, nil
}

// selectSchema menentukan schema Arrow hasil SELECT lewat LIMIT 0 dengan semua
// parameter NULL — jalur ekspor Arrow sama dengan eksekusi sebenarnya, jadi
// schema identik. duckdb-go belum membungkus duckdb_prepared_arrow_schema.
// Gagal → nil (schema opsional di Flight SQL), bukan error.
func (e *Engine) selectSchema(ctx context.Context, query string, numParams int) *arrow.Schema {
	q := strings.TrimRight(strings.TrimSpace(query), "; \t\r\n")
	rdr, err := e.Query(ctx, "SELECT * FROM (\n"+q+"\n) LIMIT 0", make([]any, numParams)...)
	if err != nil {
		return nil
	}
	defer rdr.Release()
	return rdr.Schema()
}

func returnsRows(t duckdbdrv.StmtType) bool {
	switch t {
	case duckdbdrv.STATEMENT_TYPE_SELECT, duckdbdrv.STATEMENT_TYPE_EXPLAIN,
		duckdbdrv.STATEMENT_TYPE_PRAGMA, duckdbdrv.STATEMENT_TYPE_CALL,
		duckdbdrv.STATEMENT_TYPE_RELATION, duckdbdrv.STATEMENT_TYPE_LOGICAL_PLAN:
		return true
	}
	return false
}

func namedArgs(args []any) []driver.NamedValue {
	nv := make([]driver.NamedValue, len(args))
	for i, a := range args {
		nv[i] = driver.NamedValue{Ordinal: i + 1, Value: a}
	}
	return nv
}

// Catalogs lihat engine.Engine.
func (e *Engine) Catalogs(ctx context.Context) (array.RecordReader, error) {
	return e.Query(ctx, `SELECT DISTINCT catalog_name FROM information_schema.schemata ORDER BY 1`)
}

// Schemas lihat engine.Engine.
func (e *Engine) Schemas(ctx context.Context, catalog, schemaPattern *string) (array.RecordReader, error) {
	return e.Query(ctx, `SELECT catalog_name, schema_name FROM information_schema.schemata
		WHERE ($1::VARCHAR IS NULL OR catalog_name = $1)
		  AND ($2::VARCHAR IS NULL OR schema_name LIKE $2)
		ORDER BY 1, 2`, deref(catalog), deref(schemaPattern))
}

// Tables lihat engine.Engine.
func (e *Engine) Tables(ctx context.Context, f engine.TableFilter) (array.RecordReader, error) {
	var types any // nil → semua
	if len(f.TableTypes) > 0 {
		types = f.TableTypes
	}
	return e.Query(ctx, `SELECT table_catalog, table_schema, table_name, table_type FROM information_schema.tables
		WHERE ($1::VARCHAR IS NULL OR table_catalog = $1)
		  AND ($2::VARCHAR IS NULL OR table_schema LIKE $2)
		  AND ($3::VARCHAR IS NULL OR table_name LIKE $3)
		  AND ($4::VARCHAR[] IS NULL OR list_contains($4, table_type))
		ORDER BY 1, 2, 3`, deref(f.Catalog), deref(f.SchemaPattern), deref(f.TablePattern), types)
}

// TableTypes lihat engine.Engine.
func (e *Engine) TableTypes(ctx context.Context) (array.RecordReader, error) {
	return e.Query(ctx, `SELECT table_type FROM (VALUES ('BASE TABLE'), ('VIEW'), ('LOCAL TEMPORARY')) t(table_type)`)
}

// TableSchema lihat engine.Engine.
func (e *Engine) TableSchema(ctx context.Context, catalog, schema, table string) (*arrow.Schema, error) {
	rdr, err := e.Query(ctx, "SELECT * FROM "+quoteIdent(catalog)+"."+quoteIdent(schema)+"."+quoteIdent(table)+" LIMIT 0")
	if err != nil {
		return nil, wrap("duckdb.TableSchema", err)
	}
	defer rdr.Release()
	return rdr.Schema(), nil
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// deref mengembalikan nil (bukan *string nil yang ter-box) agar dibind sebagai NULL.
func deref(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// Close menutup database (WAL di-flush ke file).
func (e *Engine) Close() error {
	if e.closed.Swap(true) {
		return nil
	}
	if err := e.connector.Close(); err != nil {
		return wrap("duckdb.Close", err)
	}
	return nil
}
