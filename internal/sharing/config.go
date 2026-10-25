// Package sharing server protokol Delta Sharing (backlog fase 9e): berbagi
// tabel read-only ke pihak luar (regulator, mitra) yang memakai klien Delta
// Sharing (pandas, Spark, Power BI). Setiap export berjalan sebagai identitas
// internal "share:<recipient>" lewat Flight SQL in-process, sehingga izin,
// masking, RLS, audit, dan access history berlaku per penerima; hasil ditulis
// sebagai Parquet dan diunduh lewat URL bertanda tangan berumur pendek.
package sharing

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Config isi --sharing-file.
type Config struct {
	// Recipients nama → hash token.
	Recipients map[string]Recipient `yaml:"recipients"`
	// Shares nama → share.
	Shares map[string]Share `yaml:"shares"`
}

// Recipient penerima; token bearer tidak pernah disimpan, hanya sha256-nya.
type Recipient struct {
	TokenSHA256 string `yaml:"token_sha256"`
}

// Share kumpulan tabel untuk penerima tertentu.
type Share struct {
	Recipients []string `yaml:"recipients"`
	// Schemas nama schema share → nama tabel share → tabel sumber
	// "catalog.schema.table".
	Schemas map[string]map[string]string `yaml:"schemas"`
}

var (
	namePattern  = regexp.MustCompile(`^[a-z0-9_]+$`)
	identPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	hexPattern   = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

// Load membaca file; path kosong = sharing mati (nil).
func Load(path string) (*Config, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("sharing.Load: %w", err)
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("sharing.Load: %w", err)
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("sharing.Load: %w", err)
	}
	return &c, nil
}

func (c *Config) validate() error {
	var errs []error
	for name, r := range c.Recipients {
		if !namePattern.MatchString(name) {
			errs = append(errs, fmt.Errorf("recipient %q: name must match [a-z0-9_]+", name))
		}
		if !hexPattern.MatchString(r.TokenSHA256) {
			errs = append(errs, fmt.Errorf("recipient %q: token_sha256 must be 64 lowercase hex characters", name))
		}
	}
	for name, s := range c.Shares {
		if !namePattern.MatchString(name) {
			errs = append(errs, fmt.Errorf("share %q: name must match [a-z0-9_]+", name))
		}
		if len(s.Recipients) == 0 {
			errs = append(errs, fmt.Errorf("share %q: at least one recipient is required", name))
		}
		for _, r := range s.Recipients {
			if _, ok := c.Recipients[r]; !ok {
				errs = append(errs, fmt.Errorf("share %q: unknown recipient %q", name, r))
			}
		}
		for schema, tables := range s.Schemas {
			if !namePattern.MatchString(schema) {
				errs = append(errs, fmt.Errorf("share %q: schema %q must match [a-z0-9_]+", name, schema))
			}
			for table, src := range tables {
				if !namePattern.MatchString(table) {
					errs = append(errs, fmt.Errorf("share %q: table %q must match [a-z0-9_]+", name, table))
				}
				if parts := strings.Split(src, "."); len(parts) != 3 || slices.ContainsFunc(parts, func(p string) bool { return !identPattern.MatchString(p) }) {
					errs = append(errs, fmt.Errorf("share %q: table %q: source %q must be catalog.schema.table", name, table, src))
				}
			}
		}
	}
	return errors.Join(errs...)
}

// recipientFor nama penerima pemilik token (dibandingkan sebagai hash,
// waktu konstan per penerima); "" bila tidak ada.
func (c *Config) recipientFor(token string) string {
	sum := sha256.Sum256([]byte(token))
	got := hex.EncodeToString(sum[:])
	found := ""
	for _, name := range slices.Sorted(maps.Keys(c.Recipients)) {
		if constantEqual(got, c.Recipients[name].TokenSHA256) {
			found = name
		}
	}
	return found
}

// shareFor share yang boleh diakses recipient; ok false bila tidak ada atau
// bukan miliknya (dijawab sama: tidak bocorkan keberadaan share).
func (c *Config) shareFor(recipient, name string) (Share, bool) {
	s, ok := c.Shares[name]
	if !ok || !slices.Contains(s.Recipients, recipient) {
		return Share{}, false
	}
	return s, true
}

// sharesFor nama share milik recipient, urut.
func (c *Config) sharesFor(recipient string) []string {
	var out []string
	for _, name := range slices.Sorted(maps.Keys(c.Shares)) {
		if slices.Contains(c.Shares[name].Recipients, recipient) {
			out = append(out, name)
		}
	}
	return out
}
