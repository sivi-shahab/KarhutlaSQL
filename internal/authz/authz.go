// Package authz memuat policy YAML (spec §5.3), memetakan identitas ke role,
// menegakkan allowlist identitas OIDC (E-02) dan izin per catalog (E-03).
// Default deny: izin hanya dari role yang eksplisit.
package authz

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"

	"go.yaml.in/yaml/v3"

	"github.com/bankmega/karhutlasql/internal/auth"
)

// Action izin per catalog.
type Action string

// Aksi yang dikenal.
const (
	Read  Action = "read"
	Write Action = "write"
	Admin Action = "admin"
)

// AnyCatalog wildcard catalog; Need dengan Catalog AnyCatalog = izin global
// (hanya role dengan entri "*" yang memenuhinya).
const AnyCatalog = "*"

// Need satu izin yang dibutuhkan statement.
type Need struct {
	Catalog string
	Action  Action
}

func (n Need) String() string {
	if n.Catalog == AnyCatalog {
		return fmt.Sprintf("%s (global)", n.Action)
	}
	return fmt.Sprintf("%s on catalog %s", n.Action, n.Catalog)
}

// DenyError izin yang tidak dimiliki.
type DenyError struct{ Need Need }

func (e *DenyError) Error() string { return "permission denied: " + e.Need.String() }

// Policy hasil parse policy.yaml.
type Policy struct {
	Default  string          `yaml:"default"`
	Allow    Allowlist       `yaml:"allow"`
	Roles    map[string]Role `yaml:"roles"`
	Subjects []Subject       `yaml:"subjects"`
}

// Allowlist identitas OIDC yang boleh masuk (E-02); identitas lokal tidak
// tunduk allowlist (sudah eksplisit di konfigurasi server). Kosong = tolak
// semua identitas OIDC.
type Allowlist struct {
	Emails  []string `yaml:"emails"`
	Domains []string `yaml:"domains"` // tanpa "@"
	Groups  []string `yaml:"groups"`
}

// Role izin per catalog.
type Role struct {
	Catalogs map[string][]Action `yaml:"catalogs"`
}

// Subject memetakan identitas ke role.
type Subject struct {
	Match Match    `yaml:"match"`
	Roles []string `yaml:"roles"`
}

// Match kondisi subject: User (hanya identitas lokal) atau Claim
// (email|groups|sub) dengan Equals atau EndsWith.
type Match struct {
	User     string `yaml:"user"`
	Claim    string `yaml:"claim"`
	Equals   string `yaml:"equals"`
	EndsWith string `yaml:"endswith"`
}

// Parse memvalidasi dan mem-parse policy (field tak dikenal = error).
func Parse(b []byte) (*Policy, error) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	var p Policy
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("authz.Parse: %w", err)
	}
	if err := p.validate(); err != nil {
		return nil, fmt.Errorf("authz.Parse: %w", err)
	}
	return &p, nil
}

func (p *Policy) validate() error {
	var errs []error
	if p.Default != "deny" {
		errs = append(errs, fmt.Errorf(`default must be "deny", got %q`, p.Default))
	}
	for name, r := range p.Roles {
		norm := map[string][]Action{} // nama catalog DuckDB case-insensitive
		for cat, acts := range r.Catalogs {
			for _, a := range acts {
				if a != Read && a != Write && a != Admin {
					errs = append(errs, fmt.Errorf("role %s catalog %s: unknown action %q", name, cat, a))
				}
			}
			norm[strings.ToLower(cat)] = acts
		}
		p.Roles[name] = Role{Catalogs: norm}
	}
	for i, s := range p.Subjects {
		m := s.Match
		switch {
		case m.User != "" && (m.Claim != "" || m.Equals != "" || m.EndsWith != ""):
			errs = append(errs, fmt.Errorf("subjects[%d]: user cannot be combined with claim", i))
		case m.User == "" && !slices.Contains([]string{"email", "groups", "sub"}, m.Claim):
			errs = append(errs, fmt.Errorf("subjects[%d]: claim must be email, groups or sub, got %q", i, m.Claim))
		case m.User == "" && (m.Equals == "") == (m.EndsWith == ""):
			errs = append(errs, fmt.Errorf("subjects[%d]: exactly one of equals/endswith is required", i))
		}
		if len(s.Roles) == 0 {
			errs = append(errs, fmt.Errorf("subjects[%d]: roles is empty", i))
		}
		for _, r := range s.Roles {
			if _, ok := p.Roles[r]; !ok {
				errs = append(errs, fmt.Errorf("subjects[%d]: undefined role %q", i, r))
			}
		}
	}
	return errors.Join(errs...)
}

