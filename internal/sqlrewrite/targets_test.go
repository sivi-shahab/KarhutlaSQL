package sqlrewrite

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestWriteTargetObjects(t *testing.T) {
	names := Names{DefaultCatalog: "memory", Catalogs: []string{"memory", "warehouse", "sandbox", "temp", "system"}}
	tests := []struct {
		sql  string
		want []string // nil = error
	}{
		{"INSERT INTO sandbox.s VALUES (1)", []string{"sandbox.main.s"}},
		{"INSERT INTO sandbox.main.s SELECT * FROM warehouse.t", []string{"sandbox.main.s"}},
		{"INSERT INTO s VALUES (1)", []string{"memory.main.s"}},
		{`INSERT INTO "Sandbox".main.s VALUES (1)`, []string{"sandbox.main.s"}},
		{"INSERT INTO myschema.s VALUES (1)", []string{"memory.myschema.s"}}, // bukan catalog → schema di default
		{"UPDATE warehouse.main.t SET i = 1", []string{"warehouse.main.t"}},
		{"DELETE FROM sandbox.main.s USING warehouse.main.t", []string{"sandbox.main.s"}},
		{"CREATE TABLE sandbox.main.x AS SELECT 1", []string{"sandbox.main.x"}},
		{"CREATE TABLE sandbox.main.y (i INT)", []string{"sandbox.main.y"}},
		{"CREATE TEMP TABLE t (i INT)", []string{"temp.main.t"}},
		{"CREATE VIEW sandbox.main.v AS SELECT 1", []string{"sandbox.main.v"}},
		{"DROP TABLE sandbox.main.s, warehouse.main.t", []string{"sandbox.main.s", "warehouse.main.t"}},
		{"DROP SCHEMA x", []string{"memory.x"}},
		{"CREATE SCHEMA x", []string{"memory.x"}},
		{"ALTER TABLE warehouse.main.t ADD COLUMN j INT", []string{"warehouse.main.t"}},
		{"ALTER TABLE sandbox.main.s RENAME TO s2", []string{"sandbox.main.s"}},
		{"TRUNCATE sandbox.main.s", []string{"sandbox.main.s"}},
		{"CREATE INDEX i ON warehouse.main.t (i)", []string{"warehouse.main.t"}},
		{"INSERT INTO sandbox.main.s FROM warehouse.t", nil}, // sintaks khusus DuckDB
		{"INSERT INTO a.b.c.d VALUES (1)", nil},
		{"DROP TABLE a, b.c.d.e", nil}, // ditemukan FuzzWriteTargets
		{"DROP FUNCTION f", nil},
		{"SELECT 1; DELETE FROM x", nil},
		{"GRANT SELECT ON t TO u", nil},
	}
	for _, tt := range tests {
		targets, err := WriteTargets(tt.sql)
		var got []string
		for _, tg := range targets {
			obj, oerr := tg.Object(names)
			if oerr != nil {
				err = oerr
				break
			}
			got = append(got, obj)
		}
		if (err != nil) != (tt.want == nil) || (err == nil && !reflect.DeepEqual(got, tt.want)) {
			t.Errorf("%s → %v err %v, want %v", tt.sql, got, err, tt.want)
		}
	}
}

// FuzzWriteTargets: extractor target tulis tidak pernah panic, dan bila
// berhasil setiap target punya 1–3 bagian non-kosong yang resolve ke nama
// objek lengkap (catalog.schema[.table]).
func FuzzWriteTargets(f *testing.F) {
	for _, q := range []string{
		"INSERT INTO sandbox.main.s VALUES (1)", "UPDATE w.t SET i = 1", "DELETE FROM t USING u",
		"CREATE TABLE \"A\".\"B\".\"C\" AS SELECT 1", "CREATE TEMP TABLE t (i INT)", "DROP TABLE a, b.c, d.e.f",
		"DROP SCHEMA s CASCADE", "ALTER TABLE x RENAME TO y", "TRUNCATE a, b", "MERGE INTO t USING s ON true WHEN MATCHED THEN DELETE",
		"WITH x AS (DELETE FROM w.t RETURNING *) INSERT INTO s.t SELECT * FROM x", "CREATE VIEW v AS SELECT 1", "COPY t FROM 'f'",
	} {
		f.Add(q)
	}
	names := Names{DefaultCatalog: "memory", Catalogs: []string{"memory", "warehouse", "sandbox", "temp", "system"}}
	f.Fuzz(func(t *testing.T, q string) {
		targets, err := WriteTargets(q)
		if err != nil {
			return
		}
		if len(targets) == 0 {
			t.Fatalf("%q: no error and no targets", q)
		}
		for _, tg := range targets {
			if len(tg.Parts) == 0 || len(tg.Parts) > 3 || slices.Contains(tg.Parts, "") {
				t.Fatalf("%q: bad target %+v", q, tg)
			}
			obj, err := tg.Object(names)
			if err != nil {
				continue
			}
			want := 3
			if tg.Schema {
				want = 2
			}
			if n := len(strings.Split(obj, ".")); n < want {
				t.Fatalf("%q: object %q from %+v", q, obj, tg)
			}
		}
	})
}
