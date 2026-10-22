package server

import (
	"crypto/x509"
	"testing"
)

func TestSelfSignedTLS(t *testing.T) {
	cfg, err := SelfSignedTLS("db.internal", "10.0.0.5")
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(cfg.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"localhost", "127.0.0.1", "db.internal", "10.0.0.5"} {
		if err := leaf.VerifyHostname(h); err != nil {
			t.Errorf("VerifyHostname(%s): %v", h, err)
		}
	}
}
