package local

import (
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"golang.org/x/crypto/bcrypt"
)

var secret = []byte("0123456789abcdef0123456789abcdef")

func minCostHash(pw string) string {
	h, _ := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.MinCost)
	return string(h)
}

func newAuth(t *testing.T, cfg Config) *Authenticator {
	t.Helper()
	if cfg.Username == "" {
		cfg.Username = "admin"
	}
	if cfg.Password == "" {
		cfg.Password = minCostHash("s3cret")
	}
	if cfg.TTL == 0 {
		cfg.TTL = time.Hour
	}
	if cfg.HMACSecret == nil && cfg.RSAKey == nil {
		cfg.HMACSecret = secret
	}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestLogin(t *testing.T) {
	hash, _ := bcrypt.GenerateFromPassword([]byte("hashed-pw"), bcrypt.MinCost)
	tests := []struct {
		name, cfgPassword, user, pass string
		wantErr                       bool
	}{
		{"ok plaintext", "s3cret", "admin", "s3cret", false},
		{"ok bcrypt hash", string(hash), "admin", "hashed-pw", false},
		{"wrong password", minCostHash("s3cret"), "admin", "nope", true},
		{"wrong user", minCostHash("s3cret"), "root", "s3cret", true},
		{"empty password", minCostHash("s3cret"), "admin", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := newAuth(t, Config{Password: tt.cfgPassword})
			tok, id, err := a.Login(tt.user, tt.pass)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidCredentials) {
					t.Fatalf("err = %v, want ErrInvalidCredentials", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			got, err := a.Verify(tok)
			if err != nil || got.Subject != "admin" || got.TokenID != id.TokenID || got.Source != "local" {
				t.Fatalf("verify = %+v err %v", got, err)
			}
		})
	}
}

func TestVerify_Rejects(t *testing.T) {
	now := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	clock := now
	a := newAuth(t, Config{Now: func() time.Time { return clock }})
	tok, _, err := a.Login("admin", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	other := newAuth(t, Config{HMACSecret: []byte(strings.Repeat("x", 32))})
	otherTok, _, _ := other.Login("admin", "s3cret")
	none, _ := jwt.NewWithClaims(jwt.SigningMethodNone, jwt.RegisteredClaims{Issuer: Issuer, Subject: "admin"}).SignedString(jwt.UnsafeAllowNoneSignatureType)
	parts := strings.Split(tok, ".")
	tampered := parts[0] + "." + parts[1] + "x." + parts[2]

	tests := []struct {
		name  string
		token string
		at    time.Time
	}{
		{"expired", tok, now.Add(2 * time.Hour)},
		{"before issue", tok, now.Add(-time.Hour)},
		{"other key", otherTok, now},
		{"alg none", none, now},
		{"tampered payload", tampered, now},
		{"garbage", "not.a.jwt", now},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			clock = tt.at
			if _, err := a.Verify(tt.token); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestRS256(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	a := newAuth(t, Config{RSAKey: key})
	tok, _, err := a.Login("admin", "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Verify(tok); err != nil {
		t.Fatal(err)
	}
	hs := newAuth(t, Config{})
	if _, err := hs.Verify(tok); err == nil {
		t.Fatal("HS256 authenticator must reject RS256 token")
	}
}

func TestNew_Invalid(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	tests := []struct {
		name string
		cfg  Config
	}{
		{"no password", Config{Username: "a", TTL: time.Hour, HMACSecret: secret}},
		{"short secret", Config{Username: "a", Password: "p", TTL: time.Hour, HMACSecret: []byte("short")}},
		{"ttl too long", Config{Username: "a", Password: "p", TTL: 25 * time.Hour, HMACSecret: secret}},
		{"both keys", Config{Username: "a", Password: "p", TTL: time.Hour, HMACSecret: secret, RSAKey: key}},
		{"reserved job prefix", Config{Username: "job:x", Password: "p", TTL: time.Hour, HMACSecret: secret}},
		{"reserved share prefix", Config{Username: "share:x", Password: "p", TTL: time.Hour, HMACSecret: secret}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.cfg); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestIssueJob(t *testing.T) {
	a := newAuth(t, Config{})
	tok, id, err := a.IssueJob("mart")
	if err != nil || id.Subject != "job:mart" {
		t.Fatalf("IssueJob = %+v %v", id, err)
	}
	got, err := a.Verify(tok)
	if err != nil || got.Subject != "job:mart" || got.TokenID != id.TokenID {
		t.Fatalf("Verify = %+v %v", got, err)
	}
	// Login dengan nama job tidak mungkin (bukan --username).
	if _, _, err := a.Login("job:mart", "s3cret"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("login as job: %v", err)
	}
	if _, _, err := a.IssueJob(""); err == nil {
		t.Fatal("empty job name accepted")
	}
	if _, id, err := a.IssueShare("ojk"); err != nil || id.Subject != "share:ojk" {
		t.Fatalf("IssueShare = %+v %v", id, err)
	}
}
