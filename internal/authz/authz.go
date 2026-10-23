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
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

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

// AnyCatalog wildcard; Need dengan Object AnyCatalog = izin global (hanya
// role dengan entri "*" yang memenuhinya).
const AnyCatalog = "*"

// Need satu izin yang dibutuhkan statement. Object: "catalog",
// "catalog.schema" atau "catalog.schema.table" (huruf kecil).
type Need struct {
	Object string
	Action Action
}

func (n Need) String() string {
	if n.Object == AnyCatalog {
		return fmt.Sprintf("%s (global)", n.Action)
	}
	return fmt.Sprintf("%s on %s", n.Action, n.Object)
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

// Role izin dan pembatasan.
type Role struct {
	// Catalogs izin per objek (G-02): kunci "*", "catalog", "catalog.schema"
	// atau "catalog.schema.table"; izin pada objek berlaku untuk isinya.
	Catalogs map[string][]Action `yaml:"catalogs"`
	// Unmask tag klasifikasi yang boleh dilihat mentah ("*" = semua) (G-03).
	Unmask []string `yaml:"unmask"`
	// RowFilters "catalog.schema.table" → predikat SQL; {{claim.x}} diganti
	// literal string nilai claim identitas (G-04).
	RowFilters map[string]string `yaml:"row_filters"`
	// Profil terbatas (G-06, mis. ai_sandbox).
	SelectOnly bool          `yaml:"select_only"`
	MaxRows    int64         `yaml:"max_rows"`
	MaxBytes   int64         `yaml:"max_bytes"`
	Timeout    time.Duration `yaml:"timeout"`
}

// placeholder {{claim.<nama>}} di row filter.
var placeholder = regexp.MustCompile(`\{\{\s*claim\.([A-Za-z0-9_\-]+)\s*\}\}`)

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
		norm := map[string][]Action{} // nama objek DuckDB case-insensitive
		for obj, acts := range r.Catalogs {
			if n := len(strings.Split(obj, ".")); obj != AnyCatalog && (obj == "" || n > 3) {
				errs = append(errs, fmt.Errorf("role %s: object %q must be *, catalog, catalog.schema or catalog.schema.table", name, obj))
			}
			for _, a := range acts {
				if a != Read && a != Write && a != Admin {
					errs = append(errs, fmt.Errorf("role %s object %s: unknown action %q", name, obj, a))
				}
			}
			norm[strings.ToLower(obj)] = acts
		}
		r.Catalogs = norm
		filters := map[string]string{}
		for table, pred := range r.RowFilters {
			if len(strings.Split(table, ".")) != 3 {
				errs = append(errs, fmt.Errorf("role %s row_filters: %q must be catalog.schema.table", name, table))
			}
			if strings.TrimSpace(pred) == "" || strings.Contains(placeholder.ReplaceAllString(pred, ""), "{{") {
				errs = append(errs, fmt.Errorf("role %s row_filters %s: empty predicate or invalid placeholder (use {{claim.name}})", name, table))
			}
			filters[strings.ToLower(table)] = pred
		}
		r.RowFilters = filters
		if r.MaxRows < 0 || r.MaxBytes < 0 || r.Timeout < 0 {
			errs = append(errs, fmt.Errorf("role %s: limits must be >= 0", name))
		}
		p.Roles[name] = r
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
	return slices.ContainsFunc(roles, func(name string) bool { return p.Roles[name].grants(n) })
}

// grants: entri "*" atau objek itu sendiri atau induknya (catalog/schema).
func (r Role) grants(n Need) bool {
	obj := strings.ToLower(n.Object)
	for key, acts := range r.Catalogs {
		if !slices.Contains(acts, n.Action) {
			continue
		}
		if key == AnyCatalog || (obj != AnyCatalog && (obj == key || strings.HasPrefix(obj, key+"."))) {
			return true
		}
	}
	return false
}

// Visible melaporkan apakah objek boleh tampil di metadata: bisa dibaca, atau
// ada izin baca pada objek di dalamnya (mis. catalog dengan satu tabel ter-grant).
func (p *Policy) Visible(roles []string, object string) bool {
	obj := strings.ToLower(object)
	if p.grants(roles, Need{Object: obj, Action: Read}) {
		return true
	}
	for _, name := range roles {
		for key, acts := range p.Roles[name].Catalogs {
			if strings.HasPrefix(key, obj+".") && slices.Contains(acts, Read) {
				return true
			}
		}
	}
	return false
}

// Unmasked melaporkan apakah salah satu role boleh melihat tag mentah.
func (p *Policy) Unmasked(roles []string, tag string) bool {
	return slices.ContainsFunc(roles, func(name string) bool {
		u := p.Roles[name].Unmask
		return slices.Contains(u, "*") || slices.Contains(u, tag)
	})
}

// RowFilter predikat RLS untuk tabel (G-04). Semantik permisif: role yang
// memberi izin baca tanpa row filter → tanpa filter; selain itu OR semua
// filter role pemberi izin. Placeholder diganti literal nilai attrs; claim
// yang tidak ada → error (fail-closed).
func (p *Policy) RowFilter(roles []string, table string, attrs map[string]string) (string, bool, error) {
	table = strings.ToLower(table)
	var preds []string
	for _, name := range roles {
		r := p.Roles[name]
		if !r.grants(Need{Object: table, Action: Read}) {
			continue
		}
		pred, ok := r.RowFilters[table]
		if !ok {
			return "", false, nil
		}
		var missing []string
		expanded := placeholder.ReplaceAllStringFunc(pred, func(m string) string {
			key := placeholder.FindStringSubmatch(m)[1]
			v, ok := attrs[key]
			if !ok {
				missing = append(missing, key)
			}
			return "'" + strings.ReplaceAll(v, "'", "''") + "'"
		})
		if len(missing) > 0 {
			return "", false, fmt.Errorf("row filter on %s needs claim %s", table, strings.Join(missing, ", "))
		}
		preds = append(preds, "("+expanded+")")
	}
	if len(preds) == 0 {
		return "", false, &DenyError{Need: Need{Object: table, Action: Read}}
	}
	return strings.Join(preds, " OR "), true, nil
}

// Limits pembatasan efektif (G-06): yang paling ketat dari semua role.
type Limits struct {
	SelectOnly bool
	MaxRows    int64
	MaxBytes   int64
	Timeout    time.Duration
}

// LimitsFor menggabungkan pembatasan role.
func (p *Policy) LimitsFor(roles []string) Limits {
	var l Limits
	minPos := func(cur, v int64) int64 {
		if v > 0 && (cur == 0 || v < cur) {
			return v
		}
		return cur
	}
	for _, name := range roles {
		r := p.Roles[name]
		l.SelectOnly = l.SelectOnly || r.SelectOnly
		l.MaxRows = minPos(l.MaxRows, r.MaxRows)
		l.MaxBytes = minPos(l.MaxBytes, r.MaxBytes)
		l.Timeout = time.Duration(minPos(int64(l.Timeout), int64(r.Timeout)))
	}
	return l
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
