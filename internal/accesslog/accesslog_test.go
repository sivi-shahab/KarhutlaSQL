package accesslog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bankmega/karhutlasql/internal/objstore"
)

type failStore struct{ objstore.Dir }

func (failStore) Put(context.Context, string, []byte) error { return errors.New("s3 down") }

func TestLog_FlushQueryChain(t *testing.T) {
	ctx := context.Background()
	store := objstore.Dir(t.TempDir())
	day1 := time.Date(2026, 10, 23, 23, 59, 0, 0, time.UTC)
	day2 := day1.Add(2 * time.Minute)
	l := New(Options{Store: store, Now: func() time.Time { return day2 }})
	l.Append(Record{Time: day1, User: "ana", Table: "w.main.nasabah", Column: "nik", Tag: "pii.nik", Policy: "partial", Rows: 3, SQLHash: "h"},
		Record{Time: day2, User: "budi", Table: "w.main.nasabah", RowFilter: "(branch_code = '0123')", SQLHash: "h2"})
	if err := l.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	l.Append(Record{Time: day2.Add(time.Second), User: "ana", Table: "w.main.t", SQLHash: "h3"}) // masih di buffer
	got, err := l.Query(ctx, day1.Add(-time.Hour), day2.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Column != "nik" || got[0].Policy != "partial" || got[0].Rows != 3 || !got[0].Time.Equal(day1) || got[1].Column != "" {
		t.Fatalf("query = %+v", got)
	}
	if got, _ := l.Query(ctx, day2, day2.Add(time.Hour)); len(got) != 2 {
		t.Fatalf("range filter: %d", len(got))
	}
	// Dua partisi hari, manifest berantai.
	if err := l.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	keys, _ := store.List(ctx, Prefix)
	var prev string
	var manifests int
	for _, k := range keys {
		if !strings.HasSuffix(k, ".manifest.json") {
			continue
		}
		manifests++
		b, _ := store.Get(ctx, k)
		var m Manifest
		_ = json.Unmarshal(b, &m)
		obj, _ := store.Get(ctx, m.Object)
		sum := sha256.Sum256(obj)
		if m.SHA256 != hex.EncodeToString(sum[:]) || m.Prev != prev {
			t.Errorf("manifest %s: %+v (prev %s)", k, m, prev)
		}
		ms := sha256.Sum256(b)
		prev = hex.EncodeToString(ms[:])
	}
	if manifests != 3 || !strings.Contains(strings.Join(keys, " "), "dt=2026-10-23/") || !strings.Contains(strings.Join(keys, " "), "dt=2026-10-24/") {
		t.Errorf("keys %v", keys)
	}
}

func TestLog_FailClosed(t *testing.T) {
	ctx := context.Background()
	l := New(Options{Store: failStore{objstore.Dir(t.TempDir())}, MaxBuffered: 2})
	l.Append(Record{Time: time.Now(), SQLHash: "a"})
	if err := l.Flush(ctx); err == nil || l.Healthy() != nil {
		t.Fatalf("flush err %v healthy %v", err, l.Healthy())
	}
	l.Append(Record{Time: time.Now(), SQLHash: "b"})
	if err := l.Healthy(); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("healthy = %v", err)
	}
	if got, _ := l.Query(ctx, time.Now().Add(-time.Hour), time.Now().Add(time.Hour)); len(got) != 2 {
		t.Fatalf("records kept in buffer: %d", len(got))
	}
}
