//go:build duckdb_arrow

package duckdb

import (
	"context"
	"strings"
	"testing"
)

func TestDeterministic(t *testing.T) {
	e := open(t, `ATTACH ':memory:' AS w; CREATE TABLE w.t (i INT, s VARCHAR);
		CREATE VIEW w.v AS SELECT i, random() AS r FROM w.t; CREATE MACRO w.m(x) AS x + now()::DATE::VARCHAR::INT`)
	for q, want := range map[string]bool{
		"SELECT sum(i), upper(s), repeat(s, 2) FROM w.t GROUP BY s": true,
		"SELECT row_number() OVER (ORDER BY i) FROM w.t":            false, // window: konservatif (urutan seri)
		"SELECT i FROM w.t WHERE i = $1":                            true,
		"SELECT random()":                                           false,
		"SELECT current_date":                                       false,
		"SELECT i FROM w.v":                                         false, // fungsi volatil di dalam view
		"SELECT w.m(i) FROM w.t":                                    false, // macro diekspansi
		"SELECT nextval('x')":                                       false,
		"SELECT * FROM missing":                                     false,
	} {
		if got, err := e.Deterministic(context.Background(), q); got != want || err != nil {
			t.Errorf("Deterministic(%q) = %v, %v; want %v", q, got, err, want)
		}
	}
}

func TestDataVersion(t *testing.T) {
	e := openLake(t, "CREATE TABLE lake.t (i INT)")
	ctx := context.Background()
	v1, ok, err := e.DataVersion(ctx, []string{"lake", "memory", "lake"})
	if err != nil || !ok || !strings.HasPrefix(v1, "lake=") || !strings.HasSuffix(v1, ",memory=local") {
		t.Fatalf("version %q %v %v", v1, ok, err)
	}
	if _, err := e.Exec(ctx, "INSERT INTO lake.t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if v2, _, _ := e.DataVersion(ctx, []string{"lake", "memory"}); v2 == v1 {
		t.Errorf("version unchanged after lake write: %s", v2)
	}
	if _, ok, _ := e.DataVersion(ctx, []string{"nope"}); ok {
		t.Error("unknown catalog must not be cacheable")
	}
}
