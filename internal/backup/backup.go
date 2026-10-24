// Package backup backup bertingkat (Fase 8d). Tingkat data: catalog DuckDB
// lokal di-EXPORT DATABASE (Parquet zstd + schema.sql + load.sql) dan
// diunggah ke arsip; catalog DuckLake dicatat ID snapshot-nya (time travel
// & retensi snapshot DuckLake; metadata Postgres dan bucket data di-backup
// infrastruktur). Tingkat waktu: monthly (tanggal 1 UTC), weekly (Minggu),
// selain itu daily — masing-masing prefix & retensi object-lock sendiri.
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/bankmega/karhutlasql/internal/engine"
	"github.com/bankmega/karhutlasql/internal/objstore"
	"github.com/bankmega/karhutlasql/internal/report"
)

// Prefix awalan key backup di arsip.
const Prefix = "backups/"

// Tiers retensi per tingkat.
type Tiers struct {
	Daily, Weekly, Monthly time.Duration
}

// Options backup.
type Options struct {
	Engine engine.Engine
	Store  objstore.FilePutter
	// StagingDir direktori lokal untuk EXPORT (harus di allowed_directories).
	StagingDir string
	Tiers      Tiers
	Signer     *report.Signer // nil = manifest tanpa tanda tangan
	Now        func() time.Time
}

// File satu file hasil export.
type File struct {
	Key    string `json:"key"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// Manifest backup.json (ditandatangani bila ada Signer).
type Manifest struct {
	ID            string           `json:"id"`
	Tier          string           `json:"tier"`
	StartedAt     time.Time        `json:"started_at"`
	FinishedAt    time.Time        `json:"finished_at"`
	Engine        string           `json:"engine"`
	Catalogs      []string         `json:"exported_catalogs"`
	LakeSnapshots map[string]int64 `json:"lake_snapshots,omitempty"`
	Files         []File           `json:"files"`
	KeyID         string           `json:"key_id,omitempty"`
	Prefix        string           `json:"prefix"`
}

// Tier tingkat untuk waktu t (UTC).
func Tier(t time.Time) string {
	t = t.UTC()
	switch {
	case t.Day() == 1:
		return "monthly"
	case t.Weekday() == time.Sunday:
		return "weekly"
	}
	return "daily"
}

func (o Options) retention(tier string) time.Duration {
	return map[string]time.Duration{"daily": o.Tiers.Daily, "weekly": o.Tiers.Weekly, "monthly": o.Tiers.Monthly}[tier]
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// Run satu backup. Gagal di tengah → objek yang sudah terunggah tetap ada
// tetapi tanpa backup.json (backup tidak lengkap mudah dikenali).
func (o Options) Run(ctx context.Context) (Manifest, error) {
	now := o.Now
	if now == nil {
		now = time.Now
	}
	start := now().UTC()
	m := Manifest{ID: start.Format("20060102T150405Z"), Tier: Tier(start), StartedAt: start, LakeSnapshots: map[string]int64{}}
	m.Prefix = Prefix + m.Tier + "/" + m.ID + "/"
	ret := o.retention(m.Tier)

	dbs, err := query(ctx, o.Engine, "SELECT database_name, type FROM duckdb_databases() WHERE NOT internal ORDER BY 1")
	if err != nil {
		return m, err
	}
	if v, err := query(ctx, o.Engine, "SELECT version()"); err == nil && len(v) == 1 {
		m.Engine = v[0][0]
	}
	stage := filepath.Join(o.StagingDir, m.ID)
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return m, fmt.Errorf("backup: %w", err)
	}
	defer func() { _ = os.RemoveAll(stage) }()
	for _, db := range dbs {
		name, typ := db[0], db[1]
		switch typ {
		case "duckdb":
			dir := filepath.Join(stage, name)
			if _, err := o.Engine.Exec(ctx, fmt.Sprintf("EXPORT DATABASE %s TO '%s' (FORMAT parquet, COMPRESSION zstd)",
				quote(name), strings.ReplaceAll(dir, "'", "''"))); err != nil {
				return m, fmt.Errorf("backup: export %s: %w", name, err)
			}
			m.Catalogs = append(m.Catalogs, name)
		case "ducklake":
			v, err := query(ctx, o.Engine, fmt.Sprintf("SELECT id::VARCHAR FROM %s.current_snapshot()", quote(name)))
			if err != nil || len(v) != 1 {
				return m, fmt.Errorf("backup: snapshot %s: %w", name, errors.Join(err, errors.New("no snapshot")))
			}
			id, _ := strconv.ParseInt(v[0][0], 10, 64) // ::VARCHAR dari BIGINT
			m.LakeSnapshots[name] = id
		}
	}
	err = filepath.WalkDir(stage, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(stage, path) // path selalu di dalam stage
		f := File{Key: m.Prefix + filepath.ToSlash(rel)}
		if f.SHA256, f.Bytes, err = sum(path); err != nil {
			return err
		}
		if err := o.Store.PutFile(ctx, f.Key, path, ret); err != nil {
			return err
		}
		m.Files = append(m.Files, f)
		return nil
	})
	if err != nil {
		return m, fmt.Errorf("backup: upload: %w", err)
	}
	slices.SortFunc(m.Files, func(a, b File) int { return strings.Compare(a.Key, b.Key) })
	m.FinishedAt = now().UTC()
	if o.Signer != nil {
		m.KeyID = o.Signer.KeyID
	}
	b, _ := json.MarshalIndent(m, "", "  ") // struct sederhana: tidak pernah error
	if o.Signer != nil {
		sig, err := o.Signer.SignBytes(ctx, b)
		if err != nil {
			return m, err
		}
		if err := putBytes(ctx, o, m.Prefix+"backup.json.sig", sig, ret); err != nil {
			return m, err
		}
	}
	// backup.json terakhir: penanda backup lengkap.
	return m, putBytes(ctx, o, m.Prefix+"backup.json", b, ret)
}

func putBytes(ctx context.Context, o Options, key string, b []byte, ret time.Duration) error {
	f, err := os.CreateTemp(o.StagingDir, "manifest-*")
	if err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	defer func() { _ = os.Remove(f.Name()) }()
	_, err = f.Write(b)
	if err = errors.Join(err, f.Close()); err != nil {
		return fmt.Errorf("backup: %w", err)
	}
	return o.Store.PutFile(ctx, key, f.Name(), ret)
}

func sum(path string) (string, int64, error) {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// query baris hasil sebagai teks.
func query(ctx context.Context, eng engine.Engine, q string) ([][]string, error) {
	rdr, err := eng.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("backup: %w", err)
	}
	defer rdr.Release()
	var out [][]string
	for rdr.Next() {
		rec := rdr.RecordBatch()
		for i := range int(rec.NumRows()) {
			row := make([]string, rec.NumCols())
			for c := range row {
				row[c] = strings.Clone(rec.Column(c).ValueStr(i))
			}
			out = append(out, row)
		}
	}
	return out, rdr.Err()
}
