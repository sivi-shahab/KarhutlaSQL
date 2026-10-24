//go:build duckdb_arrow

package flightsql

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/bankmega/karhutlasql/internal/audit"
	"github.com/bankmega/karhutlasql/internal/auth/oidc"
	"github.com/bankmega/karhutlasql/internal/authz"
)

const testPolicy = `
default: deny
allow: { domains: [bankmega.com] }
roles:
  admin:   { catalogs: { "*": [read, write, admin] } }
  analyst: { catalogs: { warehouse: [read], sandbox: [read, write] } }
subjects:
  - match: { claim: groups, equals: data-platform }
    roles: [admin]
  - match: { claim: email, endswith: "@bankmega.com" }
    roles: [analyst]
  - match: { user: admin }
    roles: [analyst]
`

const authzInit = `ATTACH ':memory:' AS warehouse; ATTACH ':memory:' AS sandbox; ATTACH ':memory:' AS secret;
	CREATE TABLE warehouse.t AS SELECT range AS i FROM range(5);
	CREATE TABLE sandbox.s (i BIGINT); CREATE TABLE secret.k AS SELECT 42 AS i;`

func policyStore(t *testing.T) *authz.Store {
	t.Helper()
	p, err := authz.Parse([]byte(testPolicy))
	if err != nil {
		t.Fatal(err)
	}
	return authz.Static(p)
}

func TestAuthz_LocalAnalyst(t *testing.T) {
	env := start(t, envOpts{initSQL: authzInit, policy: policyStore(t)})
	tests := []struct {
		sql  string
		code codes.Code
	}{
		{"SELECT count(*) FROM warehouse.t", codes.OK},
		{"FROM warehouse.t SELECT *", codes.OK}, // sintaks khusus DuckDB di jalur baca
		{"SELECT * FROM secret.k", codes.PermissionDenied},
		{"SELECT * FROM secret.k WHERE i = 1 AND false", codes.PermissionDenied}, // scan yang dibuang optimizer
		{"SELECT * FROM range(3)", codes.OK},
		{"SELECT * FROM read_csv('/etc/passwd')", codes.PermissionDenied},
		{"SELECT * FROM duckdb_settings()", codes.PermissionDenied},
		{"ATTACH ':memory:' AS z", codes.PermissionDenied},
		{"SET threads = 1", codes.PermissionDenied},
		{"COPY warehouse.t TO '/tmp/x.csv'", codes.PermissionDenied},
		{"SELECT 1; DROP TABLE sandbox.s", codes.InvalidArgument},
	}
	for _, tt := range tests {
		_, err := drain(env.ctx, env, tt.sql)
		if code := status.Code(err); code != tt.code {
			t.Errorf("%s: code %v (%v), want %v", tt.sql, code, err, tt.code)
		}
	}
	updates := []struct {
		sql  string
		code codes.Code
	}{
		{"INSERT INTO warehouse.t VALUES (9)", codes.PermissionDenied},
		{"INSERT INTO sandbox.s VALUES (1)", codes.OK},
		{"INSERT INTO sandbox.main.s SELECT * FROM warehouse.t", codes.OK},
		{"INSERT INTO sandbox.main.s SELECT * FROM secret.k", codes.PermissionDenied},
		{"CREATE TABLE sandbox.main.x AS SELECT * FROM warehouse.t", codes.OK},
		{"DROP TABLE warehouse.main.t", codes.PermissionDenied},
		{"INSERT INTO sandbox.main.s FROM warehouse.t", codes.PermissionDenied}, // target tak bisa di-parse → tolak
	}
	for _, tt := range updates {
		_, err := env.cl.ExecuteUpdate(env.ctx, tt.sql)
		if code := status.Code(err); code != tt.code {
			t.Errorf("%s: code %v (%v), want %v", tt.sql, code, err, tt.code)
		}
	}

	// Prepared statement: ditolak saat create, sebelum Describe mengeksekusi LIMIT 0.
	if _, err := env.cl.Prepare(env.ctx, "SELECT * FROM secret.k WHERE i = ?"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("prepare secret: %v", err)
	}

	// Metadata hanya catalog yang boleh dibaca.
	info, err := env.cl.GetCatalogs(env.ctx)
	if err != nil {
		t.Fatal(err)
	}
	rdr, err := env.cl.DoGet(env.ctx, info.Endpoint[0].Ticket)
	if err != nil {
		t.Fatal(err)
	}
	var cats []string
	for rdr.Next() {
		col := rdr.RecordBatch().Column(0)
		for i := range col.Len() {
			cats = append(cats, col.ValueStr(i))
		}
	}
	rdr.Release()
	slices.Sort(cats)
	if !slices.Equal(cats, []string{"sandbox", "warehouse"}) {
		t.Errorf("catalogs = %v", cats)
	}
	if n := len(env.audit.ofType(audit.AuthzDeny)); n < 8 {
		t.Errorf("authz_deny events = %d", n)
	}
}

