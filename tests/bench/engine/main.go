//go:build duckdb_arrow

// Command engine mengukur query langsung lewat internal/engine (tanpa server,
// auth, atau gRPC) untuk memisahkan overhead engine dari overhead server.
//
//	go run -tags duckdb_arrow ./tests/bench/engine -db tpch_sf10.duckdb -runs 5 < queries.txt
//
// stdin: satu query per baris (titik koma di akhir opsional).
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/bankmega/karhutlasql/internal/engine/duckdb"
)

func main() {
	db := flag.String("db", "", "file .duckdb")
	runs := flag.Int("runs", 5, "run terukur per query (setelah 1 pemanasan)")
	initSQL := flag.String("init", "", "SQL setelah Open (mis. SET ...)")
	flag.Parse()
	ctx := context.Background()
	e, err := duckdb.Open(ctx, duckdb.Config{Path: *db, InitSQL: *initSQL, Unlocked: true})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer func() { _ = e.Close() }()
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for n := 1; sc.Scan(); n++ {
		q := strings.TrimSuffix(strings.TrimSpace(sc.Text()), ";")
		var ts []time.Duration
		for i := 0; i <= *runs; i++ {
			t0 := time.Now()
			rdr, err := e.Query(ctx, q)
			if err != nil {
				fmt.Fprintln(os.Stderr, n, err)
				os.Exit(1)
			}
			for rdr.Next() {
			}
			rdr.Release()
			if i > 0 {
				ts = append(ts, time.Since(t0))
			}
		}
		slices.Sort(ts)
		fmt.Printf("%d\t%.3f\n", n, ts[len(ts)/2].Seconds())
	}
}
