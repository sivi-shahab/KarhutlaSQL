// Command karhutlasql-server adalah server Arrow Flight SQL di atas DuckDB.
package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/spf13/cobra"
	"google.golang.org/grpc"

	"github.com/bankmega/karhutlasql/internal/admin"
	"github.com/bankmega/karhutlasql/internal/audit"
	"github.com/bankmega/karhutlasql/internal/auth/local"
	"github.com/bankmega/karhutlasql/internal/config"
	"github.com/bankmega/karhutlasql/internal/engine"
	"github.com/bankmega/karhutlasql/internal/engine/duckdb"
	"github.com/bankmega/karhutlasql/internal/flightsql"
	"github.com/bankmega/karhutlasql/internal/interceptor"
	"github.com/bankmega/karhutlasql/internal/metrics"
	"github.com/bankmega/karhutlasql/internal/server"
	"github.com/bankmega/karhutlasql/internal/session"
	"github.com/bankmega/karhutlasql/internal/sqlext"
	"github.com/bankmega/karhutlasql/internal/version"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// run mengembalikan exit code agar bisa diuji tanpa os.Exit. Log ke stderr,
// audit ke stdout (atau --audit-log-dir).
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
			return serve(ctx, cfg, stdout, stderr)
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

func serve(ctx context.Context, cfg config.Config, auditOut, logOut io.Writer) (err error) {
	log := slog.New(slog.NewJSONHandler(logOut, &slog.HandlerOptions{Level: cfg.LogLevel}))

	auditLog, closeAudit, err := newAudit(cfg, auditOut, log)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, closeAudit()) }()

	authn, err := newAuthenticator(cfg, log)
	if err != nil {
		return err
	}
	var tlsCfg *tls.Config
	switch {
	case cfg.TLSCert != "":
		if tlsCfg, err = server.LoadTLS(cfg.TLSCert, cfg.TLSKey, cfg.MTLSCA); err != nil {
			return err
		}
	case cfg.TLSSelfSigned:
		if tlsCfg, err = server.SelfSignedTLS(cfg.Host); err != nil {
			return err
		}
		log.Warn("TLS with in-memory self-signed certificate (dev only; clients need --tls-skip-verify)")
	default:
		log.Warn("TLS disabled: plaintext gRPC (dev only; spec requires TLS in production)")
	}

	var initSQL string
	if cfg.InitSQLFile != "" {
		b, err := os.ReadFile(cfg.InitSQLFile)
		if err != nil {
			return fmt.Errorf("main.serve: %w", err)
		}
		initSQL = string(b)
	}
	sessions := session.NewRegistry(cfg.SessionIdleTimeout)
	m := metrics.New(sessions.Counts)
	eng, err := duckdb.Open(ctx, duckdb.Config{Path: cfg.DatabaseFilename, InitSQL: initSQL,
		Setup: func(ctx context.Context, conn *sql.Conn) error {
			return sqlext.Register(ctx, conn, sqlext.Deps{Sessions: sessions, Gatherer: m.Registry, Audit: auditLog})
		}})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, eng.Close()) }()

	srv, err := flightsql.New(eng, log, flightsql.Options{QueryTimeout: cfg.QueryTimeout, PrintQueries: cfg.PrintQueries, Audit: auditLog, Metrics: m})
	if err != nil {
		return err
	}
	health := healthCheck(eng, cfg.HealthCheckQuery)
	ai := interceptor.NewAuth(authn, auditLog, m, log)
	si := interceptor.NewSession(sessions, auditLog, m)

	lc := &net.ListenConfig{}
	lis, err := lc.Listen(ctx, "tcp", cfg.Addr())
	if err != nil {
		return fmt.Errorf("main.serve: %w", err)
	}
	var adminLis net.Listener
	if cfg.MetricsPort != 0 {
		if adminLis, err = lc.Listen(ctx, "tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.MetricsPort))); err != nil {
			return errors.Join(fmt.Errorf("main.serve: %w", err), lis.Close())
		}
		// /admin/* membawa Bearer token → TLS bila gRPC TLS; tanpa client cert
		// agar probe & Prometheus tetap bisa (auth admin = Bearer + role).
		if tlsCfg != nil {
			at := tlsCfg.Clone()
			at.ClientAuth, at.ClientCAs = tls.NoClientCert, nil
			adminLis = tls.NewListener(adminLis, at)
		}
	}

	log.Info("starting", "version", version.Version, "database", cfg.DatabaseFilename, "query_timeout", cfg.QueryTimeout.String())
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 2)
	go func() {
		errc <- server.Serve(ctx, lis, srv, server.Options{
			TLS:    tlsCfg,
			Unary:  []grpc.UnaryServerInterceptor{ai.Unary(), si.Unary()},
			Stream: []grpc.StreamServerInterceptor{ai.Stream(), si.Stream()},
			Health: health,
			Drain:  server.DrainTimeout,
		}, log)
		cancel()
	}()
	go sessions.Run(ctx, time.Minute)
	n := 1
	if adminLis != nil {
		n++
		h := admin.Handler(admin.Options{Health: health, Gatherer: m.Registry, Sessions: sessions, Verify: authn.Verify, Audit: auditLog})
		go func() { errc <- admin.Serve(ctx, adminLis, h, log); cancel() }()
	}
	for range n {
		err = errors.Join(err, <-errc)
	}
	return err
}

