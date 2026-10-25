package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// EnvPrefix prefix env var: --database-filename ⇄ KARHUTLASQL_DATABASE_FILENAME.
const EnvPrefix = "KARHUTLASQL"

// Config konfigurasi server. Secret tidak pernah disimpan di sini — hanya
// nama env var atau path file-nya (CLAUDE.md: tidak ada secret di argv).
type Config struct {
	DatabaseFilename string // kosong = in-memory
	Host             string
	Port             int
	InitSQLFile      string
	LogLevel         slog.Level

	Username          string
	PasswordEnv       string
	JWTSecretEnv      string
	JWTPrivateKeyFile string
	JWTTTL            time.Duration

	TLSCert, TLSKey, MTLSCA string
	TLSSelfSigned           bool

	PolicyFile                         string
	ClassificationFile, MaskKeyEnv     string
	OIDCIssuer, OIDCAudience           string
	OIDCUsernameClaim, OIDCGroupsClaim string

	QueryTimeout       time.Duration
	SessionIdleTimeout time.Duration

	MaxConcurrent, QueueSize int
	QueueTimeout             time.Duration
	RateLimit                float64 // request/detik per user; 0 = mati
	RateBurst                int
	SlowQueryThreshold       time.Duration
	QueryProfiling           bool
	StreamingBufferSize      string // "0" = hasil dimaterialisasi

	// Arsip governance (access-history, report): direktori lokal (dev) atau S3 WORM.
	ArchiveDir                                   string
	ArchiveS3Endpoint, ArchiveS3Bucket           string
	ArchiveS3Region                              string
	ArchiveS3AccessKeyEnv, ArchiveS3SecretKeyEnv string
	ArchiveS3Insecure                            bool
	ArchiveRetention                             time.Duration
	AccessLogFlushInterval                       time.Duration

	// Report run (G-10).
	ReportsFile, ReportSigningKeyFile string

	// Data quality (Q-05): umur maksimum validasi GX untuk DQ gate report; 0 = gate mati.
	DQMaxAge time.Duration
	// Result cache (Fase 8b): TTL 0 = mati.
	ResultCacheTTL           time.Duration
	ResultCacheMaxBytes      int64
	ResultCacheMaxEntryBytes int64
	// RevocationSyncInterval poll revocation bersama (multi-instance, butuh arsip).
	RevocationSyncInterval time.Duration
	// Backup bertingkat (Fase 8d): interval 0 = hanya lewat DoAction backup.
	BackupInterval                              time.Duration
	BackupStagingDir                            string
	BackupRetentionDaily, BackupRetentionWeekly time.Duration
	BackupRetentionMonthly                      time.Duration
	// Kunci tanda tangan di Vault Transit (Fase 8d).
	ReportSigningVaultKey string
	VaultTokenFile        string
	// Schema migration (Fase 8d): dir kosong = mati.
	MigrationsDir     string
	MigrationsCatalog string
	// MCP (Fase 8c): port 0 = mati.
	MCPPort int
	// Listener protokol PostgreSQL (backlog fase 9c); 0 = mati.
	PGPort int
	// Endpoint HTTP Arrow IPC (backlog fase 9d); 0 = mati.
	HTTPPort            int
	HTTPAllowedOrigins  []string
	MCPResourceURL      string
	MCPMaxRows          int
	VerifiedQueriesFile string
	// Job terjadwal (backlog fase 9b); kosong = tanpa job.
	JobsFile string
	// OpenTelemetry tracing (OTLP gRPC); endpoint kosong = mati.
	OTelEndpoint    string
	OTelInsecure    bool
	OTelSampleRatio float64

	// Sinkron Unity Catalog (K-01..K-04); UCEndpoint kosong = mati.
	UCEndpoint, UCTokenEnv string
	UCSyncInterval         time.Duration
	UCAdvertisedEndpoint   string // karhutla.endpoint; kosong = dari host:port

	// Four-eyes policy/classification (G-11).
	FourEyes bool
	StateDir string

	// Guardrail residency (G-07).
	AllowedDirectories []string
	DuckDBSecretsFile  string

	HealthCheckQuery string
	MetricsPort      int // 0 = HTTP admin mati
	PrintQueries     bool
	AuditLogDir      string // kosong = stdout
	AuditLogSQL      bool
}

// Addr host:port listener.
func (c Config) Addr() string { return net.JoinHostPort(c.Host, strconv.Itoa(c.Port)) }

