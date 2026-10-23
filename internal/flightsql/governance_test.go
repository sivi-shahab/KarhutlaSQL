//go:build duckdb_arrow

package flightsql

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	"github.com/golang-jwt/jwt/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bankmega/karhutlasql/internal/accesslog"
	"github.com/bankmega/karhutlasql/internal/audit"
	"github.com/bankmega/karhutlasql/internal/auth/oidc"
	"github.com/bankmega/karhutlasql/internal/authz"
	"github.com/bankmega/karhutlasql/internal/classification"
	"github.com/bankmega/karhutlasql/internal/objstore"
	"github.com/bankmega/karhutlasql/internal/report"
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
	tok     map[string]string
	access  *accesslog.Log
	archive objstore.Dir
	pub     ed25519.PublicKey
	secret  string // prefix NIK mentah, acak per env: tidak bisa ditebak query
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
	archive := objstore.Dir(t.TempDir())
	al := accesslog.New(accesslog.Options{Store: archive})
	signer, pub := testSigner(t)
	env := start(t, envOpts{initSQL: strings.ReplaceAll(govInit, "{{SECRET}}", secret), policy: authz.Static(p), oidc: v, classification: cls,
		maskKey: []byte("0123456789abcdef0123456789abcdef"), accessLog: al, archive: archive, signer: signer, reports: govReports})
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
	return &govEnv{testEnv: env, tok: tok, secret: secret, access: al, archive: archive, pub: pub}
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

// Acceptance #7 bagian kedua (G-05): access-history mencatat kolom & policy.
func TestGovernance_AccessHistory(t *testing.T) {
	g := startGov(t)
	if _, err := g.rows("analyst", "SELECT nik, nama FROM warehouse.nasabah WHERE phone <> ''"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.rows("cabang", "SELECT count(*) FROM warehouse.nasabah"); err != nil {
		t.Fatal(err)
	}
	if err := g.access.Flush(context.Background()); err != nil { // sebagian dari penyimpanan, sebagian buffer
		t.Fatal(err)
	}
	if _, err := g.cl.ExecuteUpdate(bearer(g.tok["analyst"]), "INSERT INTO sandbox.main.s VALUES ('x')"); err != nil {
		t.Fatal(err)
	}
	const q = `SELECT concat_ws('|', user, action, table_name, coalesce(column_name, '-'), coalesce(tag, '-'), coalesce(policy, '-'), coalesce(row_filter, '-'))
		FROM karhutla_access_history() WHERE status = 'OK' AND table_name NOT LIKE 'karhutla%' ORDER BY ALL`
	got, err := g.rows("ops", q)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"analyst|read|warehouse.main.nasabah|nik|pii.nik|partial|-|",
		"analyst|read|warehouse.main.nasabah|nama|-|-|-|",
		"analyst|read|warehouse.main.nasabah|phone|pii.phone|hash_hmac|-|", // kolom filter ikut tercatat
		"analyst|write|sandbox.main.s|-|-|-|-|",
		"cabang|read|warehouse.main.nasabah|branch_code|-|-|(branch_code = '0123')|",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, strings.Join(got, "\n"))
		}
	}
	// User non-admin hanya melihat miliknya.
	mine, err := g.rows("cabang", q)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range mine {
		if !strings.HasPrefix(r, "cabang|") {
			t.Errorf("cabang sees %q", r)
		}
	}
	if n := len(g.audit.ofType("mask_applied")); n == 0 {
		t.Error("no mask_applied audit event")
	}
	if ev := g.audit.ofType("row_filter_applied"); len(ev) == 0 || ev[0].Object != "warehouse.main.nasabah" {
		t.Errorf("row_filter_applied = %+v", ev)
	}
}

func TestGovernance_AccessHistoryRange(t *testing.T) {
	g := startGov(t)
	if _, err := g.rows("analyst", "SELECT nik FROM warehouse.nasabah"); err != nil {
		t.Fatal(err)
	}
	for q, want := range map[string]string{
		"SELECT count(*) > 0 FROM karhutla_access_history_range(current_date - 1, current_date + 1) WHERE column_name = 'nik'": "true|",
		"SELECT count(*) FROM karhutla_access_history_range(DATE '2020-01-01', DATE '2020-01-02') WHERE column_name = 'nik'":   "0|",
	} {
		if got, err := g.rows("ops", q); err != nil || len(got) != 1 || got[0] != want {
			t.Errorf("%s = %v %v", q, got, err)
		}
	}
}

