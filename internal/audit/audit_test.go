package audit

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLogger_SQLHashOrText(t *testing.T) {
	tests := []struct {
		name    string
		withSQL bool
	}{{"hash only", false}, {"with text", true}}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			l := New(NewWriterSink(&buf), tt.withSQL, nil)
			e := Event{Type: StatementStart, User: "admin"}
			l.SQL(&e, "SELECT nik FROM nasabah")
			l.Emit(WithRequestID(context.Background(), "req-1"), e)

			var got Event
			if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.RequestID != "req-1" || got.Time.IsZero() || len(got.SQLHash) != 64 {
				t.Fatalf("event %+v", got)
			}
			if hasText := strings.Contains(buf.String(), "nasabah"); hasText != tt.withSQL {
				t.Fatalf("sql text present=%v, want %v: %s", hasText, tt.withSQL, buf.String())
			}
		})
	}
}

func TestFileSink_DailyRotationAndMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "audit")
	s, err := NewFileSink(dir)
	if err != nil {
		t.Fatal(err)
	}
	d1 := time.Date(2026, 10, 22, 23, 59, 0, 0, time.UTC)
	for _, ts := range []time.Time{d1, d1, d1.Add(2 * time.Minute)} {
		if err := s.Emit(Event{Time: ts, Type: LoginOK}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	for file, wantLines := range map[string]int{"audit-2026-10-22.jsonl": 2, "audit-2026-10-23.jsonl": 1} {
		p := filepath.Join(dir, file)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if n := strings.Count(string(b), "\n"); n != wantLines {
			t.Errorf("%s lines = %d, want %d", file, n, wantLines)
		}
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v", file, fi.Mode().Perm())
		}
	}
	// append-only: membuka ulang tidak menimpa.
	s2, _ := NewFileSink(dir)
	_ = s2.Emit(Event{Time: d1, Type: LoginFail})
	_ = s2.Close()
	b, _ := os.ReadFile(filepath.Join(dir, "audit-2026-10-22.jsonl"))
	if n := strings.Count(string(b), "\n"); n != 3 {
		t.Errorf("after reopen lines = %d, want 3", n)
	}
}
