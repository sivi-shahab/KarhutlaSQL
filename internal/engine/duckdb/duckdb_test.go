//go:build duckdb_arrow

package duckdb

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow/array"

	"github.com/bankmega/karhutlasql/internal/engine"
)

func open(t *testing.T, initSQL string) *Engine {
	t.Helper()
	// Unlocked: analisis fungsi file (guardrail: TestGuardrail). Streaming
	// aktif seperti server; jalur materialisasi: TestQuery_Materialized.
	e, err := Open(context.Background(), Config{InitSQL: initSQL, Unlocked: true, StreamingBufferSize: "4MB"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Streaming(); err != nil {
		t.Fatalf("streaming inactive: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

// rows menguras reader dan mengembalikan kolom pertama sebagai string per baris.
// String di-clone: ValueStr bisa menunjuk buffer Arrow (memori DuckDB) yang
// dibebaskan saat Release.
func rows(t *testing.T, rdr array.RecordReader, err error) []string {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	defer rdr.Release()
	var out []string
	for rdr.Next() {
		rec := rdr.RecordBatch()
		for i := 0; i < int(rec.NumRows()); i++ {
			out = append(out, strings.Clone(rec.Column(0).ValueStr(i)))
		}
	}
	if err := rdr.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestQuery(t *testing.T) {
	e := open(t, "")
	ctx := context.Background()
	tests := []struct {
		name, sql string
		args      []any
		want      []string
	}{
		{"select 42", "SELECT 42", nil, []string{"42"}},
		{"param", "SELECT $1::INTEGER + 1", []any{41}, []string{"42"}},
		{"range count", "SELECT count(*) FROM range(1000000)", nil, []string{"1000000"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rdr, err := e.Query(ctx, tt.sql, tt.args...)
			got := rows(t, rdr, err)
			if len(got) != len(tt.want) || got[0] != tt.want[0] {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestQuery_StreamsAllRows(t *testing.T) {
	e := open(t, "")
	rdr, err := e.Query(context.Background(), "SELECT * FROM range(1000000)")
	if got := len(rows(t, rdr, err)); got != 1000000 {
		t.Fatalf("rows = %d", got)
	}
}

func TestQuery_SyntaxError(t *testing.T) {
	e := open(t, "")
	if _, err := e.Query(context.Background(), "SELEC 1"); err == nil {
		t.Fatal("want error")
	}
}

func TestQuery_CancelStopsExecution(t *testing.T) {
	e := open(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	rdr, err := e.Query(ctx, "SELECT count(*) FROM range(1000000000000)")
	if err == nil {
		for rdr.Next() {
		}
		err = rdr.Err()
		rdr.Release()
	}
	if err == nil {
		t.Fatal("want cancellation error")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("cancel took %v", d)
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Logf("cancel error (not wrapped ctx error): %v", err)
	}
}

func TestExec(t *testing.T) {
	e := open(t, "")
	ctx := context.Background()
	if _, err := e.Exec(ctx, "CREATE TABLE t (i INTEGER)"); err != nil {
		t.Fatal(err)
	}
	n, err := e.Exec(ctx, "INSERT INTO t SELECT * FROM range(5)")
	if err != nil || n != 5 {
		t.Fatalf("insert n=%d err=%v", n, err)
	}
	// tabel terlihat dari koneksi lain (database instance sama)
	rdr, err := e.Query(ctx, "SELECT count(*) FROM t")
	if got := rows(t, rdr, err); got[0] != "5" {
		t.Fatalf("count = %v", got)
	}
}

func TestOpen_InitSQL(t *testing.T) {
	e := open(t, "CREATE TABLE init_t AS SELECT 7 AS v")
	rdr, err := e.Query(context.Background(), "SELECT v FROM init_t")
	if got := rows(t, rdr, err); got[0] != "7" {
		t.Fatalf("got %v", got)
	}
	if _, err := Open(context.Background(), Config{InitSQL: "NOT SQL"}); err == nil {
		t.Fatal("bad init sql must fail Open")
	}
}

func TestDescribe(t *testing.T) {
	e := open(t, "CREATE TABLE t (i INTEGER)")
	tests := []struct {
		sql  string
		want engine.StatementInfo
	}{
		{"SELECT 1", engine.StatementInfo{NumParams: 0, ReturnsRows: true}},
		{"SELECT $1, $2", engine.StatementInfo{NumParams: 2, ReturnsRows: true}},
		{"INSERT INTO t VALUES (?)", engine.StatementInfo{NumParams: 1, ReturnsRows: false}},
	}
	for _, tt := range tests {
		got, err := e.Describe(context.Background(), tt.sql)
		if err != nil || got.NumParams != tt.want.NumParams || got.ReturnsRows != tt.want.ReturnsRows {
			t.Errorf("%q: got %+v err %v, want %+v", tt.sql, got, err, tt.want)
		}
		if got.ReturnsRows != (got.Schema != nil) {
			t.Errorf("%q: schema %v", tt.sql, got.Schema)
		}
	}
	// Schema SELECT ber-parameter dengan trailing semicolon / komentar baris.
	for _, q := range []string{
		"SELECT i, i::VARCHAR AS s FROM t WHERE i = $1;",
		"SELECT i, i::VARCHAR AS s FROM t WHERE i = $1 -- komentar",
	} {
		got, err := e.Describe(context.Background(), q)
		if err != nil || got.Schema == nil || got.Schema.NumFields() != 2 || got.Schema.Field(1).Name != "s" {
			t.Errorf("%q: schema = %v err %v", q, got.Schema, err)
		}
	}
	// Describe tidak mengeksekusi
	if _, err := e.Describe(context.Background(), "INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	rdr, err := e.Query(context.Background(), "SELECT count(*) FROM t")
	if got := rows(t, rdr, err); got[0] != "0" {
		t.Fatalf("Describe executed the statement: count=%v", got)
	}
}

func TestMetadata(t *testing.T) {
	e := open(t, "CREATE SCHEMA s1; CREATE TABLE s1.orders (id INTEGER, note VARCHAR); CREATE VIEW s1.v AS SELECT 1 AS x")
	ctx := context.Background()
	mem, s1, ord := "memory", "s1", "ord%"

	rdr, err := e.Catalogs(ctx)
	if got := rows(t, rdr, err); !contains(got, "memory") {
		t.Errorf("catalogs %v", got)
	}
	rdr, err = e.Schemas(ctx, &mem, &s1)
	if got := rows(t, rdr, err); len(got) != 1 {
		t.Errorf("schemas %v", got)
	}
	rdr, err = e.Tables(ctx, engine.TableFilter{SchemaPattern: &s1, TablePattern: &ord})
	if got := rows(t, rdr, err); len(got) != 1 {
		t.Errorf("tables by pattern %v", got)
	}
	rdr, err = e.Tables(ctx, engine.TableFilter{SchemaPattern: &s1, TableTypes: []string{"VIEW"}})
	if got := rows(t, rdr, err); len(got) != 1 {
		t.Errorf("tables by type %v", got)
	}
	sch, err := e.TableSchema(ctx, "memory", "s1", "orders")
	if err != nil || sch.NumFields() != 2 || sch.Field(1).Name != "note" {
		t.Errorf("table schema %v err %v", sch, err)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

func TestStats(t *testing.T) {
	e := open(t, "CREATE TABLE t AS SELECT range i FROM range(100000)")
	run := func(q string) engine.Stats {
		var st engine.Stats
		ctx := engine.WithStats(context.Background(), &st)
		rdr, err := e.Query(ctx, q)
		rows(t, rdr, err)
		return st
	}
	a, b := run("SELECT i % 7, count(*) FROM t WHERE i > 5 GROUP BY 1"), run("SELECT i % 3, count(*) FROM t WHERE i > 9 GROUP BY 1")
	if a.PlanHash == "" || a.PlanHash != b.PlanHash || a.PeakMemory <= 0 {
		t.Errorf("same shape: %+v %+v", a, b)
	}
	if c := run("SELECT i FROM t ORDER BY i LIMIT 3"); c.PlanHash == a.PlanHash {
		t.Errorf("different shape, same hash %s", c.PlanHash)
	}
	var st engine.Stats
	if _, err := e.Exec(engine.WithStats(context.Background(), &st), "INSERT INTO t SELECT * FROM range(10)"); err != nil || st.PlanHash == "" {
		t.Errorf("exec stats %+v %v", st, err)
	}
}

func TestGuardrail(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir() + "/"
	e, err := Open(ctx, Config{AllowedDirectories: []string{dir}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close() }()
	for q, ok := range map[string]bool{
		"COPY (SELECT 1) TO '" + dir + "x.csv'":          true,
		"COPY (SELECT 1) TO '/tmp/karhutla-outside.csv'": false,
		"SELECT * FROM read_csv('/etc/passwd')":          false,
		"ATTACH '/tmp/karhutla-outside.duckdb' AS z":     false,
		"INSTALL httpfs": false,
		"CREATE PERSISTENT SECRET p (TYPE s3, KEY_ID 'a', SECRET 'b')": false,
		"SET enable_external_access = true":                            false,
		"SET allowed_directories = ['/']":                              false,
	} {
		_, err := e.Exec(ctx, q)
		if (err == nil) != ok {
			t.Errorf("%s: err = %v", q, err)
		}
		if err != nil && strings.HasPrefix(q, "COPY") && !errors.Is(err, engine.ErrPermission) {
			t.Errorf("%s: not ErrPermission: %v", q, err)
		}
	}
}

func TestSecretSQL(t *testing.T) {
	allowed := []string{"s3://lake/"}
	q, err := secretSQL(Secret{Name: "lake", Type: "s3", Scope: "s3://lake/raw/", Options: map[string]string{"secret": "x'y", "key_id": "k"}}, allowed)
	if err != nil || q != `CREATE TEMPORARY SECRET "lake" (TYPE "s3", SCOPE 's3://lake/raw/', "key_id" 'k', "secret" 'x''y')` {
		t.Fatalf("%s %v", q, err)
	}
	for _, s := range []Secret{
		{Name: "x", Type: "s3", Scope: "s3://other/"},
		{Name: "x", Type: "s3", Scope: ""},
		{Name: "x", Type: "s3", Scope: "s3://lake/", Options: map[string]string{"PERSISTENT": "true"}},
	} {
		if _, err := secretSQL(s, allowed); err == nil {
			t.Errorf("%+v accepted", s)
		}
	}
}

func TestSnapshot(t *testing.T) {
	e := open(t, "CREATE TABLE t AS SELECT 1 AS i")
	ctx := context.Background()
	sn, err := e.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	count := func(q func(context.Context, string, ...any) (array.RecordReader, error)) []string {
		rdr, err := q(ctx, "SELECT count(*) FROM t")
		return rows(t, rdr, err)
	}
	before := count(sn.Query)
	if _, err := e.Exec(ctx, "INSERT INTO t VALUES (2)"); err != nil {
		t.Fatal(err)
	}
	if got := count(sn.Query); got[0] != before[0] || before[0] != "1" {
		t.Errorf("snapshot sees concurrent insert: %v → %v", before, got)
	}
	if err := sn.Close(); err != nil {
		t.Fatal(err)
	}
	if got := count(e.Query); got[0] != "2" {
		t.Errorf("after close: %v", got)
	}
}
