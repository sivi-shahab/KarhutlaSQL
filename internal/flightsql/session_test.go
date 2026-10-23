//go:build duckdb_arrow

package flightsql

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow/array"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bankmega/karhutlasql/internal/audit"
)

// firstCol menjalankan sql dan mengembalikan kolom pertama sebagai string.
func firstCol(ctx context.Context, env *testEnv, sql string) ([]string, error) {
	info, err := env.cl.Execute(ctx, sql)
	if err != nil {
		return nil, err
	}
	rdr, err := env.cl.DoGet(ctx, info.Endpoint[0].Ticket)
	if err != nil {
		return nil, err
	}
	defer rdr.Release()
	var out []string
	for rdr.Next() {
		col := rdr.RecordBatch().Column(0)
		for i := range col.Len() {
			out = append(out, strings.Clone(col.(*array.String).Value(i)))
		}
	}
	return out, rdr.Err()
}

func TestKillSession(t *testing.T) {
	env := start(t, envOpts{})
	ctxA := env.ctx
	ctxB, err := env.cl.Client.AuthenticateBasicToken(context.Background(), testUser, testPass)
	if err != nil {
		t.Fatal(err)
	}

	errA := make(chan error, 1)
	go func() {
		_, err := drain(ctxA, env, "SELECT count(*) FROM range(1000000000000)")
		errA <- err
	}()

	var target string
	deadline := time.Now().Add(10 * time.Second)
	for target == "" && time.Now().Before(deadline) {
		ids, err := firstCol(ctxB, env, "SELECT session_id FROM karhutla_sessions() WHERE NOT current AND active_statements > 0")
		if err != nil {
			t.Fatal(err)
		}
		if len(ids) == 1 {
			target = ids[0]
		}
		time.Sleep(50 * time.Millisecond)
	}
	if target == "" {
		t.Fatal("long query of session A not visible in karhutla_sessions()")
	}

	t0 := time.Now()
	if ids, err := firstCol(ctxB, env, "CALL karhutla_kill_session('"+target+"')"); err != nil || len(ids) != 1 {
		t.Fatalf("kill = %v err %v", ids, err)
	}
	select {
	case err := <-errA:
		if code := status.Code(err); code != codes.Canceled || !strings.Contains(err.Error(), "killed") {
			t.Fatalf("session A err = %v, want Canceled (killed)", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("session A query not cancelled")
	}
	if d := time.Since(t0); d > 5*time.Second {
		t.Errorf("cancel took %v", d)
	}
	if _, err := drain(ctxA, env, "SELECT 1"); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("token A after kill: %v, want Unauthenticated", err)
	}
	if _, err := drain(ctxB, env, "SELECT 1"); err != nil {
		t.Fatalf("session B unusable: %v", err)
	}

	if ev := env.audit.ofType(audit.SessionKilled); len(ev) != 1 || ev[0].Target != target || ev[0].Via != "sql" {
		t.Errorf("session_killed = %+v", ev)
	}
	got, err := firstCol(ctxB, env, `SELECT CAST(value AS VARCHAR) FROM karhutla_metrics()
		WHERE name = 'karhutla_queries_total' AND json_extract_string(labels, '$.status') = 'Canceled'`)
	if err != nil || len(got) != 1 || got[0] != "1.0" {
		t.Errorf("karhutla_queries_total{status=Canceled} = %v err %v", got, err)
	}
}

func TestStatementHandle_PerSession(t *testing.T) {
	env := start(t, envOpts{})
	other, err := env.cl.Client.AuthenticateBasicToken(context.Background(), testUser, testPass)
	if err != nil {
		t.Fatal(err)
	}
	info, err := env.cl.Execute(env.ctx, "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	rdr, err := env.cl.DoGet(other, info.Endpoint[0].Ticket)
	if err == nil {
		for rdr.Next() {
		}
		err = rdr.Err()
		rdr.Release()
	}
	if status.Code(err) != codes.NotFound {
		t.Fatalf("ticket of another session: %v, want NotFound", err)
	}
	if n, err := drain(env.ctx, env, "SELECT 1"); err != nil || n != 1 {
		t.Fatalf("owner: n=%d err=%v", n, err)
	}
}
