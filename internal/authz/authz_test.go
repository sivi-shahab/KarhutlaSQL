package authz

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

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

const govPolicy = `
default: deny
roles:
  analyst:
    catalogs: { warehouse: [read], "sandbox.main": [read, write], "hr.main.gaji": [read] }
    unmask: [pii.phone]
  cabang:
    catalogs: { warehouse.main.nasabah: [read] }
    row_filters: { warehouse.main.nasabah: "branch_code = {{claim.branch}}" }
  pusat:
    catalogs: { warehouse: [read] }
  ai_sandbox:
    catalogs: { warehouse: [read] }
    select_only: true
    max_rows: 500
    timeout: 30s
    resource_group: ai
  batch:
    catalogs: {}
    max_rows: 10000
    max_bytes: 1048576
    resource_group: batch
resource_groups:
  ai:    { max_concurrent: 2, queue_size: 10, queue_timeout: 5s, timeout: 30s }
  batch: { max_concurrent: 1 }
subjects:
  - { match: { user: x }, roles: [analyst, cabang, pusat, ai_sandbox, batch] }
`

func TestPolicy_Governance(t *testing.T) {
	p, err := Parse([]byte(govPolicy))
	if err != nil {
		t.Fatal(err)
	}
	// G-02: izin di level schema/table.
	for _, tt := range []struct {
		need Need
		ok   bool
	}{
		{Need{"sandbox.main.t", Write}, true},
		{Need{"sandbox.other.t", Write}, false},
		{Need{"hr.main.gaji", Read}, true},
		{Need{"hr.main.cuti", Read}, false},
		{Need{"hr", Read}, false},
	} {
		if err := p.Check([]string{"analyst"}, []Need{tt.need}); (err == nil) != tt.ok {
			t.Errorf("%v: %v", tt.need, err)
		}
	}
	if !p.Visible([]string{"analyst"}, "hr") || !p.Visible([]string{"analyst"}, "hr.main") || p.Visible([]string{"analyst"}, "finance") {
		t.Error("Visible")
	}
	if !p.Unmasked([]string{"analyst"}, "pii.phone") || p.Unmasked([]string{"analyst"}, "pii.nik") {
		t.Error("Unmasked")
	}

	// G-04: filter hanya bila semua role pemberi izin punya filter.
	pred, filtered, err := p.RowFilter([]string{"cabang"}, "warehouse.main.nasabah", map[string]string{"branch": "01'23"})
	if err != nil || !filtered || pred != "(branch_code = '01''23')" {
		t.Errorf("cabang filter = %q %v %v", pred, filtered, err)
	}
	if _, filtered, _ := p.RowFilter([]string{"cabang", "pusat"}, "warehouse.main.nasabah", nil); filtered {
		t.Error("pusat (unfiltered grant) should lift the filter")
	}
	if _, _, err := p.RowFilter([]string{"cabang"}, "warehouse.main.nasabah", map[string]string{}); err == nil {
		t.Error("missing claim accepted")
	}
	if _, _, err := p.RowFilter([]string{"cabang"}, "warehouse.main.other", nil); err == nil {
		t.Error("filter without read grant")
	}

	// G-06: pembatasan paling ketat.
	l := p.LimitsFor([]string{"ai_sandbox", "batch", "analyst"})
	if !l.SelectOnly || l.MaxRows != 500 || l.MaxBytes != 1048576 || l.Timeout.Seconds() != 30 || l.Group != "ai" {
		t.Errorf("limits = %+v", l)
	}
	// G-08: role pertama yang menyebut grup; tanpa grup → default.
	if g := p.LimitsFor([]string{"analyst", "batch", "ai_sandbox"}).Group; g != "batch" {
		t.Errorf("group = %q", g)
	}
	if g := p.LimitsFor([]string{"analyst"}).Group; g != "default" {
		t.Errorf("group = %q", g)
	}
	if p.ResourceGroups["ai"].QueueTimeout.Seconds() != 5 {
		t.Errorf("groups = %+v", p.ResourceGroups)
	}
	for _, y := range []string{
		"default: deny\nroles: { r: { catalogs: { a.b.c.d: [read] } } }",
		"default: deny\nroles: { r: { row_filters: { t: x } } }",
		"default: deny\nroles: { r: { row_filters: { a.b.c: '{{user.x}} = 1' } } }",
		"default: deny\nroles: { r: { max_rows: -1 } }",
		"default: deny\nroles: { r: { resource_group: nope } }",
		"default: deny\nresource_groups: { g: { max_concurrent: -1 } }",
	} {
		if _, err := Parse([]byte(y)); err == nil {
			t.Errorf("Parse(%q) accepted", y)
		}
	}
}

func TestAgentLimits(t *testing.T) {
	p, err := Parse([]byte(`
default: deny
roles:
  admin: { catalogs: { "*": [read, write, admin] }, max_rows: 100 }
  ai_sandbox: { catalogs: { w: [read] }, max_rows: 500, max_bytes: 1000, timeout: 30s, resource_group: ai }
resource_groups:
  ai: { max_concurrent: 1 }
`))
	if err != nil {
		t.Fatal(err)
	}
	got := p.LimitsFor([]string{"admin"}).Tighten(p.AgentLimits())
	want := Limits{SelectOnly: true, MaxRows: 100, MaxBytes: 1000, Timeout: 30 * time.Second, Group: "ai"}
	if got != want {
		t.Errorf("admin via mcp = %+v, want %+v", got, want)
	}
	// Tanpa role ai_sandbox: default aman.
	if l := DevPolicy("admin").AgentLimits(); !l.SelectOnly || l.MaxRows != 500 || l.Timeout != time.Minute {
		t.Errorf("default agent limits %+v", l)
	}
}
