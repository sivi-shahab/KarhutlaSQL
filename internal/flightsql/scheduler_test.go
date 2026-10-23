//go:build duckdb_arrow

package flightsql

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bankmega/karhutlasql/internal/scheduler"
)

// G-08/G-09: slot penuh → antre FIFO; antrean penuh → RESOURCE_EXHAUSTED;
// query kontrol (karhutla_*) tidak antre; riwayat mencatat waktu antre & plan.
func TestScheduler_QueueAndHistory(t *testing.T) {
	env := start(t, envOpts{group: scheduler.Group{MaxConcurrent: 1, QueueSize: 1, QueueTimeout: time.Minute}})
	ctxA, cancelA := context.WithCancel(env.ctx)
	defer cancelA()
	errA := make(chan error, 1)
	go func() {
		_, err := drain(ctxA, env, "SELECT count(*) FROM range(1000000000000)")
		errA <- err
	}()
	metric := func(name string) int {
		v, err := firstCol(env.ctx, env, "SELECT value::BIGINT::VARCHAR FROM karhutla_metrics() WHERE name = '"+name+"' AND labels LIKE '%\"default\"%'")
		if err != nil {
			t.Fatal(err) // query kontrol: tidak boleh ikut antre
		}
		if len(v) == 0 {
			return 0
		}
		n, _ := strconv.Atoi(v[0])
		return n
	}
	waitUntil(t, func() bool { return metric("karhutla_queue_running") == 1 })

	got := make(chan []string, 1)
	go func() {
		v, err := firstCol(env.ctx, env, "SELECT 'queued'")
		if err != nil {
			t.Error(err)
		}
		got <- v
	}()
	waitUntil(t, func() bool { return metric("karhutla_queue_depth") == 1 })
	if _, err := drain(env.ctx, env, "SELECT 43"); status.Code(err) != codes.ResourceExhausted || !strings.Contains(err.Error(), "queue full") {
		t.Fatalf("third statement: %v", err)
	}

	time.Sleep(50 * time.Millisecond) // waktu antre terukur
	cancelA()
	if err := <-errA; status.Code(err) != codes.Canceled {
		t.Fatalf("A: %v", err)
	}
	if v := <-got; len(v) != 1 || v[0] != "queued" {
		t.Fatalf("queued statement = %v", v)
	}

	rows, err := firstCol(env.ctx, env, `SELECT concat_ws('|', status, resource_group, queue_ms > 0, plan_hash IS NOT NULL)
		FROM karhutla_query_history() WHERE kind = 'query' AND statement NOT LIKE '%karhutla_%' ORDER BY started_at`)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"OK|default|true|true": false, "ResourceExhausted|default|false|false": false}
	for _, r := range rows {
		if _, ok := want[r]; ok {
			want[r] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("history missing %q: %v", k, rows)
		}
	}
}

func waitUntil(t *testing.T, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatal("condition not reached")
}
