// Package verified verified queries + semantic model (Fase 8c): pasangan
// pertanyaan → SQL yang sudah diperiksa manusia, untuk agen AI (MCP) dan
// klien SQL. SQL tetap dieksekusi sebagai pemanggil (tidak menaikkan hak).
package verified

import (
	"bytes"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Query satu verified query.
type Query struct {
	Name       string    `yaml:"-"`
	Question   string    `yaml:"question"`
	SQL        string    `yaml:"sql"`
	VerifiedBy string    `yaml:"verified_by"`
	VerifiedAt time.Time `yaml:"verified_at"`
}

// Set isi --verified-queries-file.
type Set struct {
	// SemanticModel teks bebas (biasanya YAML: tabel, relasi, metrik,
	// dimensi) yang diberikan apa adanya ke agen.
	SemanticModel string           `yaml:"semantic_model"`
	Queries       map[string]Query `yaml:"verified_queries"`
}

var namePattern = regexp.MustCompile(`^[a-z0-9_]+$`)

// Load membaca file; path kosong = set kosong.
func Load(path string) (*Set, error) {
	s := &Set{Queries: map[string]Query{}}
	if path == "" {
		return s, nil
	}
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("verified.Load: %w", err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(s); err != nil {
		return nil, fmt.Errorf("verified.Load: %w", err)
	}
	var errs []error
	for name, q := range s.Queries {
		if !namePattern.MatchString(name) || strings.TrimSpace(q.Question) == "" || strings.TrimSpace(q.SQL) == "" ||
			q.VerifiedBy == "" || q.VerifiedAt.IsZero() {
			errs = append(errs, fmt.Errorf("verified query %q: name [a-z0-9_], question, sql, verified_by and verified_at are required", name))
		}
		q.Name = name
		s.Queries[name] = q
	}
	if s.Queries == nil {
		s.Queries = map[string]Query{}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("verified.Load: %w", err)
	}
	return s, nil
}

// List semua query urut nama.
func (s *Set) List() []Query {
	out := make([]Query, 0, len(s.Queries))
	for _, n := range slices.Sorted(maps.Keys(s.Queries)) {
		out = append(out, s.Queries[n])
	}
	return out
}
