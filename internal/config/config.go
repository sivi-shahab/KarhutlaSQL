package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
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

	QueryTimeout     time.Duration
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

	fs.Int("query-timeout", 0, "batas eksekusi statement dalam detik; 0 = tanpa batas")
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

		Username:          v.GetString("username"),
		PasswordEnv:       v.GetString("password-env"),
		JWTSecretEnv:      v.GetString("jwt-secret-env"),
		JWTPrivateKeyFile: v.GetString("jwt-private-key-file"),
		JWTTTL:            v.GetDuration("jwt-ttl"),
		TLSCert:           v.GetString("tls-cert"),
		TLSKey:            v.GetString("tls-key"),
		MTLSCA:            v.GetString("mtls-ca"),
		QueryTimeout:      time.Duration(v.GetInt("query-timeout")) * time.Second,
		HealthCheckQuery:  v.GetString("health-check-query"),
		MetricsPort:       v.GetInt("metrics-port"),
		PrintQueries:      v.GetBool("print-queries"),
		AuditLogDir:       v.GetString("audit-log-dir"),
		AuditLogSQL:       v.GetBool("audit-log-sql"),
	}
	if err := c.LogLevel.UnmarshalText([]byte(v.GetString("log-level"))); err != nil {
		return Config{}, fmt.Errorf("config.Load: log-level: %w", err)
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
	if c.MTLSCA != "" && c.TLSCert == "" {
		errs = append(errs, errors.New("mtls-ca requires tls-cert and tls-key"))
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