func newAudit(cfg config.Config, stdout io.Writer, log *slog.Logger) (*audit.Logger, func() error, error) {
	onErr := func(err error) { log.Error("audit write failed", "err", err) }
	if cfg.AuditLogDir == "" {
		return audit.New(audit.NewWriterSink(stdout), cfg.AuditLogSQL, onErr), func() error { return nil }, nil
	}
	fs, err := audit.NewFileSink(cfg.AuditLogDir)
	if err != nil {
		return nil, nil, err
	}
	return audit.New(fs, cfg.AuditLogSQL, onErr), fs.Close, nil
}

func newAuthenticator(cfg config.Config, log *slog.Logger) (*local.Authenticator, error) {
	password := os.Getenv(cfg.PasswordEnv)
	if password == "" {
		return nil, fmt.Errorf("main.newAuthenticator: env %s is empty", cfg.PasswordEnv)
	}
	lc := local.Config{Username: cfg.Username, Password: password, TTL: cfg.JWTTTL}
	switch {
	case cfg.JWTPrivateKeyFile != "":
		key, err := readRSAKey(cfg.JWTPrivateKeyFile)
		if err != nil {
			return nil, err
		}
		lc.RSAKey = key
	case os.Getenv(cfg.JWTSecretEnv) != "":
		lc.HMACSecret = []byte(os.Getenv(cfg.JWTSecretEnv))
	default:
		lc.HMACSecret = make([]byte, 32)
		if _, err := rand.Read(lc.HMACSecret); err != nil {
			return nil, fmt.Errorf("main.newAuthenticator: %w", err)
		}
		log.Warn("JWT secret not set: using a random per-process secret; tokens become invalid on restart", "env", cfg.JWTSecretEnv)
	}
	return local.New(lc)
}

func readRSAKey(path string) (*rsa.PrivateKey, error) {
	if err := server.CheckSecretFile(path); err != nil {
		return nil, fmt.Errorf("main.readRSAKey: %w", err)
	}
	pem, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("main.readRSAKey: %w", err)
	}
	key, err := jwt.ParseRSAPrivateKeyFromPEM(pem)
	if err != nil {
		return nil, fmt.Errorf("main.readRSAKey: %w", err)
	}
	return key, nil
}

func healthCheck(eng engine.Engine, query string) func(context.Context) error {
	return func(ctx context.Context) error {
		rdr, err := eng.Query(ctx, query)
		if err != nil {
			return err
		}
		defer rdr.Release()
		for rdr.Next() {
		}
		return rdr.Err()
	}
}
