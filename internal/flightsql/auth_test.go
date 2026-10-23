//go:build duckdb_arrow

package flightsql

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/flight"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/bankmega/karhutlasql/internal/audit"
	"github.com/bankmega/karhutlasql/internal/engine"
)

// drain menjalankan sql dan menguras stream; mengembalikan jumlah baris & error.
func drain(ctx context.Context, env *testEnv, sql string) (int64, error) {
	info, err := env.cl.Execute(ctx, sql)
	if err != nil {
		return 0, err
	}
	rdr, err := env.cl.DoGet(ctx, info.Endpoint[0].Ticket)
	if err != nil {
		return 0, err
	}
	defer rdr.Release()
	var n int64
	for rdr.Next() {
		n += rdr.RecordBatch().NumRows()
	}
	return n, rdr.Err()
}

func TestAuth_Login(t *testing.T) {
	env := start(t, envOpts{skipLogin: true})
	tests := []struct {
		name, user, pass string
		wantCode         codes.Code
	}{
		{"ok", testUser, testPass, codes.OK},
		{"wrong password", testUser, "nope", codes.Unauthenticated},
		{"unknown user", "root", testPass, codes.Unauthenticated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, err := env.cl.Client.AuthenticateBasicToken(context.Background(), tt.user, tt.pass)
			if code := status.Code(err); code != tt.wantCode {
				t.Fatalf("login code = %v (err %v), want %v", code, err, tt.wantCode)
			}
			if tt.wantCode != codes.OK {
				return
			}
			if _, err := drain(ctx, env, "SELECT 42"); err != nil {
				t.Fatalf("query after login: %v", err)
			}
		})
	}
	if n := len(env.audit.ofType(audit.LoginFail)); n != 2 {
		t.Errorf("login_fail events = %d, want 2", n)
	}
	if ok := env.audit.ofType(audit.LoginOK); len(ok) != 1 || ok[0].User != testUser || ok[0].Peer == "" {
		t.Errorf("login_ok events = %+v", ok)
	}
}

func TestAuth_RejectsWithoutValidToken(t *testing.T) {
	env := start(t, envOpts{})
	expired := func() context.Context {
		later := time.Now().Add(2 * time.Hour) // TTL 1h
		env.clock.Store(&later)
		return env.ctx
	}
	tests := []struct {
		name string
		ctx  func() context.Context
	}{
		{"no token", context.Background},
		{"garbage token", func() context.Context {
			return metadata.AppendToOutgoingContext(context.Background(), "authorization", "Bearer abc.def.ghi")
		}},
		{"basic on non-handshake", func() context.Context {
			return metadata.AppendToOutgoingContext(context.Background(), "authorization", "Basic YWRtaW46czNjcmV0LXB3")
		}},
		{"expired token", expired}, // terakhir: memajukan jam server
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := drain(tt.ctx(), env, "SELECT 1")
			if code := status.Code(err); code != codes.Unauthenticated {
				t.Fatalf("code = %v (err %v), want Unauthenticated", code, err)
			}
		})
	}
}

func TestQueryTimeout(t *testing.T) {
	env := start(t, envOpts{queryTimeout: 300 * time.Millisecond})
	// Catatan: duckdb-go mematerialisasi hasil sebelum batch pertama (ADR 17),
	// jadi keduanya gagal di engine.Query; kasus tengah-stream ada di
	// TestQueryTimeout_MidStream dengan engine palsu.
	tests := []string{
		"SELECT count(*) FROM range(1000000000000)",
		"SELECT * FROM range(1000000000000)",
	}
	for _, sql := range tests {
		t.Run(sql, func(t *testing.T) {
			t0 := time.Now()
			_, err := drain(env.ctx, env, sql)
			if code := status.Code(err); code != codes.DeadlineExceeded {
				t.Fatalf("code = %v (err %v), want DeadlineExceeded", code, err)
			}
			if d := time.Since(t0); d > 10*time.Second {
				t.Fatalf("timeout took %v", d)
			}
		})
	}
	ends := env.audit.ofType(audit.StatementEnd)
	if len(ends) != 2 || ends[0].Status != codes.DeadlineExceeded.String() || ends[1].Status != codes.DeadlineExceeded.String() {
		t.Fatalf("statement_end = %+v", ends)
	}
	if _, err := drain(env.ctx, env, "SELECT 42"); err != nil {
		t.Fatalf("server unusable after timeout: %v", err)
	}
}

