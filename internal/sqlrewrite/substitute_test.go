//go:build duckdb_arrow

package sqlrewrite

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/bankmega/karhutlasql/internal/engine/duckdb"
)

var update = flag.Bool("update", false, "tulis ulang testdata/*.golden")

func codec(t *testing.T) *duckdb.Engine {
	t.Helper()
	e, err := duckdb.Open(context.Background(), duckdb.Config{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func TestSubstitute_Golden(t *testing.T) {
	e := codec(t)
	names := Names{DefaultCatalog: "memory", Catalogs: []string{"memory", "warehouse"}}
	subs := map[string]string{"warehouse.main.nasabah": "SELECT 'x' AS nik FROM warehouse.main.nasabah WHERE ('N0' = 'N0')"}
	cases := []struct {
		sql   string
		count int
	}{
		{"SELECT nik FROM warehouse.nasabah", 1},
		{"SELECT n.nik FROM warehouse.main.nasabah AS n WHERE n.nik = '1'", 1},
		{"FROM warehouse.nasabah SELECT *", 1},
		{"SELECT * FROM warehouse.nasabah a JOIN warehouse.nasabah b ON a.nik = b.nik", 2},
		{"SELECT 1 WHERE EXISTS (SELECT 1 FROM warehouse.nasabah)", 1},
		{"SELECT nik FROM warehouse.nasabah UNION ALL SELECT nik FROM warehouse.nasabah", 2},
		{"WITH c AS (SELECT * FROM warehouse.nasabah) SELECT * FROM c", 1},
		{"WITH nasabah AS (SELECT 1 AS nik) SELECT * FROM nasabah", 0}, // CTE, bukan tabel
		{"SELECT * FROM warehouse.nasabah t(a)", 1},
		{"SELECT * FROM memory.main.other", 0},
		{"SELECT * FROM warehouse.nasabah AT (VERSION => 1)", 1}, // AT pindah ke scan di subquery
	}
	var golden strings.Builder
	for _, c := range cases {
		out, n, err := Substitute(context.Background(), e, c.sql, names, subs)
		if err != nil || n != c.count {
			t.Errorf("%s: n=%d err=%v", c.sql, n, err)
			continue
		}
		fmt.Fprintf(&golden, "-- in\n%s\n-- out\n%s\n\n", c.sql, out)
	}
	path := "testdata/substitute.golden"
	if *update {
		if err := os.WriteFile(path, []byte(golden.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (jalankan dengan -update)", err)
	}
	if golden.String() != string(want) {
		t.Errorf("golden mismatch; jalankan go test -tags duckdb_arrow ./internal/sqlrewrite -update dan tinjau diff\n%s", golden.String())
	}

	for _, bad := range []string{
		"PIVOT warehouse.nasabah ON nik USING count(*)",
		"INSERT INTO x SELECT * FROM warehouse.nasabah",
	} {
		if _, _, err := Substitute(context.Background(), e, bad, names, subs); !errors.Is(err, ErrRewrite) {
			t.Errorf("%s: err = %v, want ErrRewrite", bad, err)
		}
	}
}

func TestTimeTravel(t *testing.T) {
	e := codec(t)
	ctx := context.Background()
	cases := map[string]string{
		"SELECT * FROM lake.kredit k JOIN lake.s.nasabah USING (id)": "SELECT * FROM lake.kredit AS k AT (VERSION => 7) INNER JOIN lake.s.nasabah AT (VERSION => 7) USING (id)",
		"WITH c AS (SELECT * FROM lake.kredit) SELECT * FROM c":      "WITH c AS (SELECT * FROM lake.kredit AT (VERSION => 7))SELECT * FROM c",
		"SELECT 1 WHERE EXISTS (SELECT 1 FROM t)":                    "SELECT 1 WHERE EXISTS(SELECT 1 FROM t AT (VERSION => 7))",
	}
	for in, want := range cases {
		got, err := TimeTravel(ctx, e, in, "VERSION => 7")
		if err != nil || got != want {
			t.Errorf("%s:\n got %q, %v\nwant %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"SELECT * FROM t AT (VERSION => 1)", "SELECT 1; SELECT 2"} {
		if _, err := TimeTravel(ctx, e, bad, "VERSION => 7"); !errors.Is(err, ErrRewrite) {
			t.Errorf("%s: err = %v, want ErrRewrite", bad, err)
		}
	}
	if _, err := TimeTravel(ctx, e, "SELECT 1 FROM t", "VERSION => 1) UNION SELECT 2 FROM (t"); !errors.Is(err, ErrRewrite) {
		t.Errorf("injected AT: err = %v, want ErrRewrite", err)
	}
}
