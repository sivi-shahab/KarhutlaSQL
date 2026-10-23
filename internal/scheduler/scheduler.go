package scheduler

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// Default nama resource group bila role tidak menyebut grup (dari flag
// --max-concurrent/--queue-size/--queue-timeout).
const Default = "default"

var (
	// ErrQueueFull antrean grup penuh.
	ErrQueueFull = errors.New("queue full")
	// ErrQueueTimeout menunggu slot melewati queue_timeout.
	ErrQueueTimeout = errors.New("queue timeout")
)

// Group batas satu resource group (G-08).
type Group struct {
	// MaxConcurrent statement berjalan bersamaan; 0 = tanpa batas.
	MaxConcurrent int `yaml:"max_concurrent"`
	// QueueSize jumlah statement yang boleh menunggu slot; penuh → ErrQueueFull.
	QueueSize int `yaml:"queue_size"`
	// QueueTimeout batas menunggu slot; 0 = tanpa batas.
	QueueTimeout time.Duration `yaml:"queue_timeout"`
	// Timeout batas eksekusi statement grup (di luar waktu antre); 0 = tanpa batas.
	Timeout time.Duration `yaml:"timeout"`
}

// Validate memeriksa nilai batas.
func (g Group) Validate() error {
	if g.MaxConcurrent < 0 || g.QueueSize < 0 || g.QueueTimeout < 0 || g.Timeout < 0 {
		return errors.New("limits must be >= 0")
	}
	return nil
}

// Scheduler semaphore + antrean FIFO berbatas per resource group. Slot adalah
// chan berkapasitas MaxConcurrent; pengirim yang terblokir dilayani runtime Go
// secara FIFO, dan slot yang dilepas langsung berpindah ke penunggu terdepan.
type Scheduler struct {
	mu     sync.Mutex
	groups map[string]*sem
}

type sem struct {
	cfg     Group
	slots   chan struct{} // nil = tanpa batas
	waiting atomic.Int64
	running atomic.Int64
}

// New membuat Scheduler kosong; grup dibuat saat pertama dipakai.
func New() *Scheduler { return &Scheduler{groups: map[string]*sem{}} }

// group mengembalikan semaphore grup; konfigurasi berubah (reload policy) →
// semaphore baru, pemegang slot lama melepas ke semaphore lamanya.
func (s *Scheduler) group(name string, g Group) *sem {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sm, ok := s.groups[name]; ok && sm.cfg == g {
		return sm
	}
	sm := &sem{cfg: g}
	if g.MaxConcurrent > 0 {
		sm.slots = make(chan struct{}, g.MaxConcurrent)
	}
	s.groups[name] = sm
	return sm
}

// Acquire menunggu slot grup name. Mengembalikan release (wajib dipanggil
// tepat sekali) dan lama menunggu. Error: ErrQueueFull, ErrQueueTimeout, atau
// sebab pembatalan ctx (kill session / klien putus).
func (s *Scheduler) Acquire(ctx context.Context, name string, g Group) (func(), time.Duration, error) {
	sm := s.group(name, g)
	release := func() {
		sm.running.Add(-1)
		if sm.slots != nil {
			<-sm.slots
		}
	}
	if sm.slots == nil {
		sm.running.Add(1)
		return release, 0, nil
	}
	select {
	case sm.slots <- struct{}{}:
		sm.running.Add(1)
		return release, 0, nil
	default:
	}
	if sm.waiting.Add(1) > int64(g.QueueSize) {
		sm.waiting.Add(-1)
		return nil, 0, fmt.Errorf("%w: resource group %s (%d waiting)", ErrQueueFull, name, g.QueueSize)
	}
	defer sm.waiting.Add(-1)
	t0 := time.Now()
	var expired <-chan time.Time
	if g.QueueTimeout > 0 {
		t := time.NewTimer(g.QueueTimeout)
		defer t.Stop()
		expired = t.C
	}
	select {
	case sm.slots <- struct{}{}:
		sm.running.Add(1)
		return release, time.Since(t0), nil
	case <-expired:
		return nil, time.Since(t0), fmt.Errorf("%w: resource group %s after %s", ErrQueueTimeout, name, g.QueueTimeout)
	case <-ctx.Done():
		return nil, time.Since(t0), context.Cause(ctx)
	}
}

// Stat keadaan satu grup.
type Stat struct {
	Group            string
	Running, Waiting int
}

// Stats keadaan semua grup, urut nama (sumber gauge metrics).
func (s *Scheduler) Stats() []Stat {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Stat, 0, len(s.groups))
	for name, sm := range s.groups {
		out = append(out, Stat{Group: name, Running: int(sm.running.Load()), Waiting: int(sm.waiting.Load())})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Group < out[j].Group })
	return out
}
