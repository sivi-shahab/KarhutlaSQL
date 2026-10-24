//go:build duckdb_arrow

package duckdb

import (
	"context"
	"math/rand/v2"
	"sync"
	"testing"
	"time"
)

// TestStreaming_Stress: berhenti awal, batal, dan selesai normal secara
// acak & bersamaan — tidak boleh crash/bocor (race handle C vs pembatalan).
func TestStreaming_Stress(t *testing.T) {
	e, err := Open(context.Background(), Config{StreamingBufferSize: "1MB", Unlocked: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close() }()
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			r := rand.New(rand.NewPCG(uint64(w), 1))
			for range 40 {
				ctx, cancel := context.WithTimeout(context.Background(), time.Duration(r.IntN(30))*time.Millisecond)
				rdr, err := e.Query(ctx, "SELECT range, range::VARCHAR FROM range(3000000)")
				if err == nil {
					for i := 0; rdr.Next() && i < r.IntN(400); i++ {
					}
					if r.IntN(2) == 0 {
						cancel()
					}
					rdr.Release()
				}
				cancel()
			}
		})
	}
	wg.Wait()
}
