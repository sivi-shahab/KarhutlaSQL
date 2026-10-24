//go:build duckdb_arrow

package duckdb

import (
	"context"
	"errors"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"

	"github.com/bankmega/karhutlasql/internal/engine"
)

func batches(t *testing.T, n, rows int) array.RecordReader {
	t.Helper()
	sch := arrow.NewSchema([]arrow.Field{{Name: "id", Type: arrow.PrimitiveTypes.Int64}, {Name: "nama", Type: arrow.BinaryTypes.String}}, nil)
	var recs []arrow.RecordBatch
	for b := range n {
		ib, sb := array.NewInt64Builder(memory.DefaultAllocator), array.NewStringBuilder(memory.DefaultAllocator)
		for i := range rows {
			ib.Append(int64(b*rows + i))
			sb.Append("n")
		}
		cols := []arrow.Array{ib.NewArray(), sb.NewArray()}
		recs = append(recs, array.NewRecordBatch(sch, cols, int64(rows)))
		cols[0].Release()
		cols[1].Release()
		ib.Release()
		sb.Release()
	}
	r, err := array.NewRecordReader(sch, recs)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestIngest(t *testing.T) {
	ctx := context.Background()
	// Guardrail aktif seperti server (Arrow view tidak butuh akses eksternal).
	e, err := Open(ctx, Config{InitSQL: "ATTACH ':memory:' AS w", StreamingBufferSize: "4MB"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	tgt := engine.IngestTarget{Catalog: "w", Schema: "main", Table: "t", IfNotExists: engine.IngestCreate, IfExists: engine.IngestAppend}
	count := func() string {
		r, err := e.Query(ctx, "SELECT count(*)::VARCHAR FROM w.main.t")
		return rows(t, r, err)[0]
	}
	if n, err := e.Ingest(ctx, tgt, batches(t, 3, 1000)); err != nil || n != 3000 || count() != "3000" {
		t.Fatalf("create: %d %v %s", n, err, count())
	}
	if n, err := e.Ingest(ctx, tgt, batches(t, 1, 10)); err != nil || n != 10 || count() != "3010" {
		t.Fatalf("append: %d %v", n, err)
	}
	tgt.IfExists = engine.IngestReplace
	if _, err := e.Ingest(ctx, tgt, batches(t, 1, 5)); err != nil || count() != "5" {
		t.Fatalf("replace: %v %s", err, count())
	}
	tgt.IfExists = engine.IngestFail
	if _, err := e.Ingest(ctx, tgt, batches(t, 1, 5)); !errors.Is(err, engine.ErrIngestTarget) {
		t.Errorf("fail if exists: %v", err)
	}
	tgt.Table, tgt.IfNotExists = "missing", engine.IngestFail
	if _, err := e.Ingest(ctx, tgt, batches(t, 1, 5)); !errors.Is(err, engine.ErrIngestTarget) {
		t.Errorf("fail if not exists: %v", err)
	}
	// Kolom tidak cocok → seluruh batch ditolak, tabel tetap.
	tgt.Table, tgt.IfExists = "t", engine.IngestAppend
	sch := arrow.NewSchema([]arrow.Field{{Name: "lain", Type: arrow.PrimitiveTypes.Int64}}, nil)
	bad, _ := array.NewRecordReader(sch, nil)
	if _, err := e.Ingest(ctx, tgt, bad); err == nil || count() != "5" {
		t.Errorf("mismatched columns: %v %s", err, count())
	}
}
