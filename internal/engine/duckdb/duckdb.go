//go:build duckdb_arrow

// Package duckdb mengimplementasikan engine.Engine di atas DuckDB (duckdb-go, Arrow interface).
package duckdb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
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
}

// Engine adalah engine.Engine berbasis DuckDB. Setiap statement memakai koneksi
// sendiri dari connector (semua koneksi berbagi satu database instance), sehingga
// cancel satu statement tidak memengaruhi statement lain.
type Engine struct {
	connector *duckdbdrv.Connector
	closed    atomic.Bool
}

var _ engine.Engine = (*Engine)(nil)

// Open membuka database dan menjalankan InitSQL.
func Open(ctx context.Context, cfg Config) (*Engine, error) {
	c, err := duckdbdrv.NewConnector(cfg.Path, nil)
	if err != nil {
		return nil, fmt.Errorf("duckdb.Open: %w", err)
	}
	e := &Engine{connector: c}
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
	return e, nil
}

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
		return nil, fmt.Errorf("duckdb.Query: %w", err)
	}
	ar, err := duckdbdrv.NewArrowFromConn(c)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("duckdb.Query: %w", err), c.Close())
	}
	rdr, err := ar.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("duckdb.Query: %w", err), c.Close())
	}
	return &connReader{RecordReader: rdr, conn: c}, nil
}

// connReader menutup koneksi saat reader dilepas terakhir kali.
type connReader struct {
	array.RecordReader
	conn *duckdbdrv.Conn
	refs atomic.Int64 // tambahan di atas referensi awal
}

func (r *connReader) Retain() { r.refs.Add(1); r.RecordReader.Retain() }

func (r *connReader) Release() {
	r.RecordReader.Release()
	if r.refs.Add(-1) < 0 {
		_ = r.conn.Close()
	}
}

// Exec lihat engine.Engine.
func (e *Engine) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	c, err := e.conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("duckdb.Exec: %w", err)
	}
	defer func() { _ = c.Close() }()
	res, err := c.ExecContext(ctx, query, namedArgs(args))
	if err != nil {
		return 0, fmt.Errorf("duckdb.Exec: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("duckdb.Exec: %w", err)
	}
	return n, nil
}

// Describe lihat engine.Engine.
func (e *Engine) Describe(ctx context.Context, query string) (engine.StatementInfo, error) {
	c, err := e.conn(ctx)
	if err != nil {
		return engine.StatementInfo{}, fmt.Errorf("duckdb.Describe: %w", err)
	}
	defer func() { _ = c.Close() }()
	// Prepare, bukan PrepareContext: PrepareContext duckdb-go mengeksekusi
	// semua statement kecuali yang terakhir pada input multi-statement.
	ds, err := c.Prepare(query)
	if err != nil {
		return engine.StatementInfo{}, fmt.Errorf("duckdb.Describe: %w", err)
	}
	defer func() { _ = ds.Close() }()
	st := ds.(*duckdbdrv.Stmt)
	typ, err := st.StatementType()
	if err != nil {
		return engine.StatementInfo{}, fmt.Errorf("duckdb.Describe: %w", err)
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
		return nil, fmt.Errorf("duckdb.TableSchema: %w", err)
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
		return fmt.Errorf("duckdb.Close: %w", err)
	}
	return nil
}
