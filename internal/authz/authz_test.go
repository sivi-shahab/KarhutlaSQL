package authz

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bankmega/karhutlasql/internal/auth"
)

const spec = `
default: deny
allow:
  domains: [bankmega.com]
  emails: [vendor@partner.io]
  groups: [data-platform]
roles:
  admin:   { catalogs: { "*": [read, write, admin] } }
  analyst: { catalogs: { Warehouse: [read], sandbox: [read, write] } }
  dq_runner: { catalogs: { warehouse: [read] } }
subjects:
  - match: { claim: groups, equals: data-platform }
    roles: [admin]
  - match: { claim: email, endswith: "@bankmega.com" }
    roles: [analyst]
  - match: { user: svc_dbt }
    roles: [analyst]
  - match: { user: svc_gx }
    roles: [dq_runner]
`

func TestPolicy(t *testing.T) {
	p, err := Parse([]byte(spec))
	if err != nil {
		t.Fatal(err)
	}
	ana := auth.Identity{Subject: "ana", Email: "ana@BankMega.com", Source: auth.SourceOIDC}
	ops := auth.Identity{Subject: "dian", Email: "dian@bankmega.com", Groups: []string{"data-platform"}, Source: auth.SourceOIDC}
	eve := auth.Identity{Subject: "eve", Email: "eve@gmail.com", Source: auth.SourceOIDC}
	fake := auth.Identity{Subject: "svc_dbt", Source: auth.SourceOIDC} // user: hanya identitas lokal
	dbt := auth.Identity{Subject: "svc_dbt", Source: auth.SourceLocal}

	allowed := map[string]bool{"ana": true, "dian": true, "eve": false}
	for _, id := range []auth.Identity{ana, ops, eve} {
		if got := p.Allowed(id); got != allowed[id.Subject] {
			t.Errorf("Allowed(%s) = %v", id.Subject, got)
		}
	}
	if !p.Allowed(auth.Identity{Email: "vendor@partner.io"}) || !p.Allowed(dbt) {
		t.Error("exact email / local identity should be allowed")
	}
	if r := p.RolesFor(fake); len(r) != 0 {
		t.Errorf("OIDC identity matched user rule: %v", r)
	}

	tests := []struct {
		id    auth.Identity
		needs []Need
		deny  bool
	}{
		{ana, []Need{{"warehouse", Read}}, false},
		{ana, []Need{{"WAREHOUSE", Read}, {"sandbox", Write}}, false},
		{ana, []Need{{"warehouse", Write}}, true},
		{ana, []Need{{AnyCatalog, Admin}}, true},
		{ops, []Need{{"anything", Write}, {AnyCatalog, Admin}}, false},
		{dbt, []Need{{"sandbox", Write}}, false},
		{eve, []Need{{"warehouse", Read}}, true},
		{ana, nil, false},
	}
	for i, tt := range tests {
		err := p.Check(p.RolesFor(tt.id), tt.needs)
		var de *DenyError
		if (err != nil) != tt.deny || (err != nil && !errors.As(err, &de)) {
			t.Errorf("case %d (%s %v): err = %v", i, tt.id.Subject, tt.needs, err)
		}
	}
}

func TestParse_Invalid(t *testing.T) {
	for _, y := range []string{
		"default: allow",
		"default: deny\nroles: { r: { catalogs: { a: [drop] } } }",
		"default: deny\nsubjects: [{ match: { user: x }, roles: [nope] }]",
		"default: deny\nroles: { r: {} }\nsubjects: [{ match: { claim: phone, equals: x }, roles: [r] }]",
		"default: deny\nroles: { r: {} }\nsubjects: [{ match: { claim: email }, roles: [r] }]",
		"default: deny\nroles: { r: {} }\nsubjects: [{ match: { user: x, claim: email, equals: y }, roles: [r] }]",
		"default: deny\nunknown_field: 1",
	} {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("Parse(%q) = nil error", y)
		}
	}
}

func TestStore_ReloadKeepsOldOnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.yaml")
	if err := os.WriteFile(path, []byte(spec), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	old := s.Policy()
	if err := os.WriteFile(path, []byte("default: allow"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(); err == nil || s.Policy() != old {
		t.Fatalf("invalid reload: err=%v replaced=%v", err, s.Policy() != old)
	}
	if err := os.WriteFile(path, []byte("default: deny\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Reload(); err != nil || s.Policy() == old {
		t.Fatalf("valid reload: err=%v", err)
	}
}
