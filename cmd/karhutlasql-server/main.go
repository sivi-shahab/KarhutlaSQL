// Command karhutlasql-server adalah server Arrow Flight SQL di atas DuckDB.
package main

import (
	"bytes"
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
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.yaml.in/yaml/v3"
	"google.golang.org/grpc"

	"github.com/bankmega/karhutlasql/internal/accesslog"
	"github.com/bankmega/karhutlasql/internal/admin"
	"github.com/bankmega/karhutlasql/internal/approval"
	"github.com/bankmega/karhutlasql/internal/audit"
	"github.com/bankmega/karhutlasql/internal/auth/local"
	"github.com/bankmega/karhutlasql/internal/auth/oidc"
	"github.com/bankmega/karhutlasql/internal/authz"
	"github.com/bankmega/karhutlasql/internal/cache"
	"github.com/bankmega/karhutlasql/internal/catalogsync"
	"github.com/bankmega/karhutlasql/internal/classification"
	"github.com/bankmega/karhutlasql/internal/config"
	"github.com/bankmega/karhutlasql/internal/dq"
	"github.com/bankmega/karhutlasql/internal/engine"
	"github.com/bankmega/karhutlasql/internal/engine/duckdb"
	"github.com/bankmega/karhutlasql/internal/flightsql"
	"github.com/bankmega/karhutlasql/internal/interceptor"
	"github.com/bankmega/karhutlasql/internal/metrics"
	"github.com/bankmega/karhutlasql/internal/objstore"
	"github.com/bankmega/karhutlasql/internal/quota"
	"github.com/bankmega/karhutlasql/internal/report"
	"github.com/bankmega/karhutlasql/internal/scheduler"
	"github.com/bankmega/karhutlasql/internal/server"
	"github.com/bankmega/karhutlasql/internal/session"
	"github.com/bankmega/karhutlasql/internal/sqlext"
	"github.com/bankmega/karhutlasql/internal/version"
)

