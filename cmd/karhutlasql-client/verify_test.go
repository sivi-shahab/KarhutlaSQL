package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bankmega/karhutlasql/internal/report"
)

func TestVerifyReport(t *testing.T) {
	dir := t.TempDir()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	keyPath := filepath.Join(dir, "k.pem")
	_ = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600)
	pder, _ := x509.MarshalPKIXPublicKey(pub)
	pubPath := filepath.Join(dir, "pub.pem")
	_ = os.WriteFile(pubPath, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pder}), 0o600)
	s, err := report.LoadSigner(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	result := []byte("PAR1 fake result")
	m, sig, _ := s.Sign(report.Manifest{ReportID: "r1", Name: "slik_bulanan", Result: report.Result{File: "result.parquet", SHA256: report.SHA256(result), Rows: 7}})
	_ = os.WriteFile(filepath.Join(dir, "manifest.json"), m, 0o600)
	_ = os.WriteFile(filepath.Join(dir, "manifest.json.sig"), sig, 0o600)
	_ = os.WriteFile(filepath.Join(dir, "result.parquet"), result, 0o600)

	verify := func() (int, string) {
		var out, errOut bytes.Buffer
		code := run(context.Background(), []string{"--verify", filepath.Join(dir, "manifest.json"), "--public-key", pubPath}, nil, &out, &errOut)
		return code, out.String() + errOut.String()
	}
	if code, out := verify(); code != 0 || !strings.HasPrefix(out, "OK report r1 (slik_bulanan)") {
		t.Fatalf("verify = %d %s", code, out)
	}
	_ = os.WriteFile(filepath.Join(dir, "result.parquet"), []byte("PAR1 tampered"), 0o600)
	if code, out := verify(); code != 1 || !strings.Contains(out, "sha256") {
		t.Fatalf("tampered = %d %s", code, out)
	}
}
