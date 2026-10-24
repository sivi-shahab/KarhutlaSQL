package cache

import (
	"errors"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/memory"
)

var schema = arrow.NewSchema([]arrow.Field{{Name: "i", Type: arrow.PrimitiveTypes.Int64}}, nil)

// reader n batch berisi angka 0..rows-1; err dilaporkan setelah batch terakhir.
func reader(t *testing.T, batches, rows int, err error) array.RecordReader {
	t.Helper()
	var recs []arrow.RecordBatch
	for range batches {
		b := array.NewInt64Builder(memory.DefaultAllocator)
		for i := range rows {
			b.Append(int64(i))
		}
		col := b.NewArray()
		recs = append(recs, array.NewRecordBatch(schema, []arrow.Array{col}, int64(rows)))
		col.Release()
		b.Release()
	}
	r, e := array.NewRecordReader(schema, recs)
	if e != nil {
		t.Fatal(e)
	}
	if err != nil {
		return errReader{r, err}
	}
	return r
}

type errReader struct {
	array.RecordReader
	err error
}

func (e errReader) Err() error { return e.err }

func drain(r array.RecordReader) (rows int64) {
	defer r.Release()
	for r.Next() {
		rows += r.RecordBatch().NumRows()
	}
	return rows
}

func TestCache(t *testing.T) {
	now := time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC)
	c := New(time.Minute, 1<<20, 64<<10)
	c.now = func() time.Time { return now }
	k := Key("oidc", "ana", "SELECT 1")
	if Key("oidc", "ana", "SELECT 1") != k || Key("oidca", "na", "SELECT 1") == k {
		t.Fatal("key not stable / ambiguous")
	}
	if _, ok := c.Get(k); ok {
		t.Fatal("hit on empty cache")
	}
	if n := drain(c.Writer(k, reader(t, 3, 100, nil))); n != 300 {
		t.Fatalf("tee rows %d", n)
	}
	r, ok := c.Get(k)
	if !ok || drain(r) != 300 {
		t.Fatal("cached result missing or wrong")
	}
	// Error di akhir stream, entri terlalu besar, berhenti di tengah: tidak disimpan.
	drain(c.Writer("err", reader(t, 1, 10, errors.New("boom"))))
	drain(c.Writer("big", reader(t, 20, 1000, nil))) // 160 KB > 64 KB
	partial := c.Writer("partial", reader(t, 3, 10, nil))
	partial.Next()
	partial.Release()
	for _, key := range []string{"err", "big", "partial"} {
		if _, ok := c.Get(key); ok {
			t.Errorf("%s stored", key)
		}
	}
	// TTL.
	now = now.Add(2 * time.Minute)
	if _, ok := c.Get(k); ok {
		t.Error("expired entry served")
	}
	// LRU: batas total → entri terlama dibuang.
	c = New(time.Minute, 20<<10, 20<<10)
	for _, key := range []string{"a", "b", "c"} {
		drain(c.Writer(key, reader(t, 1, 1000, nil))) // ±8 KB
	}
	if _, ok := c.Get("a"); ok {
		t.Error("oldest entry not evicted")
	}
	if _, ok := c.Get("c"); !ok {
		t.Error("newest entry evicted")
	}
}
