package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/bankmega/karhutlasql/internal/auth"
	"github.com/bankmega/karhutlasql/internal/objstore"
)

// Dua instance berbagi arsip: kill di A (sesi aktif di B) membatalkan
// statement B dan menolak token di B setelah Sync.
func TestRevocationSync(t *testing.T) {
	ctx := context.Background()
	store := objstore.Dir(t.TempDir())
	exp := time.Now().Add(time.Hour)
	a, b := NewRegistry(time.Hour), NewRegistry(time.Hour)
	sa := &RevocationSync{Store: store, Registry: a, Instance: "a"}
	sb := &RevocationSync{Store: store, Registry: b, Instance: "b"}
	a.SetPublisher(sa.Publish)
	b.SetPublisher(sb.Publish)

	id := auth.Identity{Subject: "ana", TokenID: "oidc:s-ana", ExpiresAt: exp}
	sess, err := b.Attach(id, "p")
	if err != nil {
		t.Fatal(err)
	}
	qctx, done := sess.Begin(ctx, "SELECT 1")
	defer done()

	// Admin di instance A: sesi tidak lokal tetapi revocation bersama → berhasil.
	if killed, err := a.Kill(ctx, id.TokenID); !killed || err != nil {
		t.Fatalf("kill on A: %v %v", killed, err)
	}
	if _, err := a.Attach(id, "p"); !errors.Is(err, ErrRevoked) {
		t.Errorf("A still accepts token: %v", err)
	}
	if err := sb.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(context.Cause(qctx), ErrKilled) {
		t.Errorf("statement on B not cancelled: %v", context.Cause(qctx))
	}
	if _, err := b.Attach(id, "p"); !errors.Is(err, ErrRevoked) {
		t.Errorf("B still accepts token: %v", err)
	}
	// Instance baru (restart) memuat revocation saat start; kill ulang idempoten.
	c := NewRegistry(time.Hour)
	if err := (&RevocationSync{Store: store, Registry: c}).Sync(ctx); err != nil || !c.Revoked(id.TokenID) {
		t.Errorf("new instance: revoked=%v err=%v", c.Revoked(id.TokenID), err)
	}
	if _, err := b.Kill(ctx, id.TokenID); err != nil {
		t.Errorf("repeat kill: %v", err)
	}
	// Publikasi gagal: revoke lokal tetap, error dilaporkan.
	d := NewRegistry(time.Hour)
	d.SetPublisher(func(context.Context, string, time.Time) error { return errors.New("arsip mati") })
	if killed, err := d.Kill(ctx, "x"); !killed || err == nil || !d.Revoked("x") {
		t.Errorf("publish failure: %v %v", killed, err)
	}
}
