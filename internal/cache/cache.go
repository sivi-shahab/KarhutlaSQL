// Package cache result cache query sebagai Arrow IPC di memori (LRU, TTL,
// batas byte total & per entri). Kunci dibentuk pemanggil (identitas, SQL
// yang dieksekusi setelah governance, parameter, versi data); paket ini tidak
// tahu soal auth.
package cache

import (
	"bytes"
	"container/list"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sync"
	"time"

	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"github.com/prometheus/client_golang/prometheus"
)

// Cache LRU hasil query.
type Cache struct {
	ttl                time.Duration
	maxBytes, maxEntry int64
	now                func() time.Time

	mu    sync.Mutex
	ll    *list.List // depan = terbaru dipakai
	m     map[string]*list.Element
	size  int64
	stats map[string]float64 // hit, miss, store, evict
}

type entry struct {
	key  string
	data []byte
	exp  time.Time
}

// New Cache dengan TTL per entri, batas total, dan batas per entri (byte IPC).
func New(ttl time.Duration, maxBytes, maxEntry int64) *Cache {
	return &Cache{ttl: ttl, maxBytes: maxBytes, maxEntry: min(maxEntry, maxBytes), now: time.Now,
		ll: list.New(), m: map[string]*list.Element{}, stats: map[string]float64{}}
}

// Key sha256 hex dari bagian-bagian kunci (dengan prefix panjang: tidak ambigu).
func Key(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_ = binary.Write(h, binary.BigEndian, uint64(len(p))) // hash.Hash tidak pernah error
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Get reader atas hasil ter-cache; false bila tidak ada / kedaluwarsa.
func (c *Cache) Get(key string) (array.RecordReader, bool) {
	c.mu.Lock()
	el, ok := c.m[key]
	if ok && c.now().After(el.Value.(*entry).exp) { //nolint:forcetypeassert // list hanya berisi *entry
		c.remove(el)
		ok = false
	}
	if !ok {
		c.stats["miss"]++
		c.mu.Unlock()
		return nil, false
	}
	c.ll.MoveToFront(el)
	c.stats["hit"]++
	data := el.Value.(*entry).data //nolint:forcetypeassert // list hanya berisi *entry
	c.mu.Unlock()
	rdr, err := ipc.NewReader(bytes.NewReader(data), ipc.WithAllocator(memory.DefaultAllocator))
	if err != nil { // data ditulis sendiri oleh Writer: tidak terduga
		return nil, false
	}
	return rdr, true
}

// Writer membungkus rdr: setiap batch juga ditulis ke buffer IPC dan
// disimpan di bawah key bila rdr habis tanpa error dan ukurannya ≤ batas
// entri. Berhenti di tengah (klien putus, batas byte role) → tidak disimpan.
func (c *Cache) Writer(key string, rdr array.RecordReader) array.RecordReader {
	return &tee{RecordReader: rdr, c: c, key: key}
}

func (c *Cache) put(key string, data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[key]; ok {
		c.remove(el)
	}
	e := &entry{key: key, data: data, exp: c.now().Add(c.ttl)}
	c.m[key] = c.ll.PushFront(e)
	c.size += int64(len(data))
	c.stats["store"]++
	for c.size > c.maxBytes {
		c.remove(c.ll.Back())
		c.stats["evict"]++
	}
}

func (c *Cache) remove(el *list.Element) {
	e := c.ll.Remove(el).(*entry) //nolint:forcetypeassert // list hanya berisi *entry
	delete(c.m, e.key)
	c.size -= int64(len(e.data))
}

type tee struct {
	array.RecordReader
	c    *Cache
	key  string
	buf  bytes.Buffer
	w    *ipc.Writer
	skip bool // melebihi batas entri / gagal tulis
}

func (t *tee) Next() bool {
	if !t.RecordReader.Next() {
		if !t.skip && t.Err() == nil {
			if t.w == nil {
				t.w = ipc.NewWriter(&t.buf, ipc.WithSchema(t.Schema()))
			}
			if t.w.Close() == nil {
				t.c.put(t.key, bytes.Clone(t.buf.Bytes()))
			}
		}
		t.skip = true // sekali saja
		return false
	}
	if t.skip {
		return true
	}
	if t.w == nil {
		t.w = ipc.NewWriter(&t.buf, ipc.WithSchema(t.Schema()))
	}
	if t.w.Write(t.RecordBatch()) != nil || int64(t.buf.Len()) > t.c.maxEntry {
		t.skip, t.buf = true, bytes.Buffer{}
	}
	return true
}

var (
	opsDesc   = prometheus.NewDesc("karhutla_result_cache_operations_total", "Operasi result cache (hit, miss, store, evict).", []string{"op"}, nil)
	bytesDesc = prometheus.NewDesc("karhutla_result_cache_bytes", "Byte IPC di result cache.", nil, nil)
)

// Describe prometheus.Collector.
func (c *Cache) Describe(ch chan<- *prometheus.Desc) { ch <- opsDesc; ch <- bytesDesc }

// Collect prometheus.Collector.
func (c *Cache) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, op := range []string{"hit", "miss", "store", "evict"} {
		ch <- prometheus.MustNewConstMetric(opsDesc, prometheus.CounterValue, c.stats[op], op)
	}
	ch <- prometheus.MustNewConstMetric(bytesDesc, prometheus.GaugeValue, float64(c.size))
}