// Allowed melaporkan apakah identitas boleh masuk (E-02).
func (p *Policy) Allowed(id auth.Identity) bool {
	if id.Source == auth.SourceLocal {
		return true
	}
	email := strings.ToLower(id.Email)
	if email != "" {
		if slices.ContainsFunc(p.Allow.Emails, func(e string) bool { return strings.EqualFold(e, email) }) {
			return true
		}
		_, domain, _ := strings.Cut(email, "@")
		if slices.ContainsFunc(p.Allow.Domains, func(d string) bool { return strings.EqualFold(strings.TrimPrefix(d, "@"), domain) }) {
			return true
		}
	}
	return slices.ContainsFunc(id.Groups, func(g string) bool { return slices.Contains(p.Allow.Groups, g) })
}

// RolesFor role milik identitas (gabungan semua subject yang cocok).
func (p *Policy) RolesFor(id auth.Identity) []string {
	var roles []string
	for _, s := range p.Subjects {
		if s.Match.matches(id) {
			for _, r := range s.Roles {
				if !slices.Contains(roles, r) {
					roles = append(roles, r)
				}
			}
		}
	}
	return roles
}

func (m Match) matches(id auth.Identity) bool {
	if m.User != "" {
		return id.Source == auth.SourceLocal && id.Subject == m.User
	}
	var vals []string
	switch m.Claim {
	case "email":
		vals = []string{id.Email}
	case "groups":
		vals = id.Groups
	case "sub":
		vals = []string{id.Subject}
	}
	for _, v := range vals {
		if v == "" {
			continue
		}
		if (m.Equals != "" && strings.EqualFold(v, m.Equals)) || (m.EndsWith != "" && strings.HasSuffix(strings.ToLower(v), strings.ToLower(m.EndsWith))) {
			return true
		}
	}
	return false
}

// Check memastikan roles memenuhi semua needs; error pertama berupa *DenyError.
func (p *Policy) Check(roles []string, needs []Need) error {
	for _, n := range needs {
		if !p.grants(roles, n) {
			return &DenyError{Need: n}
		}
	}
	return nil
}

func (p *Policy) grants(roles []string, n Need) bool {
	cat := strings.ToLower(n.Catalog)
	for _, name := range roles {
		r := p.Roles[name]
		if slices.Contains(r.Catalogs[AnyCatalog], n.Action) {
			return true
		}
		if cat != AnyCatalog && slices.Contains(r.Catalogs[cat], n.Action) {
			return true
		}
	}
	return false
}

// DevPolicy dipakai bila --policy-file tidak diset: hanya user lokal yang
// dikonfigurasi, sebagai admin. Tidak ada identitas OIDC yang diizinkan.
func DevPolicy(localUser string) *Policy {
	return &Policy{
		Default:  "deny",
		Roles:    map[string]Role{auth.RoleAdmin: {Catalogs: map[string][]Action{AnyCatalog: {Read, Write, Admin}}}},
		Subjects: []Subject{{Match: Match{User: localUser}, Roles: []string{auth.RoleAdmin}}},
	}
}

// Store policy aktif; Reload menggantinya secara atomik (hot reload SIGHUP).
type Store struct {
	path string
	p    atomic.Pointer[Policy]
}

// Load membaca policy dari path.
func Load(path string) (*Store, error) {
	s := &Store{path: path}
	if err := s.Reload(); err != nil {
		return nil, err
	}
	return s, nil
}

// Static Store berisi policy tetap (Reload no-op).
func Static(p *Policy) *Store {
	s := &Store{}
	s.p.Store(p)
	return s
}

// Reload membaca ulang file; policy lama tetap aktif bila file tidak valid.
func (s *Store) Reload() error {
	if s.path == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Clean(s.path))
	if err != nil {
		return fmt.Errorf("authz.Reload: %w", err)
	}
	p, err := Parse(b)
	if err != nil {
		return err
	}
	s.p.Store(p)
	return nil
}

// Policy policy aktif.
func (s *Store) Policy() *Policy { return s.p.Load() }