// queryHistorySize jumlah statement terakhir di karhutla_query_history().
const queryHistorySize = 10000

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
	gateDir := ""
	if cfg.FourEyes {
		gateDir = cfg.StateDir
	} else {
		log.Warn("four-eyes disabled: policy/classification changes activate on reload (dev only)")
	}
	var syncer *catalogsync.Syncer // diisi setelah engine dibuka; Trigger aman pada nil
	gate := approval.New(gateDir, func(e approval.Event) {
		auditLog.Emit(ctx, audit.Event{Type: audit.PolicyChange, User: e.By, Object: e.Document, Hash: e.Hash, Status: e.Status})
		if e.Status == "activated" {
			syncer.Trigger() // klasifikasi baru → properties UC
		}
	})
	policy, err := newPolicy(cfg, log, gate)
	if err != nil {
		return err
	}
	quotas := quota.New(func() quota.Config { return policy.Policy().Quotas })
	var oidcVerifier *oidc.Verifier
	if cfg.OIDCIssuer != "" {
		oidcVerifier, err = oidc.New(ctx, oidc.Config{Issuer: cfg.OIDCIssuer, Audience: cfg.OIDCAudience,
			UsernameClaim: cfg.OIDCUsernameClaim, GroupsClaim: cfg.OIDCGroupsClaim, ClockSkew: time.Minute})
		if err != nil {
			return err
		}
	}

	cls, maskKey, err := newClassification(cfg, gate)
	if err != nil {
		return err
	}

	archive, err := newArchive(ctx, cfg, log)
	if err != nil {
		return err
	}
	var (
		access  *accesslog.Log
		dqStore *dq.Store
	)
	if archive != nil {
		access = accesslog.New(accesslog.Options{Store: archive})
		if dqStore, err = dq.Open(ctx, archive); err != nil {
			return err
		}
	}
	if cfg.DQMaxAge == 0 {
		log.Warn("DQ gate disabled (--dq-max-age 0): report runs do not require Great Expectations results")
	}

	sessions := session.NewRegistry(cfg.SessionIdleTimeout)
	sched := scheduler.New()
	history := session.NewHistory(queryHistorySize)
	m := metrics.New(sessions.Counts, sched.Stats)
	m.Registry.MustRegister(quotas)
	if dqStore != nil {
		m.Registry.MustRegister(dqStore)
	}
	reports, signer, err := newReports(cfg, log)
	if err != nil {
		return err
	}
	var srv *flightsql.Server // diisi setelah engine dibuka (UDF report memanggilnya)
	secrets, err := loadSecrets(cfg.DuckDBSecretsFile)
	if err != nil {
		return err
	}
	eng, err := duckdb.Open(ctx, duckdb.Config{Path: cfg.DatabaseFilename, InitSQL: initSQL,
		AllowedDirectories: cfg.AllowedDirectories, Secrets: secrets, StreamingBufferSize: streamingBuffer(cfg.StreamingBufferSize),
		Setup: func(ctx context.Context, conn *sql.Conn) error {
			return sqlext.Register(ctx, conn, sqlext.Deps{Quota: quotas, Sessions: sessions, Gatherer: m.Registry, Audit: auditLog, History: history, AccessLog: access, MaskKey: maskKey, DQ: dqStore,
				RunReport: func(ctx context.Context, name, asOf string) ([]any, error) { return srv.ReportRow(ctx, name, asOf) }})
		}})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, eng.Close()) }()
	if err := eng.Streaming(); err != nil && cfg.StreamingBufferSize != "0" {
		log.Warn("result streaming unavailable; results are materialized before the first batch", "reason", err)
	}

	var resultCache *cache.Cache
	if cfg.ResultCacheTTL > 0 {
		resultCache = cache.New(cfg.ResultCacheTTL, cfg.ResultCacheMaxBytes, cfg.ResultCacheMaxEntryBytes)
		m.Registry.MustRegister(resultCache)
	}
	srv, err = flightsql.New(eng, log, flightsql.Options{
		QueryTimeout: cfg.QueryTimeout, PrintQueries: cfg.PrintQueries, Audit: auditLog, Metrics: m, Policy: policy, Classification: cls,
		Scheduler: sched, DefaultGroup: scheduler.Group{MaxConcurrent: cfg.MaxConcurrent, QueueSize: cfg.QueueSize, QueueTimeout: cfg.QueueTimeout},
		History: history, SlowQuery: cfg.SlowQueryThreshold, Profiling: cfg.QueryProfiling, AccessLog: access,
		Reports: reports, Signer: signer, Archive: archive, DQ: dqStore, DQMaxAge: cfg.DQMaxAge,
		MetadataChanged: func() { syncer.Trigger() }, Cache: resultCache, Quota: quotas,
	})
	if err != nil {
		return err
	}
	if cfg.UCEndpoint != "" {
		syncer = catalogsync.New(catalogsync.NewClient(cfg.UCEndpoint, os.Getenv(cfg.UCTokenEnv)), catalogsync.Source{
			Engine: eng, DQ: dqStore, Endpoint: advertisedEndpoint(cfg, tlsCfg != nil),
			Tags: func(table string) map[string]string { return cls.Get().TableTags(table) },
		}, auditLog)
		m.Registry.MustRegister(syncer.Errors())
	}
	health := healthCheck(eng, cfg.HealthCheckQuery)
	ai := interceptor.NewAuth(interceptor.AuthOptions{Local: authn, OIDC: oidcVerifier, Policy: policy, Audit: auditLog, Metrics: m, Log: log})
	si := interceptor.NewSession(sessions, auditLog, m)
	rl := interceptor.NewRateLimit(cfg.RateLimit, cfg.RateBurst, m)

	adminOpts := admin.Options{Health: health, Gatherer: m.Registry, Sessions: sessions, Verify: ai.Verify, Audit: auditLog, AccessLog: access, Gate: gate}

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

	shutdownTracing, err := newTracing(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() {
		sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second) // ctx sudah dibatalkan saat shutdown
		defer scancel()
		if terr := shutdownTracing(sctx); terr != nil { // span hilang ≠ shutdown gagal
			log.Warn("tracing flush failed on shutdown", "err", terr)
		}
	}()
	log.Info("starting", "version", version.Version, "database", cfg.DatabaseFilename, "query_timeout", cfg.QueryTimeout.String(),
		"max_concurrent", cfg.MaxConcurrent, "queue_size", cfg.QueueSize, "rate_limit", cfg.RateLimit)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errc := make(chan error, 2)
	go func() {
		errc <- server.Serve(ctx, lis, srv, server.Options{
			TLS:     tlsCfg,
			Unary:   []grpc.UnaryServerInterceptor{ai.Unary(), rl.Unary(), si.Unary()},
			Stream:  []grpc.StreamServerInterceptor{ai.Stream(), rl.Stream(), si.Stream()},
			Health:  health,
			Drain:   server.DrainTimeout,
			Actions: actions(adminOpts, srv),
			Tracing: cfg.OTelEndpoint != "",
		}, log)
		cancel()
	}()
	go sessions.Run(ctx, time.Minute)
	accessDone := make(chan struct{})
	if access != nil {
		go func() {
			defer close(accessDone)
			access.Run(ctx, cfg.AccessLogFlushInterval, func(err error) { log.Error("access history flush failed", "err", err) })
		}()
	} else {
		close(accessDone)
	}
	defer func() { cancel(); <-accessDone }() // flush terakhir setelah server berhenti
	go reloadOnSIGHUP(ctx, auditLog, log, gate)
	if syncer != nil {
		log.Info("unity catalog sync", "endpoint", cfg.UCEndpoint, "interval", cfg.UCSyncInterval.String())
		syncDone := make(chan struct{})
		go func() {
			defer close(syncDone)
			syncer.Run(ctx, cfg.UCSyncInterval, func(err error) { log.Warn("unity catalog sync failed; retrying", "err", err) })
		}()
		defer func() { cancel(); <-syncDone }() // sebelum engine ditutup
	}
	n := 1
	if adminLis != nil {
		n++
		h := admin.Handler(adminOpts)
		go func() { errc <- admin.Serve(ctx, adminLis, h, log); cancel() }()
	}
	for range n {
		err = errors.Join(err, <-errc)
	}
	return err
}

