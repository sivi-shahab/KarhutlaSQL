// Command karhutlasql-server adalah server Arrow Flight SQL di atas DuckDB.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/bankmega/karhutlasql/internal/config"
	"github.com/bankmega/karhutlasql/internal/engine/duckdb"
	"github.com/bankmega/karhutlasql/internal/flightsql"
	"github.com/bankmega/karhutlasql/internal/server"
	"github.com/bankmega/karhutlasql/internal/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// run mengembalikan exit code agar bisa diuji tanpa os.Exit.
func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	cmd := &cobra.Command{
		Use:           "karhutlasql-server",
		Short:         "Server Arrow Flight SQL di atas DuckDB",
		Version:       version.Version,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(cmd.Flags())
			if err != nil {
				return err
			}
			return serve(ctx, cfg, stderr)
		},
	}
	config.RegisterFlags(cmd.Flags())
	cmd.SetArgs(args)
	cmd.SetOut(stdout)
	cmd.SetErr(stderr)
	if err := cmd.ExecuteContext(ctx); err != nil {
		_, _ = fmt.Fprintln(stderr, "karhutlasql-server:", err)
		return 1
	}
	return 0
}

func serve(ctx context.Context, cfg config.Config, logOut io.Writer) error {
	log := slog.New(slog.NewJSONHandler(logOut, &slog.HandlerOptions{Level: cfg.LogLevel}))

	var initSQL string
	if cfg.InitSQLFile != "" {
		b, err := os.ReadFile(cfg.InitSQLFile)
		if err != nil {
			return fmt.Errorf("main.serve: %w", err)
		}
		initSQL = string(b)
	}
	eng, err := duckdb.Open(ctx, duckdb.Config{Path: cfg.DatabaseFilename, InitSQL: initSQL})
	if err != nil {
		return err
	}
	srv, err := flightsql.New(eng, log)
	if err != nil {
		return errors.Join(err, eng.Close())
	}
	lis, err := (&net.ListenConfig{}).Listen(ctx, "tcp", cfg.Addr())
	if err != nil {
		return errors.Join(fmt.Errorf("main.serve: %w", err), eng.Close())
	}
	log.Info("starting", "version", version.Version, "database", cfg.DatabaseFilename)
	return errors.Join(server.Serve(ctx, lis, srv, server.DrainTimeout, log), eng.Close())
}
