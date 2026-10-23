//go:build duckdb_arrow

package flightsql

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bankmega/karhutlasql/internal/auth/oidc"
	"github.com/bankmega/karhutlasql/internal/authz"
	"github.com/bankmega/karhutlasql/internal/classification"
)

const govPolicy = `
default: deny
allow: { domains: [bankmega.com] }
roles:
  admin:      { catalogs: { "*": [read, write, admin] }, unmask: ["*"] }
  analyst:    { catalogs: { warehouse: [read], sandbox: [read, write] } }
  cabang:
    catalogs: { warehouse.main.nasabah: [read] }
    row_filters: { warehouse.main.nasabah: "branch_code = {{claim.branch}}" }
  ai_sandbox:
    catalogs: { warehouse: [read], sandbox: [read, write] }
    select_only: true
    max_rows: 3
    max_bytes: 200000
subjects:
  - { match: { claim: groups, equals: ops }, roles: [admin] }
  - { match: { claim: groups, equals: analyst }, roles: [analyst] }
  - { match: { claim: groups, equals: cabang }, roles: [cabang] }
  - { match: { claim: groups, equals: agent }, roles: [ai_sandbox] }
`

const govClassification = `
tags:
  pii.nik:     { mask: partial }
  pii.phone:   { mask: hash_hmac }
  fin.balance: { mask: "null" }
columns:
  warehouse.main.nasabah.nik: pii.nik
  warehouse.main.nasabah.phone: pii.phone
  warehouse.main.nasabah.saldo: fin.balance
`

const govInit = `ATTACH ':memory:' AS warehouse; ATTACH ':memory:' AS sandbox;
CREATE TABLE warehouse.main.nasabah AS
  SELECT '{{SECRET}}' || lpad(i::VARCHAR, 4, '0') AS nik, 'Nama ' || i AS nama,
         '0812' || i AS phone, CASE WHEN i % 2 = 0 THEN '0123' ELSE '0456' END AS branch_code,
         (i * 1000)::DECIMAL(15,2) AS saldo
  FROM range(1, 11) t(i);
CREATE TABLE sandbox.main.s (x VARCHAR);`

type govEnv struct {
	*testEnv
	tok    map[string]string
	secret string // prefix NIK mentah, acak per env: tidak bisa ditebak query
}