// RegisterFlags mendefinisikan flag server di fs.
func RegisterFlags(fs *pflag.FlagSet) {
	fs.String("config", "", "YAML config file (precedence: flag > env > file)")
	fs.String("database-filename", "", ".duckdb file; empty = in-memory")
	fs.String("host", "0.0.0.0", "listen address")
	fs.Int("port", 31337, "Flight SQL gRPC port")
	fs.String("init-sql-file", "", "SQL file executed at startup (INSTALL/LOAD/ATTACH)")
	fs.String("log-level", "info", "debug|info|warn|error")

	fs.String("username", "", "local user (required)")
	fs.String("password-env", "KARHUTLASQL_PASSWORD", "name of the env var holding the password (plaintext or bcrypt hash)")
	fs.String("jwt-secret-env", "KARHUTLASQL_JWT_SECRET", "name of the env var holding the HS256 secret (≥ 32 bytes); empty = random per process")
	fs.String("jwt-private-key-file", "", "RSA PEM key (0600) for RS256; replaces HS256")
	fs.Duration("jwt-ttl", 8*time.Hour, "token lifetime (max 24h)")

	fs.String("tls-cert", "", "server certificate PEM")
	fs.String("tls-key", "", "server key PEM (0600)")
	fs.String("mtls-ca", "", "CA PEM to verify client certificates (mTLS required when set)")
	fs.String("classification-file", "", "classification YAML (column tags + masks, G-03); empty = no masking. Reload: SIGHUP")
	fs.String("mask-hmac-key-env", "KARHUTLASQL_MASK_KEY", "name of the env var holding the HMAC key for the hash_hmac mask (≥ 32 bytes)")
	fs.String("policy-file", "", "policy YAML (spec §5.3); empty = only the local user, as admin (dev). Reload: SIGHUP")
	fs.String("oidc-issuer", "", "OIDC issuer (Keycloak/Entra/Okta); empty = OIDC disabled")
	fs.String("oidc-audience", "karhutlasql", "value required in the aud claim of OIDC tokens")
	fs.String("oidc-username-claim", "preferred_username", "OIDC user name claim (falls back to sub)")
	fs.String("oidc-groups-claim", "groups", "OIDC groups claim")
	fs.Bool("tls-self-signed", false, "TLS with an in-memory self-signed certificate (dev/container; client --tls-skip-verify)")

	fs.Int("query-timeout", 0, "statement execution limit in seconds; 0 = unlimited")
	fs.Duration("session-idle-timeout", time.Hour, "idle sessions are dropped from the registry (their statement/prepared handles go with them)")
	fs.Int("max-concurrent", 8, "concurrently running statements in the default resource group; 0 = unlimited")
	fs.Int("queue-size", 64, "statements allowed to queue in the default resource group; full → RESOURCE_EXHAUSTED")
	fs.Duration("queue-timeout", time.Minute, "max wait for a slot in the default resource group; 0 = unlimited")
	fs.Float64("rate-limit", 0, "RPC requests per second per user (token bucket); 0 = disabled")
	fs.Int("rate-burst", 20, "rate limit token bucket burst")
	fs.Duration("slow-query-threshold", 10*time.Second, "executions (excluding queueing) at least this long are logged as slow_query; 0 = disabled")
	fs.String("streaming-buffer-size", "64MB", "DuckDB streaming result buffer per statement (memory bound for results not yet read by the client); 0 = fully materialize (ADR 34)")
	fs.Bool("query-profiling", false, "per-statement DuckDB profiling for peak memory & plan hash in karhutla_query_history() (costly: +~13% TPC-H SF10 time, ADR 33)")
	fs.String("archive-dir", "", "access-history & report archive in a local directory (dev; not WORM)")
	fs.String("archive-s3-endpoint", "", "archive S3/MinIO endpoint (host:port); bucket must have object-lock")
	fs.String("archive-s3-bucket", "", "archive bucket (object-lock enabled)")
	fs.String("archive-s3-region", "us-east-1", "archive bucket region")
	fs.String("archive-s3-access-key-env", "KARHUTLASQL_ARCHIVE_ACCESS_KEY", "name of the env var holding the archive S3 access key")
	fs.String("archive-s3-secret-key-env", "KARHUTLASQL_ARCHIVE_SECRET_KEY", "name of the env var holding the archive S3 secret key")
	fs.Bool("archive-s3-insecure", false, "archive S3 over plain http (dev)")
	fs.Duration("archive-retention", 87600*time.Hour, "COMPLIANCE object-lock retention for archive objects (default 10 years)")
	fs.Duration("access-log-flush-interval", 30*time.Second, "access-history flush interval to the archive")
	fs.String("reports-file", "", "report definitions (reports.yaml) for karhutla_report_run()")
	fs.String("report-signing-key-file", "", "Ed25519 PKCS#8 PEM private key (0600) that signs report manifests")
	fs.Duration("result-cache-ttl", 0, "lifetime of SELECT results in the result cache (per identity, invalidated when data changes); 0 = disabled")
	fs.Int64("result-cache-max-bytes", 256<<20, "total byte limit of the result cache (LRU)")
	fs.Int64("result-cache-max-entry-bytes", 20<<20, "results larger than this are not cached")
	fs.Duration("revocation-sync-interval", 5*time.Second, "interval for syncing session kills across instances via the archive (revocations/); applies when an archive is configured")
	fs.Duration("backup-interval", 0, "scheduled backup interval to the archive (EXPORT DATABASE of DuckDB catalogs + DuckLake snapshots); 0 = only the backup DoAction")
	fs.String("backup-staging-dir", "", "temporary local directory for EXPORT (added to allowed-directories automatically); default <state-dir>/backup-staging")
	fs.Duration("backup-retention-daily", 7*24*time.Hour, "object-lock retention of daily backups")
	fs.Duration("backup-retention-weekly", 35*24*time.Hour, "object-lock retention of weekly backups (Sunday UTC)")
	fs.Duration("backup-retention-monthly", 400*24*time.Hour, "object-lock retention of monthly backups (1st of the month UTC)")
	fs.String("report-signing-vault-key", "", "Ed25519 key in Vault Transit <mount>/<name> (replaces --report-signing-key-file; env VAULT_ADDR, VAULT_TOKEN, VAULT_NAMESPACE, VAULT_CACERT)")
	fs.String("vault-token-file", "", "Vault token file (e.g. a Vault Agent sink, 0600; re-read on every signature); empty = env VAULT_TOKEN")
	fs.String("migrations-dir", "", "directory of <version>_<name>.sql schema migrations applied at startup (once, in order, one transaction per file)")
	fs.String("migrations-catalog", "", "catalog of the karhutla_schema_migrations tracking table; empty = default catalog")
	fs.Int("http-port", 0, "HTTP Arrow IPC query endpoint port (POST /api/v1/query, Bearer or Basic; TLS follows gRPC) for web apps and notebooks; 0 = disabled")
	fs.StringSlice("http-allowed-origins", nil, "browser origins allowed to call the HTTP query endpoint (CORS), e.g. https://dash.bank.local; empty = no CORS")
	fs.Int("pg-port", 0, "PostgreSQL wire-protocol port for Postgres-only tools (psql, Power BI/Npgsql, ODBC, JDBC pg); same auth, governance and audit as Flight SQL; TLS follows gRPC; 0 = disabled")
	fs.Int("mcp-port", 0, "MCP Streamable HTTP server port (/mcp, Bearer token; TLS follows gRPC) for AI agents; 0 = disabled")
	fs.String("mcp-resource-url", "", "public URL of the MCP endpoint (e.g. https://kq.bank.local:8443/mcp) for RFC 9728 OAuth metadata; empty = no metadata")
	fs.Int("mcp-max-rows", 500, "row limit per MCP tool result (on top of the ai_sandbox profile limits)")
	fs.String("verified-queries-file", "", "YAML of verified queries + semantic model (karhutla_verified_queries(), MCP resources)")
	fs.String("jobs-file", "", "YAML of scheduled jobs (cron): signed report runs and materialized table refreshes, each run as identity job:<name> granted via policy subjects; needs --policy-file")
	fs.String("otel-endpoint", "", "OTLP gRPC collector (host:port) for OpenTelemetry tracing; auth headers via env OTEL_EXPORTER_OTLP_HEADERS; empty = disabled")
	fs.Bool("otel-insecure", false, "OTLP without TLS (dev)")
	fs.Float64("otel-sample-ratio", 1, "sampling ratio for new traces (0..1); traces from clients follow the parent decision")
	fs.Duration("dq-max-age", 26*time.Hour, "report DQ gate: the GX validation of each source table must be younger than this; 0 = gate disabled")
	fs.String("uc-endpoint", "", "Unity Catalog OSS URL (e.g. http://uc:8080) for one-way metadata sync; empty = disabled")
	fs.String("uc-token-env", "KARHUTLASQL_UC_TOKEN", "name of the env var holding the Unity Catalog bearer token (optional)")
	fs.Duration("uc-sync-interval", 15*time.Minute, "full sync interval to Unity Catalog")
	fs.String("uc-advertised-endpoint", "", "Flight SQL endpoint for the karhutla.endpoint property; empty = grpc[+tls]://host:port")
	fs.Bool("four-eyes", true, "policy/classification changes take effect after 2 approvals by different admins (G-11)")
	fs.String("state-dir", "", "server state directory (policy approvals, 0700); default <database dir>/karhutlasql-state")
	fs.StringSlice("allowed-directories", nil, "paths/URL prefixes DuckDB may access (COPY/read_*/ATTACH), e.g. /data/export/,s3://lake/; anything else is rejected")
	fs.String("duckdb-secrets-file", "", "YAML of DuckDB TEMPORARY SECRETs injected by the server (secret values from env; scope must be inside allowed-directories)")
	fs.String("health-check-query", "SELECT 1", "query for gRPC health and /healthz")
	fs.Int("metrics-port", 9090, "admin HTTP port (/healthz); 0 = disabled")
	fs.Bool("print-queries", false, "log SQL text")
	fs.String("audit-log-dir", "", "daily audit log directory (0600); empty = stdout")
	fs.Bool("audit-log-sql", false, "include SQL text in the audit log (default SHA-256 only; beware of PII)")
}

