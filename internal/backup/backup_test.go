//go:build duckdb_arrow

package backup

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bankmega/karhutlasql/internal/engine/duckdb"
	"github.com/bankmega/karhutlasql/internal/objstore"
	"github.com/bankmega/karhutlasql/internal/report"
)

func TestTier(t *testing.T) {
	for d, want := range map[string]string{"2026-11-01": "monthly", "2026-10-25": "weekly", "2026-10-24": "daily"} {
		day, _ := time.Parse(time.DateOnly, d)
		if got := Tier(day); got != want {
			t.Errorf("Tier(%s) = %s, want %s", d, got, want)
		}
	}
}

func TestRunAndRestore(t *testing.T) {
	ctx := context.Background()
	staging, archive := t.TempDir(), objstore.Dir(t.TempDir())
	eng, err := duckdb.Open(ctx, duckdb.Config{AllowedDirectories: []string{staging + "/"},
		InitSQL: "ATTACH ':memory:' AS w; CREATE TABLE w.main.kredit AS SELECT range AS id, range * 10 AS plafon FROM range(1000);" +
			"CREATE SCHEMA w.s; CREATE VIEW w.s.besar AS SELECT * FROM w.main.kredit WHERE plafon > 5000;"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close() })

	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	keyPath := filepath.Join(t.TempDir(), "k.pem")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	signer, err := report.LoadSigner(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC)
	m, err := Options{Engine: eng, Store: archive, StagingDir: staging, Signer: signer, Now: func() time.Time { return now },
		Tiers: Tiers{Daily: 7 * 24 * time.Hour}}.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if m.Tier != "daily" || !strings.HasPrefix(m.Prefix, "backups/daily/") || len(m.Files) < 3 || m.KeyID != signer.KeyID {
		t.Fatalf("manifest %+v", m)
	}
	ents, _ := os.ReadDir(staging)
	if len(ents) != 0 {
		t.Errorf("staging not cleaned: %v", ents)
	}
	// Manifest bertanda tangan & hash file cocok.
	b, _ := archive.Get(ctx, m.Prefix+"backup.json")
	sig, _ := archive.Get(ctx, m.Prefix+"backup.json.sig")
	raw, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if !ed25519.Verify(pub, b, raw) {
		t.Fatal("backup.json signature invalid")
	}
	var got Manifest
	_ = json.Unmarshal(b, &got)
	for _, f := range got.Files {
		data, err := archive.Get(ctx, f.Key)
		if err != nil || report.SHA256(data) != f.SHA256 {
			t.Errorf("%s: hash mismatch (%v)", f.Key, err)
		}
	}
	// Restore: catalog di-attach dengan nama sama lalu IMPORT DATABASE.
	dir := filepath.Join(string(archive), filepath.FromSlash(m.Prefix), "w")
	restored, err := duckdb.Open(ctx, duckdb.Config{Unlocked: true,
		InitSQL: "ATTACH ':memory:' AS w; USE w; IMPORT DATABASE '" + dir + "'"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restored.Close() })
	r, err := restored.Query(ctx, "SELECT count(*)::VARCHAR FROM w.s.besar")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Release()
	if !r.Next() || r.RecordBatch().Column(0).ValueStr(0) != "499" {
		t.Error("restored view count wrong")
	}
}
