// Package migrate schema migration berversi (Fase 8d): file
// <versi>_<nama>.sql di satu direktori diterapkan berurutan saat server
// start, masing-masing dalam satu transaksi bersama baris pelacaknya di
// <catalog>.main.karhutla_schema_migrations. File yang sudah diterapkan tidak
// boleh berubah (sha256) dan versi tidak boleh disisipkan di bawah versi
// yang sudah diterapkan.
package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/apache/arrow-go/v18/arrow/array"

	"github.com/bankmega/karhutlasql/internal/engine"
)

// File satu migration.
type File struct {
	Version int64
	Name    string
	SQL     string
	SHA256  string
}

var filePattern = regexp.MustCompile(`^(\d+)_([a-z0-9_]+)\.sql$`)

// Load membaca dir (file lain diabaikan; nama .sql tidak sesuai pola = error).
func Load(dir string) ([]File, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("migrate.Load: %w", err)
	}
	var out []File
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		m := filePattern.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migrate.Load: %s: want <version>_<name>.sql (name [a-z0-9_])", e.Name())
		}
		v, _ := strconv.ParseInt(m[1], 10, 64) // \d+: hanya overflow
		b, err := os.ReadFile(filepath.Clean(filepath.Join(dir, e.Name())))
		if err != nil {
			return nil, fmt.Errorf("migrate.Load: %w", err)
		}
		sum := sha256.Sum256(b)
		out = append(out, File{Version: v, Name: m[2], SQL: string(b), SHA256: hex.EncodeToString(sum[:])})
	}
	slices.SortFunc(out, func(a, b File) int { return int(a.Version - b.Version) })
	for i := 1; i < len(out); i++ {
		if out[i].Version == out[i-1].Version {
			return nil, fmt.Errorf("migrate.Load: duplicate version %d", out[i].Version)
		}
	}
	return out, nil
}

// ErrDrift migration yang sudah diterapkan berubah / urutan tidak valid.
var ErrDrift = errors.New("schema migration drift")

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func lit(s string) string   { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// Apply menerapkan files yang belum tercatat di catalog; hasil = yang baru
// diterapkan. by dicatat sebagai applied_by.
func Apply(ctx context.Context, eng engine.Engine, catalog, by string, files []File) ([]File, error) {
	table := quote(catalog) + ".main.karhutla_schema_migrations"
	if _, err := eng.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+table+
		" (version BIGINT, name VARCHAR, sha256 VARCHAR, applied_at TIMESTAMPTZ, applied_by VARCHAR)"); err != nil {
		return nil, fmt.Errorf("migrate.Apply: %w", err)
	}
	applied, err := appliedSums(ctx, eng, table)
	if err != nil {
		return nil, err
	}
	var maxApplied int64 = -1
	for v := range applied {
		maxApplied = max(maxApplied, v)
	}
	var done []File
	for _, f := range files {
		if sum, ok := applied[f.Version]; ok {
			if sum != f.SHA256 {
				return done, fmt.Errorf("migrate.Apply: %w: %d_%s.sql changed after it was applied (sha256 %s, applied %s)",
					ErrDrift, f.Version, f.Name, f.SHA256, sum)
			}
			continue
		}
		if f.Version < maxApplied {
			return done, fmt.Errorf("migrate.Apply: %w: %d_%s.sql is older than applied version %d", ErrDrift, f.Version, f.Name, maxApplied)
		}
		// Satu Exec = satu koneksi: gagal di tengah → transaksi tidak di-commit.
		q := "BEGIN TRANSACTION;\n" + f.SQL + "\n;\nINSERT INTO " + table + " VALUES (" + strconv.FormatInt(f.Version, 10) + ", " +
			lit(f.Name) + ", " + lit(f.SHA256) + ", now(), " + lit(by) + ");\nCOMMIT;"
		if _, err := eng.Exec(ctx, q); err != nil {
			return done, fmt.Errorf("migrate.Apply: %d_%s.sql: %w", f.Version, f.Name, err)
		}
		done = append(done, f)
		maxApplied = f.Version
	}
	return done, nil
}

func appliedSums(ctx context.Context, eng engine.Engine, table string) (map[int64]string, error) {
	rdr, err := eng.Query(ctx, "SELECT version, sha256 FROM "+table)
	if err != nil {
		return nil, fmt.Errorf("migrate.Apply: %w", err)
	}
	defer rdr.Release()
	out := map[int64]string{}
	for rdr.Next() {
		rec := rdr.RecordBatch()
		v, s := rec.Column(0).(*array.Int64), rec.Column(1) //nolint:forcetypeassert // kolom BIGINT tabel sendiri
		for i := range int(rec.NumRows()) {
			out[v.Value(i)] = strings.Clone(s.ValueStr(i))
		}
	}
	return out, rdr.Err()
}
