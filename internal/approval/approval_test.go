package approval

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fixture struct {
	path, dir string
	active    string
	events    []Event
}

func (f *fixture) doc() Doc {
	return Doc{Name: "policy", Path: f.path, Prepare: func(b []byte) (func(), error) {
		if strings.Contains(string(b), "invalid") {
			return nil, errors.New("invalid policy")
		}
		return func() { f.active = string(b) }, nil
	}}
}

func (f *fixture) write(t *testing.T, s string) {
	t.Helper()
	if err := os.WriteFile(f.path, []byte(s), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newFixture(t *testing.T) (*fixture, *Gate) {
	d := t.TempDir()
	f := &fixture{path: filepath.Join(d, "policy.yaml"), dir: filepath.Join(d, "state")}
	f.write(t, "v1")
	g := New(f.dir, func(e Event) { f.events = append(f.events, e) })
	if err := g.Open(f.doc()); err != nil {
		t.Fatal(err)
	}
	return f, g
}

func TestFourEyes(t *testing.T) {
	f, g := newFixture(t)
	if f.active != "v1" || g.Status()[0].ApprovedBy[0] != Bootstrap {
		t.Fatalf("bootstrap: %q %+v", f.active, g.Status())
	}
	st, _ := os.Stat(filepath.Join(f.dir, "policy.approved.json"))
	if st.Mode().Perm() != 0o600 {
		t.Errorf("state mode %v", st.Mode())
	}

	f.write(t, "v2")
	s, err := g.Propose("policy", "local:ana")
	if err != nil || f.active != "v1" || s.Pending == nil || len(s.Pending.Approvals) != 1 {
		t.Fatalf("propose: %q %+v %v", f.active, s, err)
	}
	h := s.Pending.Hash
	if _, err := g.Approve("policy", "local:ana", h); !errors.Is(err, ErrSameApprover) || f.active != "v1" {
		t.Fatalf("same approver: %v %q", err, f.active)
	}
	if _, err := g.Approve("policy", "oidc:dian", "deadbeef"); !errors.Is(err, ErrNoPending) {
		t.Fatalf("wrong hash: %v", err)
	}
	if s, err = g.Approve("policy", "oidc:dian", h); err != nil || f.active != "v2" || s.Pending != nil || s.ActiveHash != h {
		t.Fatalf("second approval: %q %+v %v", f.active, s, err)
	}

	// SIGHUP (tanpa identitas) → butuh dua persetujuan.
	f.write(t, "v3")
	s, _ = g.Propose("policy", "")
	h = s.Pending.Hash
	_, _ = g.Approve("policy", "a", h)
	if f.active != "v2" {
		t.Fatal("activated after one approval")
	}
	_, _ = g.Approve("policy", "b", h)
	if f.active != "v3" {
		t.Fatal("not activated after two approvals")
	}

	// File tidak valid → error, yang aktif tetap.
	f.write(t, "invalid")
	if _, err := g.Propose("policy", "a"); err == nil || f.active != "v3" {
		t.Fatalf("invalid: %v %q", err, f.active)
	}

	// Restart: file berubah tanpa persetujuan → yang disetujui tetap aktif, file jadi pending.
	f.write(t, "v4-unapproved")
	f.active = ""
	g2 := New(f.dir, nil)
	if err := g2.Open(f.doc()); err != nil {
		t.Fatal(err)
	}
	if f.active != "v3" || g2.Status()[0].Pending == nil {
		t.Fatalf("restart: active %q status %+v", f.active, g2.Status())
	}
	var kinds []string
	for _, e := range f.events {
		kinds = append(kinds, e.Status)
	}
	if strings.Join(kinds, ",") != "activated,pending,approved,approved,activated,pending,approved,approved,activated" {
		t.Errorf("events %v", kinds)
	}
}

func TestDisabled(t *testing.T) {
	d := t.TempDir()
	f := &fixture{path: filepath.Join(d, "p.yaml")}
	f.write(t, "v1")
	g := New("", nil)
	if err := g.Open(f.doc()); err != nil || f.active != "v1" {
		t.Fatal(err)
	}
	f.write(t, "v2")
	if _, err := g.Propose("policy", ""); err != nil || f.active != "v2" {
		t.Fatalf("disabled: %v %q", err, f.active)
	}
	if _, err := os.Stat(filepath.Join(d, "state")); err == nil {
		t.Error("state written while disabled")
	}
}

func TestCorruptState(t *testing.T) {
	f, _ := newFixture(t)
	p := filepath.Join(f.dir, "policy.approved.json")
	b, _ := os.ReadFile(p)
	_ = os.WriteFile(p, []byte(strings.Replace(string(b), `"content":"v1"`, `"content":"evil"`, 1)), 0o600)
	if err := New(f.dir, nil).Open(f.doc()); err == nil {
		t.Fatal("tampered state accepted")
	}
}
