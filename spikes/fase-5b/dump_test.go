//go:build duckdb_arrow

package spike

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/bankmega/karhutlasql/internal/engine/duckdb"
)

// TestDumpTPCH menulis tests/data/queries/01..22.sql dari ekstensi tpch (sekali).
func TestDumpTPCH(t *testing.T) {
	if os.Getenv("DUMP_TPCH") == "" {
		t.Skip("set DUMP_TPCH=1")
	}
	ctx := context.Background()
	e, _ := duckdb.Open(ctx, duckdb.Config{})
	if _, err := e.Exec(ctx, "LOAD tpch"); err != nil {
		t.Fatal(err)
	}
	rdr, err := e.Query(ctx, "SELECT query_nr, query FROM tpch_queries() ORDER BY 1")
	if err != nil {
		t.Fatal(err)
	}
	defer rdr.Release()
	for rdr.Next() {
		rec := rdr.RecordBatch()
		for i := range int(rec.NumRows()) {
			q := strings.TrimSpace(strings.Clone(rec.Column(1).ValueStr(i)))
			if err := os.WriteFile(fmt.Sprintf("../../tests/data/queries/%02s.sql", rec.Column(0).ValueStr(i)), []byte(q+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
}