// newReports definisi report dan kunci tanda tangan (file 0600).
func newReports(cfg config.Config, log *slog.Logger) (map[string]report.Def, *report.Signer, error) {
	defs, err := report.Load(cfg.ReportsFile)
	if err != nil || cfg.ReportSigningKeyFile == "" {
		return defs, nil, err
	}
	if err := server.CheckSecretFile(cfg.ReportSigningKeyFile); err != nil {
		return nil, nil, fmt.Errorf("main.newReports: %w", err)
	}
	s, err := report.LoadSigner(cfg.ReportSigningKeyFile)
	if err != nil {
		return nil, nil, err
	}
	log.Info("report signing key", "key_id", s.KeyID, "reports", len(defs))
	return defs, s, nil
}

// loadSecrets membaca --duckdb-secrets-file; nilai rahasia diambil dari env
// (file tidak memuat secret):
//
//	secrets:
//	  - name: lake
//	    type: s3
//	    scope: s3://lake/
//	    options: { endpoint: "minio:9000", url_style: path, use_ssl: "false" }
//	    env: { key_id: LAKE_KEY_ID, secret: LAKE_SECRET }
func loadSecrets(path string) ([]duckdb.Secret, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("main.loadSecrets: %w", err)
	}
	var f struct {
		Secrets []struct {
			Name, Type, Scope string
			Options, Env      map[string]string
		}
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("main.loadSecrets: %w", err)
	}
	out := make([]duckdb.Secret, 0, len(f.Secrets))
	for _, s := range f.Secrets {
		opts := map[string]string{}
		for k, v := range s.Options {
			opts[k] = v
		}
		for k, env := range s.Env {
			v := os.Getenv(env)
			if v == "" {
				return nil, fmt.Errorf("main.loadSecrets: secret %s: env %s is empty", s.Name, env)
			}
			opts[k] = v
		}
		out = append(out, duckdb.Secret{Name: s.Name, Type: s.Type, Scope: s.Scope, Options: opts})
	}
	return out, nil
}

// newArchive penyimpanan access-history & report; nil bila tidak dikonfigurasi.
func newArchive(ctx context.Context, cfg config.Config, log *slog.Logger) (objstore.Store, error) {
	switch {
	case cfg.ArchiveDir != "":
		log.Warn("archive in local directory: not WORM (dev only)", "dir", cfg.ArchiveDir)
		return objstore.Dir(cfg.ArchiveDir), nil
	case cfg.ArchiveS3Endpoint != "":
		ak, sk := os.Getenv(cfg.ArchiveS3AccessKeyEnv), os.Getenv(cfg.ArchiveS3SecretKeyEnv)
		if ak == "" || sk == "" {
			return nil, fmt.Errorf("main.newArchive: env %s and %s are required", cfg.ArchiveS3AccessKeyEnv, cfg.ArchiveS3SecretKeyEnv)
		}
		st, err := objstore.NewS3(ctx, objstore.S3Config{Endpoint: cfg.ArchiveS3Endpoint, Bucket: cfg.ArchiveS3Bucket, Region: cfg.ArchiveS3Region,
			AccessKey: ak, SecretKey: sk, Insecure: cfg.ArchiveS3Insecure, Retention: cfg.ArchiveRetention})
		if err != nil {
			return nil, err
		}
		log.Info("archive", "store", st.String(), "retention", cfg.ArchiveRetention.String())
		return st, nil
	}
	log.Warn("no archive configured: access history (G-05) and report run (G-10) disabled (dev only)")
	return nil, nil
}

