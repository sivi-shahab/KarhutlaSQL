package sqlrewrite

import (
	"reflect"
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
