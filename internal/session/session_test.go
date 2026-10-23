package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bankmega/karhutlasql/internal/auth"
)

func TestRegistry_KillCancelsAndRevokes(t *testing.T) {
	r := NewRegistry(time.Hour)
	exp := time.Now().Add(time.Hour)
	a, err := r.Attach(auth.Identity{Subject: "a", Source: auth.SourceLocal, TokenID: "A", ExpiresAt: exp}, "p")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := r.Attach(auth.Identity{Subject: "b", TokenID: "B", ExpiresAt: exp}, "p")
	a.Put("h", 1)
	ctx, done := a.Begin(context.Background(), "SELECT 1")
	defer done()
	_, doneB := b.Begin(context.Background(), "SELECT 2")
	defer doneB()

	if in := r.Snapshot(); len(in) != 2 || in[0].ActiveStatements != 1 || in[0].SQL != "SELECT 1" {
		t.Fatalf("snapshot = %+v", in)
	}
	if _, ok := b.Get("h"); ok {
		t.Fatal("handle visible to other session")
	}
	if !r.Kill("A") || r.Kill("nope") {
		t.Fatal("kill result")
	}
	if !errors.Is(context.Cause(ctx), ErrKilled) {
		t.Fatalf("cause = %v", context.Cause(ctx))
	}
	if _, err := r.Attach(auth.Identity{Subject: "a", TokenID: "A", ExpiresAt: exp}, "p"); !errors.Is(err, ErrRevoked) {
		t.Fatalf("attach after kill = %v", err)
	}
	if bySrc, n := r.Counts(); n != 1 || len(bySrc) != 1 {
		t.Fatalf("counts = %v %d", bySrc, n)
	}
}

func TestRegistry_Sweep(t *testing.T) {
	r := NewRegistry(time.Minute)
	now := time.Now()
	r.now = func() time.Time { return now }
	exp := now.Add(time.Hour)
	idle, _ := r.Attach(auth.Identity{TokenID: "idle", ExpiresAt: exp}, "")
	busy, _ := r.Attach(auth.Identity{TokenID: "busy", ExpiresAt: exp}, "")
	_, done := busy.Begin(context.Background(), "q")
	defer done()
	_ = idle
	_, _ = r.Attach(auth.Identity{TokenID: "dead", ExpiresAt: exp}, "")
	r.Kill("dead")

	now = now.Add(2 * time.Minute)
	r.Sweep()
	if in := r.Snapshot(); len(in) != 1 || in[0].ID != "busy" {
		t.Fatalf("after idle sweep = %+v", in)
	}
	if !r.Revoked("dead") {
		t.Fatal("revocation dropped before token expiry")
	}
	now = exp.Add(time.Second)
	r.Sweep()
	if r.Revoked("dead") || len(r.Snapshot()) != 0 {
		t.Fatal("expired revocation/session not swept")
	}
	if _, err := r.Attach(auth.Identity{TokenID: "x"}, ""); err == nil {
		t.Fatal("identity without expiry accepted")
	}
}
