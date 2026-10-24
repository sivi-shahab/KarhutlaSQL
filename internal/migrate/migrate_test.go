//go:build duckdb_arrow

package migrate

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bankmega/karhutlasql/internal/engine/duckdb"
)

func TestApply(t *testing.T) {
	ctx := context.Background()
	eng, err := duckdb.Open(ctx, duckdb.Config{InitSQL: "ATTACH ':memory:' AS w"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })
	dir := t.TempDir()
	write := func(name, sql string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(sql), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	apply := func() ([]File, error) {
		files, err := Load(dir)
		if err != nil {
			return nil, err
		}
		return Apply(ctx, eng, "w", "test", files)
	}
	write("0001_nasabah.sql", "CREATE TABLE w.main.nasabah (id INT);")
	write("0002_kolom.sql", "ALTER TABLE w.main.nasabah ADD COLUMN nik VARCHAR;\nINSERT INTO w.main.nasabah VALUES (1, 'a');")
	write("README.md", "bukan migration")
	if done, err := apply(); err != nil || len(done) != 2 {
		t.Fatalf("first run: %v %v", done, err)
	}
	if done, err := apply(); err != nil || len(done) != 0 { // idempoten
		t.Fatalf("second run: %v %v", done, err)
	}
	// Gagal di tengah: tidak ada sisa (DDL & baris pelacak ikut batal).
	write("0003_rusak.sql", "CREATE TABLE w.main.sisa (i INT);\nSELECT * FROM tidak_ada;")
	if _, err := apply(); err == nil {
		t.Fatal("broken migration applied")
	}
	if _, err := eng.Query(ctx, "SELECT * FROM w.main.sisa"); err == nil {
		t.Error("partial migration left table behind")
	}
	write("0003_rusak.sql", "CREATE TABLE w.main.sisa (i INT);")
	if done, err := apply(); err != nil || len(done) != 1 {
		t.Fatalf("fixed migration: %v %v", done, err)
	}
	// File yang sudah diterapkan diubah → drift; versi lama disisipkan → drift.
	write("0001_nasabah.sql", "CREATE TABLE w.main.nasabah (id BIGINT);")
	if _, err := apply(); !errors.Is(err, ErrDrift) {
		t.Errorf("changed file: %v", err)
	}
	write("0001_nasabah.sql", "CREATE TABLE w.main.nasabah (id INT);")
	write("0000_awal.sql", "SELECT 1;")
	if _, err := apply(); !errors.Is(err, ErrDrift) {
		t.Errorf("inserted older version: %v", err)
	}
	if err := os.Remove(filepath.Join(dir, "0000_awal.sql")); err != nil {
		t.Fatal(err)
	}
	write("bad-name.sql", "SELECT 1;")
	if _, err := Load(dir); err == nil {
		t.Error("bad file name accepted")
	}
}
