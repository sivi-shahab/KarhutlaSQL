package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestAcquire_FIFOAndLimits(t *testing.T) {
	s := New()
	g := Group{MaxConcurrent: 2, QueueSize: 3}
	ctx := context.Background()
	var held []func()
	for range 2 {
		rel, wait, err := s.Acquire(ctx, "x", g)
		if err != nil || wait != 0 {
			t.Fatalf("slot: %v %v", wait, err)
		}
		held = append(held, rel)
	}
	// 3 penunggu masuk berurutan; yang ke-4 ditolak.
	order := make(chan int, 3)
	var wg sync.WaitGroup
	for i := range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rel, _, err := s.Acquire(ctx, "x", g)
			if err != nil {
				t.Error(err)
				return
			}
			order <- i
			rel()
		}()
		waitFor(t, func() bool { return s.Stats()[0].Waiting == i+1 })
	}
	if _, _, err := s.Acquire(ctx, "x", g); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("4th waiter: %v", err)
	}
	if st := s.Stats()[0]; st.Running != 2 || st.Waiting != 3 {
		t.Fatalf("stats %+v", st)
	}
	held[0]() // satu slot: penunggu dilayani berurutan
	wg.Wait()
	close(order)
	want := 0
	for got := range order {
		if got != want {
			t.Fatalf("FIFO: got %d want %d", got, want)
		}
		want++
	}
	held[1]()
	if st := s.Stats()[0]; st.Running != 0 || st.Waiting != 0 {
		t.Fatalf("after release %+v", st)
	}
}

func TestAcquire_TimeoutCancelUnlimited(t *testing.T) {
	s := New()
	g := Group{MaxConcurrent: 1, QueueSize: 5, QueueTimeout: 20 * time.Millisecond}
	rel, _, _ := s.Acquire(context.Background(), "x", g)
	defer rel()
	if _, wait, err := s.Acquire(context.Background(), "x", g); !errors.Is(err, ErrQueueTimeout) || wait < g.QueueTimeout {
		t.Fatalf("timeout: %v %v", wait, err)
	}
	ctx, cancel := context.WithCancelCause(context.Background()) // tanpa queue timeout ctx yang menang
	g.QueueTimeout = time.Minute
	rel2, _, err := s.Acquire(context.Background(), "x", g) // konfigurasi baru → semaphore baru
	if err != nil {
		t.Fatal(err)
	}
	defer rel2()
	boom := errors.New("killed")
	time.AfterFunc(10*time.Millisecond, func() { cancel(boom) })
	if _, _, err := s.Acquire(ctx, "x", g); !errors.Is(err, boom) {
		t.Fatalf("cancel: %v", err)
	}
	for range 100 { // tanpa batas
		if _, _, err := s.Acquire(context.Background(), "free", Group{}); err != nil {
			t.Fatal(err)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for range 500 {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not reached")
}