var govReports = map[string]report.Def{
	"nasabah_cabang": {Roles: []string{"analyst", "admin"}, SQL: "SELECT nik, branch_code FROM warehouse.main.nasabah ORDER BY nama",
		Asserts: []report.Assert{{Name: "ada_data", SQL: "SELECT count(*) = 10 FROM warehouse.main.nasabah"}}},
	"gagal_gate": {Roles: []string{"analyst"}, SQL: "SELECT 1",
		Asserts: []report.Assert{{Name: "nik_kosong", SQL: "SELECT count(*) = 0 FROM warehouse.main.nasabah"}}},
}

func testSigner(t testing.TB) (*report.Signer, ed25519.PublicKey) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(priv)
	p := filepath.Join(t.TempDir(), "report.key")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := report.LoadSigner(p)
	if err != nil {
		t.Fatal(err)
	}
	return s, pub
}

// Acceptance #10 (G-10): report terikat snapshot, manifest bertanda tangan.
func TestReportRun(t *testing.T) {
	g := startGov(t)
	got, err := g.rows("analyst", "CALL karhutla_report_run('nasabah_cabang')")
	if err != nil || len(got) != 1 {
		t.Fatalf("report: %v %v", got, err)
	}
	f := strings.Split(got[0], "|") // report_id|name|snapshot|rows|location|result_sha|manifest_sha|
	if f[3] != "10" || !strings.HasPrefix(f[4], "reports/nasabah_cabang/") {
		t.Fatalf("row %q", got[0])
	}
	read := func(k string) []byte {
		b, err := g.archive.Get(context.Background(), f[4]+k)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	result := read("result.parquet")
	m, err := report.Verify(read("manifest.json"), read("manifest.json.sig"), result, g.pub)
	if err != nil {
		t.Fatal(err)
	}
	if m.SnapshotID != f[2] || m.User != "analyst" || len(m.Asserts) != 1 || !m.Asserts[0].Passed || m.Tables["warehouse.main.nasabah"] == "" {
		t.Errorf("manifest %+v", m)
	}
	// Hasil dieksekusi sebagai pemanggil: NIK ter-mask.
	tbl, err := pqarrow.ReadTable(context.Background(), bytes.NewReader(result), nil, pqarrow.ArrowReadProperties{}, memory.DefaultAllocator)
	if err != nil {
		t.Fatal(err)
	}
	defer tbl.Release()
	nik := tbl.Column(0).Data().Chunk(0).(*array.String)
	if v := nik.Value(0); strings.Contains(v, g.secret) || !strings.HasPrefix(v, "***") {
		t.Errorf("report nik not masked: %q", v)
	}
	// Snapshot sama selama data sama.
	again, _ := g.rows("ops", "CALL karhutla_report_run('nasabah_cabang')")
	if strings.Split(again[0], "|")[2] != f[2] {
		t.Errorf("snapshot id changed without data change: %s vs %s", again[0], f[2])
	}

	// DQ gate gagal → FAILED_PRECONDITION + audit.
	if _, err := g.rows("analyst", "CALL karhutla_report_run('gagal_gate')"); status.Code(err) != codes.FailedPrecondition || !strings.Contains(err.Error(), "nik_kosong") {
		t.Errorf("gate: %v", err)
	}
	// Role tidak diizinkan.
	if _, err := g.rows("cabang", "SELECT * FROM karhutla_report_run('nasabah_cabang')"); status.Code(err) != codes.PermissionDenied {
		t.Errorf("role: %v", err)
	}
	var statuses []string
	for _, e := range g.audit.ofType(audit.ReportRun) {
		statuses = append(statuses, e.Status)
	}
	if strings.Join(statuses, ",") != "OK,OK,FailedPrecondition,PermissionDenied" {
		t.Errorf("report_run audit %v", statuses)
	}
}