func TestAudit_Statements(t *testing.T) {
	for _, withSQL := range []bool{false, true} {
		env := start(t, envOpts{auditSQL: withSQL})
		if n, err := drain(env.ctx, env, "SELECT * FROM range(10)"); err != nil || n != 10 {
			t.Fatalf("query n=%d err=%v", n, err)
		}
		if _, err := env.cl.ExecuteUpdate(env.ctx, "CREATE TABLE t (i INTEGER)"); err != nil {
			t.Fatal(err)
		}
		_, _ = drain(env.ctx, env, "SELEC broken")

		starts, ends := env.audit.ofType(audit.StatementStart), env.audit.ofType(audit.StatementEnd)
		if len(starts) != 3 || len(ends) != 3 {
			t.Fatalf("withSQL=%v starts=%d ends=%d", withSQL, len(starts), len(ends))
		}
		want := []struct {
			kind, status string
			rows         int64
		}{{"query", "OK", 10}, {"update", "OK", 0}, {"query", "InvalidArgument", 0}}
		for i, w := range want {
			e := ends[i]
			if e.User != testUser || e.Kind != w.kind || e.Status != w.status || e.Rows != w.rows || len(e.SQLHash) != 64 || e.RequestID == "" {
				t.Errorf("withSQL=%v end[%d] = %+v, want %+v", withSQL, i, e, w)
			}
			if (e.SQL != "") != withSQL || e.RequestID != starts[i].RequestID {
				t.Errorf("withSQL=%v end[%d] sql=%q req=%s/%s", withSQL, i, e.SQL, e.RequestID, starts[i].RequestID)
			}
		}
	}
}

func TestHealth_Unauthenticated(t *testing.T) {
	env := start(t, envOpts{skipLogin: true})
	conn, err := grpc.NewClient(env.addr, grpc.WithTransportCredentials(env.creds))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	resp, err := healthpb.NewHealthClient(conn).Check(context.Background(), &healthpb.HealthCheckRequest{})
	if err != nil || resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Fatalf("health = %v err %v", resp, err)
	}
}

