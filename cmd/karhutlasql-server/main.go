// Command karhutlasql-server adalah server Arrow Flight SQL di atas DuckDB.
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/bankmega/karhutlasql/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run mengembalikan exit code agar bisa diuji tanpa os.Exit.
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("karhutlasql-server", flag.ContinueOnError)
	fs.SetOutput(stderr)
	showVersion := fs.Bool("version", false, "tampilkan versi lalu keluar")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if *showVersion {
		_, _ = fmt.Fprintln(stdout, "karhutlasql-server", version.Version)
		return 0
	}
	_, _ = fmt.Fprintln(stderr, "karhutlasql-server: server belum diimplementasikan (Fase 1); gunakan --version atau --help")
	return 1
}
