package verified

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "v.yaml")
		if err := os.WriteFile(p, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	s, err := Load(write(`
semantic_model: |
  tables: [warehouse.main.nasabah]
verified_queries:
  nasabah_per_cabang:
    question: Berapa jumlah nasabah per cabang?
    sql: SELECT branch_code, count(*) FROM warehouse.main.nasabah GROUP BY 1
    verified_by: dian@bankmega.com
    verified_at: 2026-10-01T00:00:00Z
`))
	if err != nil || len(s.List()) != 1 || s.List()[0].Name != "nasabah_per_cabang" || s.SemanticModel == "" {
		t.Fatalf("%+v %v", s, err)
	}
	for _, bad := range []string{
		"verified_queries: {X: {question: q, sql: s, verified_by: a, verified_at: 2026-10-01T00:00:00Z}}",
		"verified_queries: {x: {question: q, sql: s, verified_at: 2026-10-01T00:00:00Z}}",
		"verified_queries: {x: {question: q, sql: s, verified_by: a, verified_at: 2026-10-01T00:00:00Z, extra: 1}}",
	} {
		if _, err := Load(write(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if s, err := Load(""); err != nil || len(s.List()) != 0 {
		t.Errorf("empty: %v", err)
	}
}
