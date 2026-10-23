package session

import (
	"sync"
	"time"
)

// Query satu statement selesai (G-09). SQL sudah disamarkan pemanggil
// (--audit-log-sql).
type Query struct {
	RequestID, SessionID, User, Kind, Group string
	SQL                                     string
	Status, Error                           string
	StartedAt                               time.Time
	QueueWait, Duration                     time.Duration
	Rows, Bytes, PeakMemory                 int64
	PlanHash                                string
}

// History ring buffer statement terakhir di memori.
// ponytail: hilang saat restart & hanya n terakhir; persistensi ikut Parquet
// access-history (Fase 6b) bila riwayat panjang dibutuhkan.
type History struct {
	mu   sync.Mutex
	buf  []Query
	next int
	full bool
}

// NewHistory menyimpan n statement terakhir (n ≥ 1).
func NewHistory(n int) *History { return &History{buf: make([]Query, max(n, 1))} }

// Add mencatat q, menimpa yang tertua bila penuh. Aman pada nil.
func (h *History) Add(q Query) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.buf[h.next] = q
	h.next = (h.next + 1) % len(h.buf)
	h.full = h.full || h.next == 0
}

// Snapshot salinan isi, tertua dulu.
func (h *History) Snapshot() []Query {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.full {
		return append([]Query(nil), h.buf[:h.next]...)
	}
	return append(append([]Query(nil), h.buf[h.next:]...), h.buf[:h.next]...)
}
