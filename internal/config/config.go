package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// EnvPrefix prefix env var: --database-filename ⇄ KARHUTLASQL_DATABASE_FILENAME.
const EnvPrefix = "KARHUTLASQL"

// Config konfigurasi server.
type Config struct {
	DatabaseFilename string // kosong = in-memory
	Host             string
	Port             int
	InitSQLFile      string
	LogLevel         slog.Level
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
	if c.InitSQLFile != "" {
		if _, err := os.Stat(c.InitSQLFile); err != nil {
			errs = append(errs, fmt.Errorf("init-sql-file: %w", err))
		}
	}
	return errors.Join(errs...)
}