// Load membaca konfigurasi dari flag (yang di-set), env KARHUTLASQL_*, file
// YAML --config, lalu default flag — dengan urutan prioritas itu.
func Load(fs *pflag.FlagSet) (Config, error) {
	v := viper.New()
	v.SetEnvPrefix(EnvPrefix)
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))
	v.AutomaticEnv()
	if err := v.BindPFlags(fs); err != nil {
		return Config{}, fmt.Errorf("config.Load: %w", err)
	}
	if f := v.GetString("config"); f != "" {
		v.SetConfigFile(f)
		if err := v.ReadInConfig(); err != nil {
			return Config{}, fmt.Errorf("config.Load: %w", err)
		}
	}

	c := Config{
		DatabaseFilename: v.GetString("database-filename"),
		Host:             v.GetString("host"),
		Port:             v.GetInt("port"),
		InitSQLFile:      v.GetString("init-sql-file"),

		Username:                 v.GetString("username"),
		PasswordEnv:              v.GetString("password-env"),
		JWTSecretEnv:             v.GetString("jwt-secret-env"),
		JWTPrivateKeyFile:        v.GetString("jwt-private-key-file"),
		JWTTTL:                   v.GetDuration("jwt-ttl"),
		TLSCert:                  v.GetString("tls-cert"),
		TLSKey:                   v.GetString("tls-key"),
		MTLSCA:                   v.GetString("mtls-ca"),
		TLSSelfSigned:            v.GetBool("tls-self-signed"),
		PolicyFile:               v.GetString("policy-file"),
		ClassificationFile:       v.GetString("classification-file"),
		MaskKeyEnv:               v.GetString("mask-hmac-key-env"),
		OIDCIssuer:               v.GetString("oidc-issuer"),
		OIDCAudience:             v.GetString("oidc-audience"),
		OIDCUsernameClaim:        v.GetString("oidc-username-claim"),
		OIDCGroupsClaim:          v.GetString("oidc-groups-claim"),
		QueryTimeout:             time.Duration(v.GetInt("query-timeout")) * time.Second,
		SessionIdleTimeout:       v.GetDuration("session-idle-timeout"),
		MaxConcurrent:            v.GetInt("max-concurrent"),
		QueueSize:                v.GetInt("queue-size"),
		QueueTimeout:             v.GetDuration("queue-timeout"),
		RateLimit:                v.GetFloat64("rate-limit"),
		RateBurst:                v.GetInt("rate-burst"),
		SlowQueryThreshold:       v.GetDuration("slow-query-threshold"),
		QueryProfiling:           v.GetBool("query-profiling"),
		StreamingBufferSize:      v.GetString("streaming-buffer-size"),
		ArchiveDir:               v.GetString("archive-dir"),
		ArchiveS3Endpoint:        v.GetString("archive-s3-endpoint"),
		ArchiveS3Bucket:          v.GetString("archive-s3-bucket"),
		ArchiveS3Region:          v.GetString("archive-s3-region"),
		ArchiveS3AccessKeyEnv:    v.GetString("archive-s3-access-key-env"),
		ArchiveS3SecretKeyEnv:    v.GetString("archive-s3-secret-key-env"),
		ArchiveS3Insecure:        v.GetBool("archive-s3-insecure"),
		ArchiveRetention:         v.GetDuration("archive-retention"),
		AccessLogFlushInterval:   v.GetDuration("access-log-flush-interval"),
		AllowedDirectories:       v.GetStringSlice("allowed-directories"),
		FourEyes:                 v.GetBool("four-eyes"),
		DQMaxAge:                 v.GetDuration("dq-max-age"),
		ResultCacheTTL:           v.GetDuration("result-cache-ttl"),
		ResultCacheMaxBytes:      v.GetInt64("result-cache-max-bytes"),
		ResultCacheMaxEntryBytes: v.GetInt64("result-cache-max-entry-bytes"),
		RevocationSyncInterval:   v.GetDuration("revocation-sync-interval"),
		BackupInterval:           v.GetDuration("backup-interval"),
		BackupStagingDir:         v.GetString("backup-staging-dir"),
		BackupRetentionDaily:     v.GetDuration("backup-retention-daily"),
		BackupRetentionWeekly:    v.GetDuration("backup-retention-weekly"),
		BackupRetentionMonthly:   v.GetDuration("backup-retention-monthly"),
		ReportSigningVaultKey:    v.GetString("report-signing-vault-key"),
		VaultTokenFile:           v.GetString("vault-token-file"),
		MigrationsDir:            v.GetString("migrations-dir"),
		MigrationsCatalog:        v.GetString("migrations-catalog"),
		MCPPort:                  v.GetInt("mcp-port"),
		PGPort:                   v.GetInt("pg-port"),
		HTTPPort:                 v.GetInt("http-port"),
		HTTPAllowedOrigins:       v.GetStringSlice("http-allowed-origins"),
		MCPResourceURL:           v.GetString("mcp-resource-url"),
		MCPMaxRows:               v.GetInt("mcp-max-rows"),
		VerifiedQueriesFile:      v.GetString("verified-queries-file"),
		JobsFile:                 v.GetString("jobs-file"),
		OTelEndpoint:             v.GetString("otel-endpoint"),
		OTelInsecure:             v.GetBool("otel-insecure"),
		OTelSampleRatio:          v.GetFloat64("otel-sample-ratio"),
		UCEndpoint:               v.GetString("uc-endpoint"),
		UCTokenEnv:               v.GetString("uc-token-env"),
		UCSyncInterval:           v.GetDuration("uc-sync-interval"),
		UCAdvertisedEndpoint:     v.GetString("uc-advertised-endpoint"),
		ReportsFile:              v.GetString("reports-file"),
		ReportSigningKeyFile:     v.GetString("report-signing-key-file"),
		StateDir:                 v.GetString("state-dir"),
		DuckDBSecretsFile:        v.GetString("duckdb-secrets-file"),
		HealthCheckQuery:         v.GetString("health-check-query"),
		MetricsPort:              v.GetInt("metrics-port"),
		PrintQueries:             v.GetBool("print-queries"),
		AuditLogDir:              v.GetString("audit-log-dir"),
		AuditLogSQL:              v.GetBool("audit-log-sql"),
	}
	if err := c.LogLevel.UnmarshalText([]byte(v.GetString("log-level"))); err != nil {
		return Config{}, fmt.Errorf("config.Load: log-level: %w", err)
	}
	if c.StateDir == "" && c.DatabaseFilename != "" {
		c.StateDir = filepath.Join(filepath.Dir(c.DatabaseFilename), "karhutlasql-state")
	}
	if c.BackupStagingDir == "" && c.StateDir != "" {
		c.BackupStagingDir = filepath.Join(c.StateDir, "backup-staging")
	}
	if err := c.validate(); err != nil {
		return Config{}, fmt.Errorf("config.Load: %w", err)
	}
	return c, nil
}

