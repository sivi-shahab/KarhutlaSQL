//go:build duckdb_arrow

package duckdb

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/apache/arrow-go/v18/arrow/array"
	duckdbdrv "github.com/duckdb/duckdb-go/v2"
	"github.com/google/uuid"

	"github.com/bankmega/karhutlasql/internal/engine"
)

// Ingest lihat engine.Engine: rdr didaftarkan sebagai Arrow view pada satu
// koneksi lalu CREATE TABLE AS / INSERT BY NAME dalam satu transaksi (gagal
// di tengah = tidak ada baris yang masuk). Data dibaca bertahap dari rdr.
func (e *Engine) Ingest(ctx context.Context, t engine.IngestTarget, rdr array.RecordReader) (int64, error) {
	c, err := e.conn(ctx)
	if err != nil {
		return 0, wrap("duckdb.Ingest", err)
	}
	defer func() { _ = c.Close() }()
	ar, err := duckdbdrv.NewArrowFromConn(c)
	if err != nil {
		return 0, wrap("duckdb.Ingest", err)
	}
	cr := &countingReader{RecordReader: rdr}
	view := "karhutla_ingest_" + uuid.NewString()[:8]
	release, err := ar.RegisterView(cr, view)
	if err != nil {
		return 0, wrap("duckdb.Ingest", err)
	}
	defer release()

	name := quoteIdent(t.Catalog) + "." + quoteIdent(t.Schema) + "." + quoteIdent(t.Table)
	exec := func(q string, args ...any) error {
		_, err := c.ExecContext(ctx, q, namedArgs(args))
		return err
	}
	if err := exec("BEGIN TRANSACTION"); err != nil {
		return 0, wrap("duckdb.Ingest", err)
	}
	commit := false
	defer func() {
		if !commit {
			_ = exec("ROLLBACK")
		}
	}()
	rows, err := queryRows(ctx, c, "SELECT count(*)::VARCHAR FROM duckdb_tables() WHERE database_name = $1 AND schema_name = $2 AND table_name = $3",
		[]any{t.Catalog, t.Schema, t.Table})
	if err != nil {
		return 0, wrap("duckdb.Ingest", err)
	}
	exists := len(rows) == 1 && rows[0][0] != "0"
	var q string
	switch {
	case !exists && t.IfNotExists == engine.IngestFail:
		return 0, fmt.Errorf("duckdb.Ingest: %w: table %s does not exist", engine.ErrIngestTarget, name)
	case !exists:
		q = "CREATE TABLE " + name + " AS SELECT * FROM " + quoteIdent(view)
	case t.IfExists == engine.IngestFail:
		return 0, fmt.Errorf("duckdb.Ingest: %w: table %s already exists", engine.ErrIngestTarget, name)
	case t.IfExists == engine.IngestReplace:
		q = "CREATE OR REPLACE TABLE " + name + " AS SELECT * FROM " + quoteIdent(view)
	default:
		q = "INSERT INTO " + name + " BY NAME SELECT * FROM " + quoteIdent(view)
	}
	if err := exec(q); err != nil {
		return 0, wrap("duckdb.Ingest", err)
	}
	if err := exec("COMMIT"); err != nil {
		return 0, wrap("duckdb.Ingest", err)
	}
	commit = true
	return cr.rows.Load(), errors.Join(ctx.Err(), rdr.Err())
}

// countingReader menghitung baris yang dibaca DuckDB dari stream.
type countingReader struct {
	array.RecordReader
	rows atomic.Int64
}

func (r *countingReader) Next() bool {
	if !r.RecordReader.Next() {
		return false
	}
	r.rows.Add(r.RecordBatch().NumRows())
	return true
}
