//go:build duckdb_arrow

package main

import (
	"bytes"
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRun(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{"version", []string{"--version"}, 0, "karhutlasql-server version dev", ""},
		{"help", []string{"--help"}, 0, "--database-filename", ""},
		{"unknown flag", []string{"--nope"}, 1, "", "unknown flag"},
		{"invalid config", []string{"--port", "0"}, 1, "", "out of range"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			if got := run(context.Background(), tt.args, &out, &errOut); got != tt.wantCode {
				t.Fatalf("exit code = %d, want %d (stderr: %s)", got, tt.wantCode, errOut.String())
			}
			if !strings.Contains(out.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want contains %q", out.String(), tt.wantStdout)
			}
			if !strings.Contains(errOut.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want contains %q", errOut.String(), tt.wantStderr)
			}
		})
	}
}

// TestRun_ServeAndShutdown: server listen, lalu cancel ctx (= SIGTERM) → exit 0.
func TestRun_ServeAndShutdown(t *testing.T) {
	lis, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := lis.Addr().(*net.TCPAddr).Port
	_ = lis.Close()

	ctx, cancel := context.WithCancel(context.Background())
	var errOut bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{"--host", "127.0.0.1", "--port", strconv.Itoa(port)}, &bytes.Buffer{}, &errOut)
	}()

	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never listened: %s", errOut.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit code %d: %s", code, errOut.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown hung")
	}
}