// fakeIdP issuer OIDC minimal: discovery + JWKS, token ditandatangani RS256.
type fakeIdP struct {
	srv *httptest.Server
	key *rsa.PrivateKey
}

func newIdP(t testing.TB) *fakeIdP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &fakeIdP{key: key}
	mux := http.NewServeMux()
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": idp.srv.URL, "jwks_uri": idp.srv.URL + "/keys",
			"authorization_endpoint": idp.srv.URL + "/auth", "token_endpoint": idp.srv.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	return idp
}

func (i *fakeIdP) token(t testing.TB, claims jwt.MapClaims) string {
	t.Helper()
	base := jwt.MapClaims{"iss": i.srv.URL, "aud": "karhutlasql", "exp": time.Now().Add(time.Hour).Unix(),
		"iat": time.Now().Unix(), "sub": "u1", "sid": "sid-1", "email_verified": true}
	for k, v := range claims {
		base[k] = v
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, base)
	tok.Header["kid"] = "k1"
	s, err := tok.SignedString(i.key)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func bearer(tok string) context.Context {
	return metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer "+tok)
}

func TestAuthz_OIDC(t *testing.T) {
	idp := newIdP(t)
	v, err := oidc.New(context.Background(), oidc.Config{Issuer: idp.srv.URL, Audience: "karhutlasql", ClockSkew: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	env := start(t, envOpts{initSQL: authzInit, policy: policyStore(t), oidc: v})

	ana := idp.token(t, jwt.MapClaims{"preferred_username": "ana", "email": "ana@bankmega.com", "sid": "s-ana"})
	ops := idp.token(t, jwt.MapClaims{"preferred_username": "dian", "email": "dian@bankmega.com", "groups": []string{"data-platform"}, "sid": "s-ops"})
	tests := []struct {
		name, tok, sql string
		code           codes.Code
	}{
		{"analyst read", ana, "SELECT count(*) FROM warehouse.t", codes.OK},
		{"analyst secret", ana, "SELECT * FROM secret.k", codes.PermissionDenied},
		{"admin group", ops, "SELECT * FROM secret.k", codes.OK},
		{"outside allowlist", idp.token(t, jwt.MapClaims{"email": "eve@gmail.com", "sid": "s-eve"}), "SELECT 1", codes.PermissionDenied},
		{"unverified email", idp.token(t, jwt.MapClaims{"email": "mallory@bankmega.com", "email_verified": false, "sid": "s-m"}), "SELECT 1", codes.PermissionDenied},
		{"wrong audience", idp.token(t, jwt.MapClaims{"email": "ana@bankmega.com", "aud": "other"}), "SELECT 1", codes.Unauthenticated},
		{"expired", idp.token(t, jwt.MapClaims{"email": "ana@bankmega.com", "exp": time.Now().Add(-2 * time.Minute).Unix()}), "SELECT 1", codes.Unauthenticated},
		{"unknown issuer", idp.token(t, jwt.MapClaims{"email": "ana@bankmega.com", "iss": "https://evil.example"}), "SELECT 1", codes.Unauthenticated},
	}
	for _, tt := range tests {
		_, err := drain(bearer(tt.tok), env, tt.sql)
		if code := status.Code(err); code != tt.code {
			t.Errorf("%s: code %v (%v), want %v", tt.name, code, err, tt.code)
		}
	}
	if _, err := env.cl.ExecuteUpdate(bearer(ana), "INSERT INTO warehouse.t VALUES (1)"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("analyst insert warehouse: %v", err)
	}
	if _, err := env.cl.ExecuteUpdate(bearer(ana), "INSERT INTO sandbox.main.s VALUES (1)"); err != nil {
		t.Errorf("analyst insert sandbox: %v", err)
	}

	// Kill sesi OIDC (sid) → token hasil refresh dengan sid yang sama juga ditolak.
	if killed, err := env.sessions.Kill(context.Background(), "oidc:s-ana"); !killed || err != nil {
		t.Fatal("OIDC session not registered under sid")
	}
	refreshed := idp.token(t, jwt.MapClaims{"preferred_username": "ana", "email": "ana@bankmega.com", "sid": "s-ana", "exp": time.Now().Add(2 * time.Hour).Unix()})
	if _, err := drain(bearer(refreshed), env, "SELECT 1"); status.Code(err) != codes.Unauthenticated {
		t.Errorf("refreshed token of killed sid: %v", err)
	}
}
