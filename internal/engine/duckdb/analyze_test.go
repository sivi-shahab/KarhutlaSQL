//go:build duckdb_arrow

package duckdb

import (
	"context"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestAnalyze(t *testing.T) {
	e := open(t, `ATTACH ':memory:' AS warehouse; ATTACH ':memory:' AS sandbox;
		CREATE TABLE warehouse.t (i INTEGER); CREATE TABLE sandbox.s (i INTEGER);
		CREATE VIEW sandbox.v AS SELECT * FROM warehouse.t;`)
	tests := []struct {
		sql       string
		kind      string
		catalogs  []string
		functions []string
	}{
		{"SELECT 42", "SELECT", nil, nil},
		{"SELECT * FROM warehouse.t", "SELECT", []string{"warehouse"}, nil},
		{"FROM sandbox.s SELECT *", "SELECT", []string{"sandbox"}, nil},
		{"SELECT * FROM sandbox.v", "SELECT", []string{"warehouse"}, nil}, // view diekspansi ke tabel dasar
		{"SELECT * FROM warehouse.t WHERE i = $1", "SELECT", []string{"warehouse"}, nil},
		{"SELECT * FROM warehouse.t WHERE false", "SELECT", []string{"warehouse"}, nil},
		{"WITH c AS (SELECT * FROM warehouse.t) SELECT * FROM c, sandbox.s", "SELECT", []string{"warehouse", "sandbox"}, nil},
		{"SELECT (SELECT max(i) FROM warehouse.t) FROM range(3)", "SELECT", []string{"warehouse"}, []string{"RANGE"}},
		{"SELECT * FROM read_csv('/etc/passwd')", "SELECT", nil, []string{"READ_CSV"}},
		{"SELECT * FROM duckdb_tables()", "SELECT", nil, []string{"DUCKDB_TABLES"}},
		{"INSERT INTO sandbox.s SELECT * FROM warehouse.t", "INSERT", []string{"warehouse"}, nil},
		{"INSERT INTO sandbox.s VALUES (1)", "INSERT", nil, nil},
		{"UPDATE warehouse.t SET i = 1", "UPDATE", []string{"warehouse"}, nil},
		{"CREATE TABLE sandbox.x AS SELECT * FROM warehouse.t", "CREATE", []string{"warehouse"}, nil},
		{"DROP TABLE sandbox.s", "DROP", nil, nil},
		{"COPY warehouse.t TO '/tmp/x.csv'", "COPY", nil, nil},
		{"ATTACH ':memory:' AS z", "ATTACH", nil, nil},
		{"SET threads = 1", "SET", nil, nil},
		{"CALL range(3)", "CALL", nil, []string{"RANGE"}},
		{"EXPLAIN SELECT 1", "EXPLAIN", nil, nil},
	}
	for _, tt := range tests {
		a, err := e.Analyze(context.Background(), tt.sql)
		if err != nil {
			t.Errorf("Analyze(%q): %v", tt.sql, err)
			continue
		}
		slices.Sort(a.Catalogs)
		slices.Sort(tt.catalogs)
		if a.Kind != tt.kind || !reflect.DeepEqual(a.Catalogs, tt.catalogs) || !reflect.DeepEqual(a.Functions, tt.functions) {
			t.Errorf("Analyze(%q) = %s %v %v, want %s %v %v", tt.sql, a.Kind, a.Catalogs, a.Functions, tt.kind, tt.catalogs, tt.functions)
		}
		if a.DefaultCatalog != "memory" || !slices.Contains(a.AllCatalogs, "warehouse") {
			t.Errorf("catalogs = %s %v", a.DefaultCatalog, a.AllCatalogs)
		}
	}
	// Multi-statement ditolak tanpa mengeksekusi statement pertama.
	if _, err := e.Analyze(context.Background(), "DROP TABLE sandbox.s; SELECT 1"); err == nil {
		t.Fatal("multi-statement accepted")
	}
	if _, err := e.Describe(context.Background(), "DROP TABLE sandbox.s; SELECT 1"); err == nil {
		t.Fatal("Describe accepted multi-statement")
	}
	rdr, err := e.Query(context.Background(), "SELECT count(*) FROM sandbox.s")
	if got := rows(t, rdr, err); len(got) != 1 {
		t.Fatalf("sandbox.s dropped by analyze/describe: %v", got)
	}
}

// TestAnalyze_TPCH memastikan allowlist operator mencakup plan 22 query TPC-H
// (tidak ada operator yang salah terbaca sebagai table function).
func TestAnalyze_TPCH(t *testing.T) {
	e := open(t, "")
	ctx := context.Background()
	if _, err := e.Exec(ctx, "LOAD tpch; CALL dbgen(sf = 0)"); err != nil {
		t.Skipf("tpch extension unavailable: %v", err)
	}
	rdr, err := e.Query(ctx, "SELECT query FROM tpch_queries() ORDER BY query_nr")
	queries := rows(t, rdr, err)
	if len(queries) != 22 {
		t.Fatalf("tpch queries = %d", len(queries))
	}
	for i, q := range queries {
		a, err := e.Analyze(ctx, q)
		if err != nil {
			t.Errorf("Q%d: %v", i+1, err)
			continue
		}
		if len(a.Functions) != 0 || len(a.Catalogs) != 1 || a.Catalogs[0] != "memory" {
			t.Errorf("Q%d: catalogs %v functions %v\n%s", i+1, a.Catalogs, a.Functions, strings.TrimSpace(q)[:60])
		}
	}
}

// TestBinderCatalogOrSchema mengunci aturan binder DuckDB yang diandalkan
// sqlrewrite.Target.Catalogs: "x.t" → catalog x bila x ter-attach; bila x juga
// nama schema di catalog default → error ambigu (bukan salah tulis diam-diam).
func TestBinderCatalogOrSchema(t *testing.T) {
	e := open(t, `ATTACH ':memory:' AS sandbox; CREATE TABLE sandbox.main.s (i INT);
		CREATE SCHEMA other; CREATE TABLE other.s (i INT);`)
	ctx := context.Background()
	if _, err := e.Exec(ctx, "INSERT INTO sandbox.s VALUES (1)"); err != nil {
		t.Fatalf("catalog.table: %v", err)
	}
	if _, err := e.Exec(ctx, "INSERT INTO other.s VALUES (1)"); err != nil {
		t.Fatalf("schema.table: %v", err)
	}
	if _, err := e.Exec(ctx, "CREATE SCHEMA sandbox"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Exec(ctx, "INSERT INTO sandbox.s VALUES (1)"); err == nil || !strings.Contains(err.Error(), "mbiguous") {
		t.Fatalf("ambiguous catalog/schema: %v", err)
	}
}

func TestAnalyze_Columns(t *testing.T) {
	e := open(t, `ATTACH ':memory:' AS w; CREATE TABLE w.n (nik VARCHAR, nama VARCHAR, branch VARCHAR, saldo INT);
		INSERT INTO w.n VALUES ('1', 'a', '0123', 5)`)
	for _, tt := range []struct {
		sql  string
		want map[string][]string
	}{
		{"SELECT nama FROM w.n WHERE nik = 'tidak-ada'", map[string][]string{"w.main.n": {"nik", "nama"}}}, // filter & statistik tidak membuang kolom
		{"SELECT count(*) FROM w.n", nil},
		{"SELECT a.nama FROM w.n a JOIN w.n b ON a.nik = b.branch WHERE b.saldo > 1", map[string][]string{"w.main.n": {"nik", "nama", "branch", "saldo"}}},
		{"SELECT upper(nama) FROM (SELECT * FROM w.n) WHERE length(nik) > 3", map[string][]string{"w.main.n": {"nik", "nama"}}},
		{"SELECT 42", nil},
		{"SELECT nama FROM (SELECT * FROM w.n) WHERE nik IN (SELECT branch FROM w.n)", map[string][]string{"w.main.n": {"nik", "nama", "branch"}}},
	} {
		a, err := e.Analyze(context.Background(), tt.sql)
		if err != nil {
			t.Fatal(err)
		}
		for k := range a.Columns {
			slices.Sort(a.Columns[k])
		}
		for k := range tt.want {
			slices.Sort(tt.want[k])
		}
		if !reflect.DeepEqual(a.Columns, tt.want) {
			t.Errorf("%s: columns = %v, want %v", tt.sql, a.Columns, tt.want)
		}
	}
	// Analyze tidak boleh mengubah setting global (mis. disabled_optimizers
	// berlaku ke semua koneksi): Describe LIMIT 0 atas query panjang tetap instan.
	rdr, err := e.Query(context.Background(), "SELECT current_setting('disabled_optimizers')")
	if got := rows(t, rdr, err); len(got) != 1 || got[0] != "" {
		t.Errorf("disabled_optimizers = %v", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := e.Describe(ctx, "SELECT count(*) FROM range(1000000000000)"); err != nil {
		t.Errorf("describe long query: %v", err)
	}
}

func TestNormTable(t *testing.T) {
	for in, want := range map[string]string{
		"memory.main.n":         "memory.main.n",
		`"secret".main.k`:       "secret.main.k",
		`W."Select"."My ""T"""`: `w.select.my "t"`,
		`"a.b".main.t`:          `"a.b".main.t`, // titik di nama: tidak didukung, dibiarkan (prefix match)
	} {
		if got := normTable(in); got != want {
			t.Errorf("normTable(%q) = %q, want %q", in, got, want)
		}
	}
	e := open(t, `ATTACH ':memory:' AS secret; CREATE TABLE secret.main."select" (i INT)`)
	a, err := e.Analyze(context.Background(), `SELECT i FROM secret.main."select"`)
	if err != nil || !slices.Equal(a.Tables, []string{"secret.main.select"}) || !slices.Equal(a.Catalogs, []string{"secret"}) {
		t.Errorf("tables %v catalogs %v %v", a.Tables, a.Catalogs, err)
	}
}
