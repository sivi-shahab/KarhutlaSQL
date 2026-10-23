package report

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

func keyFiles(t *testing.T) (string, []byte) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	p := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	pder, _ := x509.MarshalPKIXPublicKey(pub)
	return p, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pder})
}

func TestSignVerify(t *testing.T) {
	keyPath, pubPEM := keyFiles(t)
	s, err := LoadSigner(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ParsePublicKey(pubPEM)
	if err != nil {
		t.Fatal(err)
	}
	b := array.NewRecordBuilder(memory.DefaultAllocator, arrow.NewSchema([]arrow.Field{{Name: "x", Type: arrow.PrimitiveTypes.Int64}}, nil))
	b.Field(0).(*array.Int64Builder).AppendValues([]int64{1, 2, 3}, nil)
	rec := b.NewRecordBatch()
	rdr, _ := array.NewRecordReader(rec.Schema(), []arrow.RecordBatch{rec})
	result, rows, err := WriteParquet(rdr)
	if err != nil || rows != 3 {
		t.Fatal(rows, err)
	}
	m, sig, err := s.Sign(Manifest{ReportID: "r1", Name: "slik", Result: Result{File: "result.parquet", SHA256: SHA256(result), Rows: rows}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Verify(m, sig, result, pub); err != nil || got.ReportID != "r1" || got.KeyID != s.KeyID {
		t.Fatalf("verify: %+v %v", got, err)
	}
	tampered := append([]byte{}, result...)
	tampered[len(tampered)/2] ^= 1
	if _, err := Verify(m, sig, tampered, pub); err == nil {
		t.Error("tampered result accepted")
	}
	if _, err := Verify([]byte(strings.Replace(string(m), `"rows": 3`, `"rows": 4`, 1)), sig, result, pub); err == nil {
		t.Error("tampered manifest accepted")
	}
	_, otherPub := keyFiles(t)
	op, _ := ParsePublicKey(otherPub)
	if _, err := Verify(m, sig, result, op); err == nil {
		t.Error("wrong key accepted")
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "r.yaml")
		_ = os.WriteFile(p, []byte(s), 0o600)
		return p
	}
	defs, err := Load(write("reports:\n  slik_bulanan:\n    roles: [admin]\n    sql: SELECT 1\n    asserts: [{ name: ok, sql: SELECT true }]\n"))
	if err != nil || len(defs["slik_bulanan"].Asserts) != 1 {
		t.Fatal(defs, err)
	}
	for _, bad := range []string{
		"reports:\n  Bad-Name: { roles: [admin], sql: SELECT 1 }\n",
		"reports:\n  r: { roles: [admin] }\n",
		"reports:\n  r: { sql: SELECT 1 }\n",
		"reports:\n  r: { roles: [a], sql: SELECT 1, asserts: [{ name: x }] }\n",
		"reports:\n  r: { roles: [a], sql: SELECT 1, unknown: 1 }\n",
	} {
		if _, err := Load(write(bad)); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
