package sqlrewrite

import (
	"reflect"
	"testing"
)

func TestWriteTargetCatalogs(t *testing.T) {
	all := []string{"memory", "warehouse", "sandbox", "temp", "system"}
	tests := []struct {
		sql  string
		want []string // nil = error
	}{
		{"INSERT INTO sandbox.s VALUES (1)", []string{"sandbox"}},
		{"INSERT INTO sandbox.main.s SELECT * FROM warehouse.t", []string{"sandbox"}},
		{"INSERT INTO s VALUES (1)", []string{"memory"}},
		{`INSERT INTO "Sandbox".main.s VALUES (1)`, []string{"sandbox"}},
		{"INSERT INTO myschema.s VALUES (1)", []string{"memory"}}, // bukan catalog → schema di default
		{"UPDATE warehouse.main.t SET i = 1", []string{"warehouse"}},
		{"DELETE FROM sandbox.main.s USING warehouse.main.t", []string{"sandbox"}},
		{"CREATE TABLE sandbox.main.x AS SELECT 1", []string{"sandbox"}},
		{"CREATE TABLE sandbox.main.y (i INT)", []string{"sandbox"}},
		{"CREATE TEMP TABLE t (i INT)", []string{"temp"}},
		{"CREATE VIEW sandbox.main.v AS SELECT 1", []string{"sandbox"}},
		{"DROP TABLE sandbox.main.s, warehouse.main.t", []string{"sandbox", "warehouse"}},
		{"DROP SCHEMA x", []string{"memory"}},
		{"CREATE SCHEMA x", []string{"memory"}},
		{"ALTER TABLE warehouse.main.t ADD COLUMN j INT", []string{"warehouse"}},
		{"ALTER TABLE sandbox.main.s RENAME TO s2", []string{"sandbox"}},
		{"TRUNCATE sandbox.main.s", []string{"sandbox"}},
		{"CREATE INDEX i ON warehouse.main.t (i)", []string{"warehouse"}},
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
			cats, cerr := tg.Catalogs("memory", all)
			if cerr != nil {
				err = cerr
				break
			}
			got = append(got, cats...)
		}
		if (err != nil) != (tt.want == nil) || (err == nil && !reflect.DeepEqual(got, tt.want)) {
			t.Errorf("%s → %v err %v, want %v", tt.sql, got, err, tt.want)
		}
	}
}
