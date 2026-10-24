package jobs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func load(t *testing.T, y string) ([]Job, error) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "jobs.yaml")
	if err := os.WriteFile(p, []byte(y), 0o600); err != nil {
		t.Fatal(err)
	}
	return Load(p)
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name, yaml, err string
	}{
		{"ok", `jobs:
  mart: { schedule: "CRON_TZ=Asia/Jakarta 0 2 * * *", refresh: { table: mart.main.saldo, sql: "SELECT 1;" } }
  slik: { schedule: "@monthly", report: slik_bulanan }`, ""},
		{"bad schedule", `jobs: { a: { schedule: "every day", report: r } }`, "schedule"},
		{"both kinds", `jobs: { a: { schedule: "@daily", report: r, refresh: { table: a.b.c, sql: x } } }`, "exactly one"},
		{"neither kind", `jobs: { a: { schedule: "@daily" } }`, "exactly one"},
		{"two-part table", `jobs: { a: { schedule: "@daily", refresh: { table: main.t, sql: x } } }`, "catalog.schema.table"},
		{"injection in table", `jobs: { a: { schedule: "@daily", refresh: { table: "a.b.c; DROP TABLE x", sql: x } } }`, "catalog.schema.table"},
		{"empty sql", `jobs: { a: { schedule: "@daily", refresh: { table: a.b.c, sql: " " } } }`, "refresh.sql"},
		{"report name", `jobs: { a: { schedule: "@daily", report: "x'); DROP" } }`, "report"},
		{"job name", `jobs: { Bad-Name: { schedule: "@daily", report: r } }`, "name"},
		{"unknown field", `jobs: { a: { schedule: "@daily", report: r, run_as: admin } }`, "run_as"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			js, err := load(t, tt.yaml)
			if tt.err == "" {
				if err != nil || len(js) != 2 {
					t.Fatalf("got %v %v", js, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.err) {
				t.Fatalf("err = %v, want %q", err, tt.err)
			}
		})
	}
}

func TestRunner(t *testing.T) {
	js, err := load(t, `jobs:
  fast: { schedule: "@every 1s", report: r }
  slow: { schedule: "@every 1s", report: s }
  manual: { schedule: "@yearly", refresh: { table: a.b.c, sql: "SELECT 1" } }`)
	if err != nil {
		t.Fatal(err)
	}
	var fast, manual atomic.Int32
	exec := func(_ context.Context, j Job) (int64, error) {
		switch j.Name {
		case "fast":
			fast.Add(1)
			return 7, nil
		case "slow":
			time.Sleep(1500 * time.Millisecond)
			return 0, errors.New("boom")
		default:
			manual.Add(1)
			return 1, nil
		}
	}
	r := NewRunner(js, exec)
	if err := r.Trigger("manual"); err == nil {
		t.Fatal("trigger before Run accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	time.Sleep(100 * time.Millisecond)
	if err := r.Trigger("manual"); err != nil {
		t.Fatal(err)
	}
	if err := r.Trigger("nope"); err == nil {
		t.Fatal("unknown job accepted")
	}
	time.Sleep(3200 * time.Millisecond)
	cancel()
	<-done
	st := map[string]State{}
	for _, s := range r.Snapshot() {
		st[s.Job.Name] = s
	}
	if n := fast.Load(); n < 2 || st["fast"].LastStatus != "OK" || st["fast"].LastRows != 7 {
		t.Errorf("fast: runs=%d state=%+v", n, st["fast"])
	}
	// slow: 1,5 s per run dengan jadwal tiap detik → ada jadwal yang dilewati
	// saat run sebelumnya masih jalan, dan error tercatat.
	if s := st["slow"]; s.Running || (s.LastStatus != "FAILED" && s.LastStatus != "SKIPPED") {
		t.Errorf("slow state = %+v", s)
	}
	if manual.Load() != 1 || st["manual"].LastStatus != "OK" {
		t.Errorf("manual runs=%d state=%+v", manual.Load(), st["manual"])
	}
}