func TestTLS(t *testing.T) {
	pki := newPKI(t)
	serverTLS := func(mtls bool) *tls.Config {
		c := &tls.Config{Certificates: []tls.Certificate{pki.server}, MinVersion: tls.VersionTLS12}
		if mtls {
			c.ClientCAs, c.ClientAuth = pki.pool, tls.RequireAndVerifyClientCert
		}
		return c
	}
	clientTLS := func(withCert bool) credentials.TransportCredentials {
		c := &tls.Config{RootCAs: pki.pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
		if withCert {
			c.Certificates = []tls.Certificate{pki.client}
		}
		return credentials.NewTLS(c)
	}
	tests := []struct {
		name    string
		mtls    bool
		client  credentials.TransportCredentials
		wantErr bool
	}{
		{"tls ok", false, clientTLS(false), false},
		{"plaintext to tls rejected", false, nil, true},
		{"mtls with client cert", true, clientTLS(true), false},
		{"mtls without client cert rejected", true, clientTLS(false), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			creds := tt.client
			if creds == nil {
				creds = insecureCreds()
			}
			env := start(t, envOpts{tls: serverTLS(tt.mtls), clientCreds: creds, skipLogin: true})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ctx, err := env.cl.Client.AuthenticateBasicToken(ctx, testUser, testPass)
			if err == nil {
				_, err = drain(ctx, env, "SELECT 1")
			}
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

// Pastikan klien memakai token dari header Handshake (pola klien Java/JDBC), bukan hanya trailer.
func TestHandshake_TokenInHeader(t *testing.T) {
	env := start(t, envOpts{skipLogin: true})
	ctx := metadata.AppendToOutgoingContext(context.Background(), "authorization", "Basic YWRtaW46czNjcmV0LXB3") // admin:s3cret-pw
	stream, err := env.cl.Client.Handshake(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.CloseSend()
	hdr, err := stream.Header()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Recv(); err != nil && !strings.Contains(err.Error(), "EOF") {
		t.Fatal(err)
	}
	for _, md := range []metadata.MD{hdr, stream.Trailer()} {
		if v := md.Get("authorization"); len(v) != 1 || !strings.HasPrefix(v[0], "Bearer ") {
			t.Fatalf("authorization metadata = %v", v)
		}
	}
	var _ flight.FlightServiceClient = env.cl.Client
}

type pki struct {
	pool           *x509.CertPool
	server, client tls.Certificate
}

func newPKI(t *testing.T) pki {
	t.Helper()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, _ := x509.ParseCertificate(caDER)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	leaf := func(serial int64, usage x509.ExtKeyUsage) tls.Certificate {
		key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		tpl := &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "leaf"},
			NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
			IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, ExtKeyUsage: []x509.ExtKeyUsage{usage},
			KeyUsage: x509.KeyUsageDigitalSignature,
		}
		der, err := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	}
	return pki{pool: pool, server: leaf(2, x509.ExtKeyUsageServerAuth), client: leaf(3, x509.ExtKeyUsageClientAuth)}
}

func insecureCreds() credentials.TransportCredentials { return insecure.NewCredentials() }

// slowEngine: reader mengirim batch 1, menunggu deadline ctx, mengirim batch 2,
// lalu melaporkan error ctx — timeout yang benar-benar terjadi di tengah stream.
type slowEngine struct{ engine.Engine }

func (slowEngine) Close() error { return nil }

func (slowEngine) Analyze(context.Context, string) (engine.Analysis, error) {
	return engine.Analysis{Kind: "SELECT", DefaultCatalog: "memory"}, nil
}

func (slowEngine) Query(ctx context.Context, _ string, _ ...any) (array.RecordReader, error) {
	return &slowReader{ctx: ctx}, nil
}

type slowReader struct {
	ctx  context.Context
	n    int
	rec  arrow.RecordBatch
	refs int64
}

var slowSchema = arrow.NewSchema([]arrow.Field{{Name: "i", Type: arrow.PrimitiveTypes.Int64}}, nil)

func (r *slowReader) Schema() *arrow.Schema { return slowSchema }
func (r *slowReader) Retain()               { r.refs++ }
func (r *slowReader) Release() {
	if r.rec != nil {
		r.rec.Release()
		r.rec = nil
	}
}
func (r *slowReader) Record() arrow.RecordBatch      { return r.rec }
func (r *slowReader) RecordBatch() arrow.RecordBatch { return r.rec }
func (r *slowReader) Err() error {
	if r.n > 2 {
		return r.ctx.Err()
	}
	return nil
}

func (r *slowReader) Next() bool {
	r.n++
	if r.n == 2 {
		<-r.ctx.Done()
	}
	if r.n > 2 {
		return false
	}
	if r.rec != nil {
		r.rec.Release()
	}
	b := array.NewInt64Builder(memory.DefaultAllocator)
	defer b.Release()
	b.Append(int64(r.n))
	col := b.NewArray()
	defer col.Release()
	r.rec = array.NewRecordBatch(slowSchema, []arrow.Array{col}, 1)
	return true
}

func TestQueryTimeout_MidStream(t *testing.T) {
	env := start(t, envOpts{queryTimeout: 200 * time.Millisecond, engine: slowEngine{}})
	n, err := drain(env.ctx, env, "SELECT * FROM slow")
	if code := status.Code(err); code != codes.DeadlineExceeded {
		t.Fatalf("code = %v (err %v, rows %d), want DeadlineExceeded — stream must not end silently", code, err, n)
	}
	if n != 2 {
		t.Fatalf("rows before timeout = %d, want 2", n)
	}
	if ends := env.audit.ofType(audit.StatementEnd); len(ends) != 1 || ends[0].Status != codes.DeadlineExceeded.String() || ends[0].Rows != 2 {
		t.Fatalf("statement_end = %+v", ends)
	}
}