func (c Config) validate() error {
	var errs []error
	if c.Port < 1 || c.Port > 65535 {
		errs = append(errs, fmt.Errorf("port %d out of range 1-65535", c.Port))
	}
	if c.Username == "" {
		errs = append(errs, errors.New("username is required (auth is always on)"))
	}
	if c.PasswordEnv == "" {
		errs = append(errs, errors.New("password-env is required"))
	}
	if c.JWTTTL <= 0 || c.JWTTTL > 24*time.Hour {
		errs = append(errs, fmt.Errorf("jwt-ttl %s must be in (0, 24h]", c.JWTTTL))
	}
	if (c.TLSCert == "") != (c.TLSKey == "") {
		errs = append(errs, errors.New("tls-cert and tls-key must be set together"))
	}
	if c.OIDCIssuer != "" && (c.OIDCAudience == "" || c.PolicyFile == "") {
		errs = append(errs, errors.New("oidc-issuer requires oidc-audience and policy-file (allowlist & roles)"))
	}
	if c.TLSSelfSigned && c.TLSCert != "" {
		errs = append(errs, errors.New("tls-self-signed and tls-cert are mutually exclusive"))
	}
	if c.MTLSCA != "" && c.TLSCert == "" {
		errs = append(errs, errors.New("mtls-ca requires tls-cert and tls-key"))
	}
	if c.MaxConcurrent < 0 || c.QueueSize < 0 || c.QueueTimeout < 0 || c.RateLimit < 0 || c.RateBurst < 1 || c.SlowQueryThreshold < 0 {
		errs = append(errs, errors.New("max-concurrent, queue-size, queue-timeout, rate-limit, slow-query-threshold must be >= 0 and rate-burst >= 1"))
	}
	if c.ArchiveDir != "" && c.ArchiveS3Endpoint != "" {
		errs = append(errs, errors.New("archive-dir and archive-s3-endpoint are mutually exclusive"))
	}
	if (c.ArchiveS3Endpoint == "") != (c.ArchiveS3Bucket == "") {
		errs = append(errs, errors.New("archive-s3-endpoint and archive-s3-bucket must be set together"))
	}
	if c.ArchiveRetention <= 0 || c.AccessLogFlushInterval <= 0 {
		errs = append(errs, errors.New("archive-retention and access-log-flush-interval must be > 0"))
	}
	if c.RevocationSyncInterval <= 0 {
		errs = append(errs, errors.New("revocation-sync-interval must be > 0"))
	}
	if c.BackupInterval < 0 || c.BackupRetentionDaily <= 0 || c.BackupRetentionWeekly <= 0 || c.BackupRetentionMonthly <= 0 {
		errs = append(errs, errors.New("backup-interval must be >= 0 and backup-retention-* > 0"))
	}
	if c.ReportSigningVaultKey != "" && c.ReportSigningKeyFile != "" {
		errs = append(errs, errors.New("report-signing-vault-key and report-signing-key-file are mutually exclusive"))
	}
	if c.PGPort < 0 || c.PGPort > 65535 || c.HTTPPort < 0 || c.HTTPPort > 65535 {
		errs = append(errs, errors.New("pg-port and http-port must be 0..65535"))
	}
	if c.MCPPort < 0 || c.MCPPort > 65535 || c.MCPMaxRows <= 0 {
		errs = append(errs, errors.New("mcp-port must be 0..65535 and mcp-max-rows > 0"))
	}
	if c.OTelSampleRatio < 0 || c.OTelSampleRatio > 1 {
		errs = append(errs, errors.New("otel-sample-ratio must be between 0 and 1"))
	}
	if c.ResultCacheTTL < 0 || c.ResultCacheMaxBytes <= 0 || c.ResultCacheMaxEntryBytes <= 0 {
		errs = append(errs, errors.New("result-cache-ttl must be >= 0 and result-cache-max-*bytes > 0"))
	}
	if c.DQMaxAge < 0 || c.UCSyncInterval <= 0 {
		errs = append(errs, errors.New("dq-max-age must be >= 0 and uc-sync-interval > 0"))
	}
	if c.FourEyes && c.StateDir == "" && (c.PolicyFile != "" || c.ClassificationFile != "") {
		errs = append(errs, errors.New("four-eyes with an in-memory database requires --state-dir (approvals must survive restart)"))
	}
	if c.SessionIdleTimeout <= 0 {
		errs = append(errs, errors.New("session-idle-timeout must be > 0"))
	}
	if c.QueryTimeout < 0 {
		errs = append(errs, errors.New("query-timeout must be >= 0"))
	}
	if c.MetricsPort < 0 || c.MetricsPort > 65535 || (c.MetricsPort != 0 && c.MetricsPort == c.Port) {
		errs = append(errs, fmt.Errorf("metrics-port %d invalid (0-65535, different from port)", c.MetricsPort))
	}
	if strings.TrimSpace(c.HealthCheckQuery) == "" {
		errs = append(errs, errors.New("health-check-query must not be empty"))
	}
	if c.InitSQLFile != "" {
		if _, err := os.Stat(c.InitSQLFile); err != nil {
			errs = append(errs, fmt.Errorf("init-sql-file: %w", err))
		}
	}
	return errors.Join(errs...)
}
