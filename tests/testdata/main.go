//go:build duckdb_arrow

// Command testdata membuat database TPC-H (default sf 0.01 untuk test,
// sf 10 untuk benchmark) memakai engine yang sama dengan server (versi
// storage DuckDB dijamin cocok).
package main

import (
	"context"
	"fmt"
	"os"
	"strconv"

	"github.com/bankmega/karhutlasql/internal/engine/duckdb"
)

func main() {
	if len(os.Args) < 2 || len(os.Args) > 3 {
		fmt.Fprintln(os.Stderr, "usage: testdata <out.duckdb> [scale factor, default 0.01]")
		os.Exit(2)
	}
	sf := "0.01"
	if len(os.Args) == 3 {
		if _, err := strconv.ParseFloat(os.Args[2], 64); err != nil {
			fmt.Fprintln(os.Stderr, "testdata: scale factor:", err)
			os.Exit(2)
		}
		sf = os.Args[2]
	}
	if err := run(os.Args[1], sf); err != nil {
		fmt.Fprintln(os.Stderr, "testdata:", err)
		os.Exit(1)
	}
}

func run(out, sf string) error {
	if err := os.Remove(out); err != nil && !os.IsNotExist(err) {
		return err
	}
	ctx := context.Background()
	// INSTALL vss/sqlite: ekstensi untuk tes vector search (10b) & SQLite (10c).
	e, err := duckdb.Open(ctx, duckdb.Config{Path: out, InitSQL: "INSTALL tpch; LOAD tpch; INSTALL vss; INSTALL sqlite; CALL dbgen(sf = " + sf + ");"})
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