// newTracing memasang tracer provider OTLP gRPC global bila --otel-endpoint
// diset; selain itu no-op. Hasil: fungsi shutdown (flush span tersisa).
func newTracing(ctx context.Context, cfg config.Config) (func(context.Context) error, error) {
	if cfg.OTelEndpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.OTelEndpoint)}
	if cfg.OTelInsecure {
		opts = append(opts, otlptracegrpc.WithInsecure())
	}
	exp, err := otlptracegrpc.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("main.newTracing: %w", err)
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String("service.name", "karhutlasql"), attribute.String("service.version", version.Version)))
	if err != nil {
		return nil, fmt.Errorf("main.newTracing: %w", err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.OTelSampleRatio))))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return tp.Shutdown, nil
}

func newPolicy(cfg config.Config, log *slog.Logger, gate *approval.Gate) (*authz.Store, error) {
	if cfg.PolicyFile == "" {
		log.Warn("no policy file: only the local user is allowed, as admin (dev only)", "user", cfg.Username)
		return authz.Static(authz.DevPolicy(cfg.Username)), nil
	}
	store := authz.NewStore()
	return store, gate.Open(approval.Doc{Name: "policy", Path: cfg.PolicyFile, Prepare: store.Prepare})
}

func newClassification(cfg config.Config, gate *approval.Gate) (*classification.Store, []byte, error) {
	cls, _ := classification.Load("") // path kosong: tidak pernah error
	key := []byte(os.Getenv(cfg.MaskKeyEnv))
	if cfg.ClassificationFile == "" {
		return cls, key, nil
	}
	check := func(c *classification.Classification) error {
		if c.UsesHMAC() && len(key) < 32 {
			return fmt.Errorf("hash_hmac masks need env %s with >= 32 bytes", cfg.MaskKeyEnv)
		}
		return nil
	}
	err := gate.Open(approval.Doc{Name: "classification", Path: cfg.ClassificationFile,
		Prepare: func(b []byte) (func(), error) { return cls.Prepare(b, check) }})
	return cls, key, err
}

// streamingBuffer "0" → kosong (materialisasi).
func streamingBuffer(s string) string {
	if s == "0" {
		return ""
	}
	return s
}

// advertisedEndpoint nilai karhutla.endpoint di Unity Catalog (K-02).
func advertisedEndpoint(cfg config.Config, tls bool) string {
	if cfg.UCAdvertisedEndpoint != "" {
		return cfg.UCAdvertisedEndpoint
	}
	scheme := "grpc://"
	if tls {
		scheme = "grpc+tls://"
	}
	return scheme + cfg.Addr()
}

// actions DoAction tambahan: admin (G-12) di atas operasi admin HTTP, dan
// runner GX (Q-03).
func actions(o admin.Options, srv *flightsql.Server) map[string]func(context.Context, []byte) ([]byte, error) {
	out := map[string]func(context.Context, []byte) ([]byte, error){}
	for _, typ := range admin.Actions {
		out[typ] = func(ctx context.Context, body []byte) ([]byte, error) { return o.Do(ctx, typ, body) }
	}
	for _, typ := range []string{flightsql.ActionDQSnapshot, flightsql.ActionDQPublish} {
		out[typ] = func(ctx context.Context, body []byte) ([]byte, error) { return srv.DQAction(ctx, typ, body) }
	}
	out[flightsql.ActionPIIAccess] = srv.PIIAccessAction
	return out
}

// reloadOnSIGHUP membaca ulang policy & classification: dengan four-eyes isi
// baru menjadi pending (perlu 2 persetujuan admin), tanpa itu langsung aktif;
// file tidak valid → versi lama tetap aktif.
func reloadOnSIGHUP(ctx context.Context, a *audit.Logger, log *slog.Logger, gate *approval.Gate) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGHUP)
	defer signal.Stop(ch)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ch:
			ev := audit.Event{Type: audit.PolicyReload, Status: "OK"}
			var errs []error
			for _, st := range gate.Status() {
				_, err := gate.Propose(st.Document, "")
				errs = append(errs, err)
			}
			if err := errors.Join(errs...); err != nil {
				ev.Status, ev.Error = "FAILED", err.Error()
				log.Error("policy reload failed; keeping previous policy", "err", err)
			} else {
				log.Info("policy reloaded", "four_eyes", gate.Enabled())
			}
			a.Emit(ctx, ev)
		}
	}
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
