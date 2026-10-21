//go:build duckdb_arrow

// Command testdata membuat tests/data/tpch_sf0_01.duckdb memakai engine yang
// sama dengan server (versi storage DuckDB dijamin cocok).
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/bankmega/karhutlasql/internal/engine/duckdb"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: testdata <out.duckdb>")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, "testdata:", err)
		os.Exit(1)
	}
}

func run(out string) error {
	if err := os.Remove(out); err != nil && !os.IsNotExist(err) {
		return err
	}
	ctx := context.Background()
	e, err := duckdb.Open(ctx, duckdb.Config{Path: out, InitSQL: "INSTALL tpch; LOAD tpch; CALL dbgen(sf = 0.01);"})
	if err != nil {
		return err
	}
	rdr, err := e.Query(ctx, "SELECT version()")
	if err != nil {
		return err
	}
	for rdr.Next() {
		fmt.Println("duckdb", rdr.RecordBatch().Column(0).ValueStr(0), "→", out)
	}
	rdr.Release()
	return e.Close()
}
