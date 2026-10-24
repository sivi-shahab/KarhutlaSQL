package classification

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"go.yaml.in/yaml/v3"
)

// Jenis mask (spec G-03). tokenize menyusul.
const (
	MaskNull    = "null"
	MaskHMAC    = "hash_hmac"
	MaskPartial = "partial"
	MaskRedact  = "redact"
)

// HMACFunc nama UDF HMAC-SHA256 (internal/sqlext); argumen kedua nonce query.
const HMACFunc = "karhutla_mask_hmac"

// Mask kebijakan mask satu tag.
type Mask struct {
	Mask     string `yaml:"mask"`
	KeepLast int    `yaml:"keep_last"` // partial: jumlah karakter akhir yang terlihat (default 4)
}

// Classification isi classification.yaml.
type Classification struct {
	Tags map[string]Mask `yaml:"tags"`
	// Columns "catalog.schema.table.column" → tag.
	Columns map[string]string `yaml:"columns"`
	// Subjects "catalog.schema.table" → kolom pengenal subjek data (mis. NIK)
	// untuk audit.pii_access (UU PDP hak akses subjek).
	Subjects map[string]string `yaml:"subjects"`

	byTable map[string]map[string]string // table → column → tag (huruf kecil)
}

// Parse memvalidasi dan mem-parse classification (field tak dikenal = error).
func Parse(b []byte) (*Classification, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var c Classification
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("classification.Parse: %w", err)
	}
	var errs []error
	for tag, m := range c.Tags {
		switch m.Mask {
		case MaskNull, MaskHMAC, MaskRedact:
		case MaskPartial:
			if m.KeepLast < 0 {
				errs = append(errs, fmt.Errorf("tag %s: keep_last must be >= 0", tag))
			}
			if m.KeepLast == 0 {
				m.KeepLast = 4
				c.Tags[tag] = m
			}
		default:
			errs = append(errs, fmt.Errorf("tag %s: unknown mask %q", tag, m.Mask))
		}
	}
	c.byTable = map[string]map[string]string{}
	for col, tag := range c.Columns {
		if _, ok := c.Tags[tag]; !ok {
			errs = append(errs, fmt.Errorf("column %s: undefined tag %q", col, tag))
		}
		parts := strings.Split(strings.ToLower(col), ".")
		if len(parts) != 4 {
			errs = append(errs, fmt.Errorf("column %s: want catalog.schema.table.column", col))
			continue
		}
		table := strings.Join(parts[:3], ".")
		if c.byTable[table] == nil {
			c.byTable[table] = map[string]string{}
		}
		c.byTable[table][parts[3]] = tag
	}
	subjects := map[string]string{}
	for table, col := range c.Subjects {
		if len(strings.Split(table, ".")) != 3 || strings.TrimSpace(col) == "" {
			errs = append(errs, fmt.Errorf("subjects %s: want catalog.schema.table: column", table))
		}
		subjects[strings.ToLower(table)] = strings.ToLower(col)
	}
	c.Subjects = subjects
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("classification.Parse: %w", err)
	}
	return &c, nil
}

// UsesHMAC melaporkan apakah ada tag dengan mask hash_hmac (butuh kunci).
func (c *Classification) UsesHMAC() bool {
	for _, m := range c.Tags {
		if m.Mask == MaskHMAC {
			return true
		}
	}
	return false
}

// TableTags kolom bertag milik tabel "catalog.schema.table" (huruf kecil) → tag.
func (c *Classification) TableTags(table string) map[string]string {
	return c.byTable[strings.ToLower(table)]
}

// Expr ekspresi SQL DuckDB yang me-mask kolom ident (sudah di-quote) bertipe
// typ. nonce diteruskan ke UDF HMAC (menolak pemanggilan di luar rewrite).
func (m Mask) Expr(ident, typ, nonce string) string {
	v := "CAST(" + ident + " AS VARCHAR)"
	switch m.Mask {
	case MaskNull:
		return "CAST(NULL AS " + typ + ")"
	case MaskHMAC:
		return fmt.Sprintf("%s(%s, %s)", HMACFunc, v, Literal(nonce))
	case MaskPartial:
		return fmt.Sprintf("CASE WHEN %[1]s IS NULL THEN NULL WHEN length(%[1]s) <= %[2]d THEN repeat('*', length(%[1]s)) "+
			"ELSE repeat('*', length(%[1]s) - %[2]d) || right(%[1]s, %[2]d) END", v, m.KeepLast)
	default: // redact (dan nilai tak dikenal: fail-closed)
		return "CASE WHEN " + ident + " IS NULL THEN NULL ELSE '[REDACTED]' END"
	}
}

// Ident meng-quote identifier SQL.
func Ident(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// Literal meng-quote string literal SQL.
func Literal(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// Store classification aktif; Reload menggantinya atomik.
type Store struct {
	path string
	c    atomic.Pointer[Classification]
}

// Load membaca dari path; path kosong = classification kosong (tanpa mask).
func Load(path string) (*Store, error) {
	s := &Store{path: path}
	if path == "" {
		s.c.Store(&Classification{byTable: map[string]map[string]string{}})
		return s, nil
	}
	return s, s.Reload()
}

// Reload membaca ulang file; yang lama tetap bila file tidak valid.
func (s *Store) Reload() error {
	if s.path == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Clean(s.path))
	if err != nil {
		return fmt.Errorf("classification.Reload: %w", err)
	}
	c, err := Parse(b)
	if err != nil {
		return err
	}
	s.c.Store(c)
	return nil
}

// Prepare memvalidasi isi classification; activate menjadikannya aktif
// (dipakai approval.Gate, G-11). check tambahan opsional (mis. kunci HMAC).
func (s *Store) Prepare(b []byte, check func(*Classification) error) (func(), error) {
	c, err := Parse(b)
	if err == nil && check != nil {
		err = check(c)
	}
	if err != nil {
		return nil, err
	}
	return func() { s.c.Store(c) }, nil
}

// Get classification aktif.
func (s *Store) Get() *Classification { return s.c.Load() }
