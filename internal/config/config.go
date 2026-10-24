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
	fs.String("config", "", "file konfigurasi YAML (prioritas: flag > env > file)")
	fs.String("database-filename", "", "file .duckdb; kosong = in-memory")
	fs.String("host", "0.0.0.0", "alamat listen")
	fs.Int("port", 31337, "port gRPC Flight SQL")
	fs.String("init-sql-file", "", "file SQL yang dieksekusi saat start (INSTALL/LOAD/ATTACH)")
	fs.String("log-level", "info", "debug|info|warn|error")

	fs.String("username", "", "user lokal (wajib)")
	fs.String("password-env", "KARHUTLASQL_PASSWORD", "nama env var berisi password (plaintext atau hash bcrypt)")
	fs.String("jwt-secret-env", "KARHUTLASQL_JWT_SECRET", "nama env var berisi secret HS256 (≥ 32 byte); kosong = acak per proses")
	fs.String("jwt-private-key-file", "", "kunci RSA PEM (0600) untuk RS256; menggantikan HS256")
	fs.Duration("jwt-ttl", 8*time.Hour, "umur token (maks 24h)")

	fs.String("tls-cert", "", "sertifikat server PEM")
	fs.String("tls-key", "", "kunci server PEM (0600)")
	fs.String("mtls-ca", "", "CA PEM untuk verifikasi client cert (mTLS wajib bila diset)")
	fs.String("classification-file", "", "classification YAML (tag kolom + mask, G-03); kosong = tanpa mask. Reload: SIGHUP")
	fs.String("mask-hmac-key-env", "KARHUTLASQL_MASK_KEY", "nama env var berisi kunci HMAC mask hash_hmac (≥ 32 byte)")
	fs.String("policy-file", "", "policy YAML (spec §5.3); kosong = hanya user lokal sebagai admin (dev). Reload: SIGHUP")
	fs.String("oidc-issuer", "", "issuer OIDC (Keycloak/Entra/Okta); kosong = OIDC mati")
	fs.String("oidc-audience", "karhutlasql", "nilai wajib di claim aud token OIDC")
	fs.String("oidc-username-claim", "preferred_username", "claim nama user OIDC (fallback sub)")
	fs.String("oidc-groups-claim", "groups", "claim grup OIDC")
	fs.Bool("tls-self-signed", false, "TLS dengan sertifikat self-signed di memori (dev/container; klien --tls-skip-verify)")

	fs.Int("query-timeout", 0, "batas eksekusi statement dalam detik; 0 = tanpa batas")
	fs.Duration("session-idle-timeout", time.Hour, "sesi tanpa aktivitas dibuang dari registry (statement/prepared handle-nya ikut hilang)")
	fs.Int("max-concurrent", 8, "statement berjalan bersamaan di resource group default; 0 = tanpa batas")
	fs.Int("queue-size", 64, "statement yang boleh antre di resource group default; penuh → RESOURCE_EXHAUSTED")
	fs.Duration("queue-timeout", time.Minute, "batas menunggu slot di resource group default; 0 = tanpa batas")
	fs.Float64("rate-limit", 0, "request RPC per detik per user (token bucket); 0 = mati")
	fs.Int("rate-burst", 20, "burst token bucket rate limit")
	fs.Duration("slow-query-threshold", 10*time.Second, "eksekusi (di luar antre) selama ini dicatat ke log slow_query; 0 = mati")
	fs.String("streaming-buffer-size", "64MB", "buffer hasil streaming DuckDB per statement (batas memori hasil yang belum dibaca klien); 0 = materialisasi penuh (ADR 34)")
	fs.Bool("query-profiling", false, "profiling DuckDB per statement untuk peak memory & plan hash di karhutla_query_history() (mahal: +~13% waktu TPC-H SF10, ADR 33)")
	fs.String("archive-dir", "", "arsip access-history & report di direktori lokal (dev; bukan WORM)")
	fs.String("archive-s3-endpoint", "", "endpoint S3/MinIO arsip (host:port); bucket wajib object-lock")
	fs.String("archive-s3-bucket", "", "bucket arsip (object-lock aktif)")
	fs.String("archive-s3-region", "us-east-1", "region bucket arsip")
	fs.String("archive-s3-access-key-env", "KARHUTLASQL_ARCHIVE_ACCESS_KEY", "nama env var access key S3 arsip")
	fs.String("archive-s3-secret-key-env", "KARHUTLASQL_ARCHIVE_SECRET_KEY", "nama env var secret key S3 arsip")
	fs.Bool("archive-s3-insecure", false, "S3 arsip lewat http (dev)")
	fs.Duration("archive-retention", 87600*time.Hour, "retensi object-lock COMPLIANCE objek arsip (default 10 tahun)")
	fs.Duration("access-log-flush-interval", 30*time.Second, "interval flush access-history ke arsip")
	fs.String("reports-file", "", "definisi report (reports.yaml) untuk karhutla_report_run()")
	fs.String("report-signing-key-file", "", "kunci privat Ed25519 PKCS#8 PEM (0600) penanda tangan manifest report")
	fs.Duration("dq-max-age", 26*time.Hour, "DQ gate report: validasi GX tiap tabel sumber harus lebih muda dari ini; 0 = gate mati")
	fs.String("uc-endpoint", "", "URL Unity Catalog OSS (mis. http://uc:8080) untuk sinkron metadata satu arah; kosong = mati")
	fs.String("uc-token-env", "KARHUTLASQL_UC_TOKEN", "nama env var bearer token Unity Catalog (opsional)")
	fs.Duration("uc-sync-interval", 15*time.Minute, "interval sinkron penuh ke Unity Catalog")
	fs.String("uc-advertised-endpoint", "", "endpoint Flight SQL untuk property karhutla.endpoint; kosong = grpc[+tls]://host:port")
	fs.Bool("four-eyes", true, "perubahan policy/classification aktif setelah 2 persetujuan admin berbeda (G-11)")
	fs.String("state-dir", "", "direktori state server (persetujuan policy, 0700); default <dir database>/karhutlasql-state")
	fs.StringSlice("allowed-directories", nil, "path/prefix URL yang boleh diakses DuckDB (COPY/read_*/ATTACH), mis. /data/export/,s3://lake/; selain itu ditolak")
	fs.String("duckdb-secrets-file", "", "YAML TEMPORARY SECRET DuckDB yang diinjeksi server (nilai rahasia dari env; scope wajib di allowed-directories)")
	fs.String("health-check-query", "SELECT 1", "query untuk gRPC health dan /healthz")
	fs.Int("metrics-port", 9090, "port HTTP admin (/healthz); 0 = mati")
	fs.Bool("print-queries", false, "catat teks SQL ke log")
	fs.String("audit-log-dir", "", "direktori audit log harian (0600); kosong = stdout")
	fs.Bool("audit-log-sql", false, "sertakan teks SQL di audit (default hanya SHA-256; hati-hati PII)")
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

		Username:               v.GetString("username"),
		PasswordEnv:            v.GetString("password-env"),
		JWTSecretEnv:           v.GetString("jwt-secret-env"),
		JWTPrivateKeyFile:      v.GetString("jwt-private-key-file"),
		JWTTTL:                 v.GetDuration("jwt-ttl"),
		TLSCert:                v.GetString("tls-cert"),
		TLSKey:                 v.GetString("tls-key"),
		MTLSCA:                 v.GetString("mtls-ca"),
		TLSSelfSigned:          v.GetBool("tls-self-signed"),
		PolicyFile:             v.GetString("policy-file"),
		ClassificationFile:     v.GetString("classification-file"),
		MaskKeyEnv:             v.GetString("mask-hmac-key-env"),
		OIDCIssuer:             v.GetString("oidc-issuer"),
		OIDCAudience:           v.GetString("oidc-audience"),
		OIDCUsernameClaim:      v.GetString("oidc-username-claim"),
		OIDCGroupsClaim:        v.GetString("oidc-groups-claim"),
		QueryTimeout:           time.Duration(v.GetInt("query-timeout")) * time.Second,
		SessionIdleTimeout:     v.GetDuration("session-idle-timeout"),
		MaxConcurrent:          v.GetInt("max-concurrent"),
		QueueSize:              v.GetInt("queue-size"),
		QueueTimeout:           v.GetDuration("queue-timeout"),
		RateLimit:              v.GetFloat64("rate-limit"),
		RateBurst:              v.GetInt("rate-burst"),
		SlowQueryThreshold:     v.GetDuration("slow-query-threshold"),
		QueryProfiling:         v.GetBool("query-profiling"),
		StreamingBufferSize:    v.GetString("streaming-buffer-size"),
		ArchiveDir:             v.GetString("archive-dir"),
		ArchiveS3Endpoint:      v.GetString("archive-s3-endpoint"),
		ArchiveS3Bucket:        v.GetString("archive-s3-bucket"),
		ArchiveS3Region:        v.GetString("archive-s3-region"),
		ArchiveS3AccessKeyEnv:  v.GetString("archive-s3-access-key-env"),
		ArchiveS3SecretKeyEnv:  v.GetString("archive-s3-secret-key-env"),
		ArchiveS3Insecure:      v.GetBool("archive-s3-insecure"),
		ArchiveRetention:       v.GetDuration("archive-retention"),
		AccessLogFlushInterval: v.GetDuration("access-log-flush-interval"),
		AllowedDirectories:     v.GetStringSlice("allowed-directories"),
		FourEyes:               v.GetBool("four-eyes"),
		DQMaxAge:               v.GetDuration("dq-max-age"),
		UCEndpoint:             v.GetString("uc-endpoint"),
		UCTokenEnv:             v.GetString("uc-token-env"),
		UCSyncInterval:         v.GetDuration("uc-sync-interval"),
		UCAdvertisedEndpoint:   v.GetString("uc-advertised-endpoint"),
		ReportsFile:            v.GetString("reports-file"),
		ReportSigningKeyFile:   v.GetString("report-signing-key-file"),
		StateDir:               v.GetString("state-dir"),
		DuckDBSecretsFile:      v.GetString("duckdb-secrets-file"),
		HealthCheckQuery:       v.GetString("health-check-query"),
		MetricsPort:            v.GetInt("metrics-port"),
		PrintQueries:           v.GetBool("print-queries"),
		AuditLogDir:            v.GetString("audit-log-dir"),
		AuditLogSQL:            v.GetBool("audit-log-sql"),
	}
	if err := c.LogLevel.UnmarshalText([]byte(v.GetString("log-level"))); err != nil {
		return Config{}, fmt.Errorf("config.Load: log-level: %w", err)
	}
	if c.StateDir == "" && c.DatabaseFilename != "" {
		c.StateDir = filepath.Join(filepath.Dir(c.DatabaseFilename), "karhutlasql-state")
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