func startGov(t testing.TB) *govEnv {
	t.Helper()
	idp := newIdP(t)
	v, err := oidc.New(context.Background(), oidc.Config{Issuer: idp.srv.URL, Audience: "karhutlasql", ClockSkew: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	p, err := authz.Parse([]byte(govPolicy))
	if err != nil {
		t.Fatal(err)
	}
	cpath := filepath.Join(t.TempDir(), "classification.yaml")
	if err := os.WriteFile(cpath, []byte(govClassification), 0o600); err != nil {
		t.Fatal(err)
	}
	cls, err := classification.Load(cpath)
	if err != nil {
		t.Fatal(err)
	}
	secret := newNonce() + "-"
	env := start(t, envOpts{initSQL: strings.ReplaceAll(govInit, "{{SECRET}}", secret), policy: authz.Static(p), oidc: v, classification: cls, maskKey: []byte("0123456789abcdef0123456789abcdef")})
	tok := map[string]string{}
	for user, claims := range map[string]jwt.MapClaims{
		"ops":     {"groups": []string{"ops"}},
		"analyst": {"groups": []string{"analyst"}},
		"cabang":  {"groups": []string{"cabang"}, "branch": "0123"},
		"nobr":    {"groups": []string{"cabang"}},
		"agent":   {"groups": []string{"agent"}},
	} {
		claims["email"], claims["sid"], claims["preferred_username"] = user+"@bankmega.com", "s-"+user, user
		tok[user] = idp.token(t, claims)
	}
	return &govEnv{testEnv: env, tok: tok, secret: secret}
}

// rows menjalankan sql sebagai user; setiap baris = kolom digabung "|".
func (g *govEnv) rows(user, sql string) ([]string, error) {
	ctx := bearer(g.tok[user])
	info, err := g.cl.Execute(ctx, sql)
	if err != nil {
		return nil, err
	}
	rdr, err := g.cl.DoGet(ctx, info.Endpoint[0].Ticket)
	if err != nil {
		return nil, err
	}
	defer rdr.Release()
	var out []string
	for rdr.Next() {
		rec := rdr.RecordBatch()
		for r := range int(rec.NumRows()) {
			var b strings.Builder
			for _, c := range rec.Columns() {
				b.WriteString(c.ValueStr(r) + "|")
			}
			out = append(out, b.String())
		}
	}
	return out, rdr.Err()
}

func TestGovernance_Masking(t *testing.T) {
	g := startGov(t)
	got, err := g.rows("analyst", "SELECT nik, nama, phone, saldo FROM warehouse.nasabah ORDER BY nama LIMIT 1")
	if err != nil || len(got) != 1 {
		t.Fatalf("analyst: %v %v", got, err)
	}
	f := strings.Split(got[0], "|")
	if f[0] != strings.Repeat("*", len(g.secret))+"0001" || f[1] != "Nama 1" || len(f[2]) != 64 || f[3] != "(null)" {
		t.Errorf("masked row = %q", got[0])
	}
	if got, err := g.rows("ops", "SELECT nik FROM warehouse.nasabah ORDER BY nik LIMIT 1"); err != nil || got[0] != g.secret+"0001|" {
		t.Errorf("admin unmasked = %v %v", got, err)
	}
	// HMAC deterministik: hasil sama untuk nilai sama (join/group-by tetap bisa).
	a, _ := g.rows("analyst", "SELECT phone FROM warehouse.nasabah WHERE nama = 'Nama 3'")
	b, _ := g.rows("analyst", "SELECT phone FROM warehouse.nasabah WHERE nama = 'Nama 3'")
	if len(a) != 1 || a[0] != b[0] {
		t.Errorf("hmac not deterministic: %v %v", a, b)
	}
}

// bypassQueries upaya membaca NIK mentah; setiap hasil harus ditolak atau ter-mask.
var bypassQueries = []string{
	"SELECT * FROM warehouse.nasabah",
	"SELECT nik FROM warehouse.main.nasabah WHERE nik = 'RAW-NIK-0001'",
	"SELECT count(*) FROM warehouse.nasabah WHERE nik LIKE 'RAW%'",
	"FROM warehouse.nasabah SELECT nik",
	"WITH x AS (SELECT * FROM warehouse.nasabah) SELECT nik FROM x",
	"SELECT (SELECT max(nik) FROM warehouse.nasabah)",
	"SELECT COLUMNS(*) FROM warehouse.nasabah",
	"SELECT struct_pack(n.*) FROM warehouse.nasabah n",
	"SELECT string_agg(nik, ',') FROM warehouse.nasabah",
	"SELECT a.nik FROM warehouse.nasabah a JOIN warehouse.nasabah b ON a.nik = b.nik",
	"SELECT nik FROM warehouse.nasabah UNION SELECT 'x'",
	"WITH nasabah AS (SELECT * FROM warehouse.nasabah) SELECT nik FROM nasabah",
	"SELECT karhutla_mask_hmac('RAW-NIK-0001', 'guess')",
	"PIVOT warehouse.nasabah ON branch_code USING max(nik)",
	"SELECT nik FROM warehouse.nasabah USING SAMPLE 100%",
	"SELECT * FROM warehouse.nasabah AT (VERSION => 1)",
	"EXPLAIN SELECT nik FROM warehouse.nasabah",
	"SELECT nik FROM warehouse.nasabah t(nik_alias) WHERE true",
}

func TestGovernance_Bypass(t *testing.T) {
	g := startGov(t)
	for _, q := range bypassQueries {
		got, err := g.rows("analyst", q)
		if strings.Contains(strings.Join(got, "\n"), g.secret) {
			t.Errorf("LEAK %s → %v", q, got)
		}
		if err != nil {
			t.Logf("rejected %s: %v", q, status.Convert(err).Message())
		}
	}
	// View di sandbox atas tabel ter-govern: tidak boleh jadi jalan pintas.
	if _, err := g.cl.ExecuteUpdate(bearer(g.tok["analyst"]), "CREATE VIEW sandbox.main.v AS SELECT * FROM warehouse.main.nasabah"); err != nil {
		t.Fatal(err)
	}
	if got, err := g.rows("analyst", "SELECT nik FROM sandbox.v"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("view bypass: %v %v", got, err)
	}
	// Tulis dari tabel ter-govern → ditolak (hanya SELECT yang di-rewrite).
	if _, err := g.cl.ExecuteUpdate(bearer(g.tok["analyst"]), "INSERT INTO sandbox.main.s SELECT nik FROM warehouse.main.nasabah"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("insert-select governed: %v", err)
	}
	// Prepared statement juga ter-mask.
	stmt, err := g.cl.Prepare(bearer(g.tok["analyst"]), "SELECT nik FROM warehouse.nasabah ORDER BY nik LIMIT 1")
	if err != nil {
		t.Fatal(err)
	}
	info, err := stmt.Execute(bearer(g.tok["analyst"]))
	if err != nil {
		t.Fatal(err)
	}
	rdr, err := g.cl.DoGet(bearer(g.tok["analyst"]), info.Endpoint[0].Ticket)
	if err != nil {
		t.Fatal(err)
	}
	for rdr.Next() {
		if v := rdr.RecordBatch().Column(0).ValueStr(0); strings.Contains(v, g.secret) {
			t.Errorf("prepared leak %q", v)
		}
	}
	rdr.Release()
}

func TestGovernance_RLS(t *testing.T) {
	g := startGov(t)
	got, err := g.rows("cabang", "SELECT DISTINCT branch_code FROM warehouse.nasabah")
	if err != nil || len(got) != 1 || got[0] != "0123|" {
		t.Errorf("cabang branches = %v %v", got, err)
	}
	if got, err := g.rows("cabang", "SELECT count(*) FROM warehouse.nasabah WHERE branch_code = '0456'"); err != nil || got[0] != "0|" {
		t.Errorf("cabang other branch = %v %v", got, err)
	}
	if _, err := g.rows("nobr", "SELECT count(*) FROM warehouse.nasabah"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("missing branch claim: %v", err)
	}
	if got, err := g.rows("ops", "SELECT count(DISTINCT branch_code) FROM warehouse.nasabah"); err != nil || got[0] != "2|" {
		t.Errorf("admin unfiltered = %v %v", got, err)
	}
}

func TestGovernance_AISandbox(t *testing.T) {
	g := startGov(t)
	if got, err := g.rows("agent", "SELECT * FROM warehouse.nasabah"); err != nil || len(got) != 3 {
		t.Errorf("agent rows = %d %v", len(got), err)
	}
	if got, err := g.rows("agent", "SELECT * FROM warehouse.nasabah LIMIT 2"); err != nil || len(got) != 2 {
		t.Errorf("agent own limit = %d %v", len(got), err)
	}
	for _, q := range []string{"INSERT INTO sandbox.main.s VALUES ('x')", "COPY warehouse.main.nasabah TO '/tmp/x.csv'"} {
		if _, err := g.cl.ExecuteUpdate(bearer(g.tok["agent"]), q); status.Code(err) != codes.PermissionDenied {
			t.Errorf("agent %s: %v", q, err)
		}
	}
	if _, err := g.rows("agent", "SELECT repeat('x', 100000) FROM range(3)"); status.Code(err) != codes.ResourceExhausted {
		t.Errorf("agent bytes cap: %v", err)
	}
}

// FuzzGovernance: properti — query apa pun dari analyst tidak pernah
// mengembalikan NIK mentah (ditolak atau ter-mask).
func FuzzGovernance(f *testing.F) {
	for _, q := range bypassQueries {
		f.Add(q)
	}
	g := startGov(f) // satu server per proses worker fuzz
	f.Fuzz(func(t *testing.T, q string) {
		got, _ := g.rows("analyst", q)
		if s := strings.Join(got, "\n"); strings.Contains(s, g.secret) {
			t.Fatalf("LEAK %q → %s", q, s)
		}
	})
}
