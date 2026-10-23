//go:build duckdb_arrow

package sqlext

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/bankmega/karhutlasql/internal/audit"
	"github.com/bankmega/karhutlasql/internal/auth"
	"github.com/bankmega/karhutlasql/internal/engine/duckdb"
	"github.com/bankmega/karhutlasql/internal/session"
)

type nopSink struct{ events []audit.Event }

func (s *nopSink) Emit(e audit.Event) error { s.events = append(s.events, e); return nil }

func rows(t *testing.T, eng *duckdb.Engine, ctx context.Context, sql string) ([][]string, error) {
	t.Helper()
	rdr, err := eng.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rdr.Release()
	var out [][]string
	for rdr.Next() {
		rec := rdr.RecordBatch()
		for r := range int(rec.NumRows()) {
			var row []string
			for _, c := range rec.Columns() {
				row = append(row, strings.Clone(c.ValueStr(r))) // buffer milik DuckDB bebas saat Release
			}
			out = append(out, row)
		}
	}
	return out, rdr.Err()
}

func TestFunctions(t *testing.T) {
	ctx := context.Background()
	reg := session.NewRegistry(time.Hour)
	sink := &nopSink{}
	pr := prometheus.NewRegistry()
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "c_total"}, []string{"k"})
	h := prometheus.NewHistogram(prometheus.HistogramOpts{Name: "h", Buckets: []float64{1}})
	pr.MustRegister(c, h)
	c.WithLabelValues("v").Add(3)
	h.Observe(0.5)

	eng, err := duckdb.Open(ctx, duckdb.Config{Setup: func(ctx context.Context, conn *sql.Conn) error {
		return Register(ctx, conn, Deps{Sessions: reg, Gatherer: pr, Audit: audit.New(sink, false, nil)})
	}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()

	exp := time.Now().Add(time.Hour)
	admin := auth.Identity{Subject: "admin", Roles: []string{auth.RoleAdmin}, TokenID: "S-admin", ExpiresAt: exp}
	user := auth.Identity{Subject: "ana", TokenID: "S-ana", ExpiresAt: exp}
	sa, _ := reg.Attach(admin, "p1")
	su, _ := reg.Attach(user, "p2")
	actx, adone := sa.Begin(auth.WithIdentity(ctx, admin), "SELECT secret")
	defer adone()
	uctx, udone := su.Begin(auth.WithIdentity(ctx, user), "SELECT * FROM karhutla_sessions()")
	defer udone()

	// admin melihat semua, user hanya sesinya; SQL di-hash secara default.
	got, err := rows(t, eng, actx, "SELECT session_id, statement, current FROM karhutla_sessions() ORDER BY session_id")
	if err != nil || len(got) != 2 || got[0][0] != "S-admin" || got[0][2] != "true" || !strings.HasPrefix(got[0][1], "sha256:") {
		t.Fatalf("admin sessions = %v err %v", got, err)
	}
	if got, err := rows(t, eng, uctx, "SELECT session_id FROM karhutla_sessions()"); err != nil || len(got) != 1 || got[0][0] != "S-ana" {
		t.Fatalf("user sessions = %v err %v", got, err)
	}
	if _, err := rows(t, eng, ctx, "SELECT * FROM karhutla_sessions()"); err == nil || !strings.Contains(err.Error(), ErrPermissionDenied.Error()) {
		t.Fatalf("no identity err = %v", err)
	}

	// Describe (prepare) tidak boleh mengeksekusi kill.
	if _, err := eng.Describe(actx, "SELECT * FROM karhutla_kill_session('S-ana')"); err != nil {
		t.Fatal(err)
	}
	if uctx.Err() != nil {
		t.Fatal("describe killed the session")
	}
	if _, err := rows(t, eng, uctx, "CALL karhutla_kill_session('S-admin')"); err == nil || !strings.Contains(err.Error(), ErrPermissionDenied.Error()) {
		t.Fatalf("non-admin kill err = %v", err)
	}
	if got, err := rows(t, eng, actx, "CALL karhutla_kill_session('S-ana')"); err != nil || len(got) != 1 || got[0][1] != "true" {
		t.Fatalf("kill = %v err %v", got, err)
	}
	if !errors.Is(context.Cause(uctx), session.ErrKilled) || !reg.Revoked("S-ana") {
		t.Fatal("kill did not cancel/revoke")
	}
	if len(sink.events) != 1 || sink.events[0].Type != audit.SessionKilled || sink.events[0].Target != "S-ana" || sink.events[0].SessionID != "S-admin" {
		t.Fatalf("audit = %+v", sink.events)
	}

	got, err = rows(t, eng, actx, "SELECT name, labels, value FROM karhutla_metrics() ORDER BY name, labels")
	want := [][]string{
		{"c_total", `{"k":"v"}`, "3"},
		{"h_bucket", `{"le":"1"}`, "1"},
		{"h_count", `{}`, "1"},
		{"h_sum", `{}`, "0.5"},
	}
	if err != nil || len(got) != len(want) {
		t.Fatalf("metrics = %v err %v", got, err)
	}
	for i := range want {
		if strings.Join(got[i], "|") != strings.Join(want[i], "|") {
			t.Errorf("metrics[%d] = %v, want %v", i, got[i], want[i])
		}
	}
}
