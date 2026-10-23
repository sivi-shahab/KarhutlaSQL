//go:build duckdb_arrow

// Spike Fase 5b: coverage parser untuk rewrite SELECT (ADR 24).
// go test -tags duckdb_arrow -v ./spikes/fase-5b/
package spike

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	pg "github.com/pganalyze/pg_query_go/v6"

	"github.com/bankmega/karhutlasql/internal/engine/duckdb"
)

const tpch = "../../tests/data/tpch_sf0_01.duckdb"

func rowsOf(t *testing.T, e *duckdb.Engine, q string, args ...any) ([]string, error) {
	rdr, err := e.Query(context.Background(), q, args...)
	if err != nil {
		return nil, err
	}
	defer rdr.Release()
	var out []string
	for rdr.Next() {
		rec := rdr.RecordBatch()
		for r := range int(rec.NumRows()) {
			var b strings.Builder
			for _, c := range rec.Columns() {
				b.WriteString(strings.Clone(c.ValueStr(r)) + "|")
			}
			out = append(out, b.String())
		}
	}
	slices.Sort(out)
	return out, rdr.Err()
}

func one(t *testing.T, e *duckdb.Engine, q string, args ...any) (string, error) {
	r, err := rowsOf(t, e, q, args...)
	if err != nil || len(r) != 1 {
		return "", fmt.Errorf("%w (%d rows)", err, len(r))
	}
	return strings.TrimSuffix(r[0], "|"), nil
}

func TestParserCoverage(t *testing.T) {
	ctx := context.Background()
	src, err := os.ReadFile(tpch)
	if err != nil {
		t.Skip("tests/data/tpch_sf0_01.duckdb tidak ada: make testdata")
	}
	db := filepath.Join(t.TempDir(), "tpch.duckdb")
	if err := os.WriteFile(db, src, 0o600); err != nil {
		t.Fatal(err)
	}
	e, err := duckdb.Open(ctx, duckdb.Config{Path: db})
	if err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob("../../tests/data/queries/*.sql")
	var pgOK, pgSame, jsOK, jsSame, n int
	for _, f := range files {
		b, _ := os.ReadFile(f)
		q := strings.TrimSpace(string(b))
		n++
		param := strings.Contains(q, "$1") || strings.Contains(q, "?")
		var args []any
		if param {
			args = []any{int64(3)}
		}
		want, werr := rowsOf(t, e, q, args...)
		if werr != nil {
			t.Fatalf("%s: original fails: %v", f, werr)
		}
		same := func(sql string) bool {
			got, err := rowsOf(t, e, sql, args...)
			return err == nil && slices.Equal(got, want)
		}
		// pg_query: parse + deparse dialek Postgres.
		pgRes := "parse-fail"
		if tree, err := pg.Parse(q); err == nil {
			pgOK++
			pgRes = "deparse-fail"
			if out, err := pg.Deparse(tree); err == nil {
				pgRes = "diff"
				if same(out) {
					pgSame++
					pgRes = "ok"
				}
			}
		}
		// DuckDB: json_serialize_sql → json_deserialize_sql.
		jsRes := "parse-fail"
		js, err := one(t, e, "SELECT json_serialize_sql($1::VARCHAR)", q)
		if err == nil && strings.Contains(js, `"error":false`) {
			jsOK++
			jsRes = "deparse-fail"
			if out, err := one(t, e, "SELECT json_deserialize_sql($1::JSON)", js); err == nil {
				jsRes = "diff"
				if same(out) {
					jsSame++
					jsRes = "ok"
				}
			}
		}
		t.Logf("%-8s pg=%-12s duckdb=%-12s %.60s", filepath.Base(f), pgRes, jsRes, strings.ReplaceAll(q, "\n", " "))
	}
	t.Logf("TOTAL %d | pg_query parse %d, round-trip same result %d | duckdb json parse %d, round-trip same result %d", n, pgOK, pgSame, jsOK, jsSame)
}
