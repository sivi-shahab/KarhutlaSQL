package report

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeVault Transit minimal: GET keys/<k>, POST sign/<k>.
func fakeVault(t *testing.T, token string) (*httptest.Server, ed25519.PublicKey, *int) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	signs := new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Vault-Token") != token {
			http.Error(w, `{"errors":["permission denied"]}`, http.StatusForbidden)
			return
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/transit/keys/report":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"type": "ed25519", "latest_version": 2,
				"keys": map[string]any{"2": map[string]string{"public_key": base64.StdEncoding.EncodeToString(pub)}}}})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/transit/sign/report":
			var in struct {
				Input      string `json:"input"`
				KeyVersion int    `json:"key_version"`
			}
			_ = json.NewDecoder(r.Body).Decode(&in)
			msg, _ := base64.StdEncoding.DecodeString(in.Input)
			if in.KeyVersion != 2 {
				http.Error(w, "bad version", http.StatusBadRequest)
				return
			}
			*signs++
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]string{
				"signature": "vault:v2:" + base64.StdEncoding.EncodeToString(ed25519.Sign(priv, msg))}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, pub, signs
}

func TestVaultSigner(t *testing.T) {
	ctx := context.Background()
	srv, pub, signs := fakeVault(t, "s.tok")
	tokFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokFile, []byte("s.tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := NewVaultSigner(ctx, VaultConfig{Addr: srv.URL, Key: "transit/report", TokenFile: tokFile})
	if err != nil {
		t.Fatal(err)
	}
	m, sig, err := s.Sign(ctx, Manifest{ReportID: "r1", Result: Result{SHA256: SHA256([]byte("x"))}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(m, sig, []byte("x"), pub); err != nil || s.KeyID != KeyID(pub) || *signs != 2 { // self-test + manifest
		t.Fatalf("verify %v keyid %s signs %d", err, s.KeyID, *signs)
	}
	if _, err := NewVaultSigner(ctx, VaultConfig{Addr: srv.URL, Key: "transit/report", Token: "salah"}); err == nil || strings.Contains(err.Error(), "salah") {
		t.Errorf("wrong token: %v", err)
	}
	if _, err := NewVaultSigner(ctx, VaultConfig{Addr: srv.URL, Key: "report"}); err == nil {
		t.Error("key without mount accepted")
	}
}
