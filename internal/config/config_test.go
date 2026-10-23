package config

import (
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/spf13/pflag"
)

func load(t *testing.T, args []string) (Config, error) {
	t.Helper()
	fs := pflag.NewFlagSet("test", pflag.ContinueOnError)
	RegisterFlags(fs)
	if err := fs.Parse(append([]string{"--username", "admin"}, args...)); err != nil {
		t.Fatal(err)
	}
	return Load(fs)
}

func TestLoad_Precedence(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "cfg.yaml")
	if err := os.WriteFile(file, []byte("port: 1111\nhost: file-host\nlog-level: debug\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		env      map[string]string
		args     []string
		wantPort int
		wantHost string
	}{
		{"defaults", nil, nil, 31337, "0.0.0.0"},
		{"file", nil, []string{"--config", file}, 1111, "file-host"},
		{"env over file", map[string]string{"KARHUTLASQL_PORT": "2222"}, []string{"--config", file}, 2222, "file-host"},
		{"flag over env", map[string]string{"KARHUTLASQL_PORT": "2222"}, []string{"--config", file, "--port", "3333"}, 3333, "file-host"},
		{"env config path", map[string]string{"KARHUTLASQL_CONFIG": file}, nil, 1111, "file-host"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			c, err := load(t, tt.args)
			if err != nil {
				t.Fatal(err)
			}
			if c.Port != tt.wantPort || c.Host != tt.wantHost {
				t.Fatalf("got port=%d host=%q, want %d %q", c.Port, c.Host, tt.wantPort, tt.wantHost)
			}
		})
	}
}

func TestLoad_Invalid(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"port range", []string{"--port", "70000"}},
		{"missing init sql file", []string{"--init-sql-file", "/nonexistent/init.sql"}},
		{"bad log level", []string{"--log-level", "loud"}},
		{"missing config file", []string{"--config", "/nonexistent/cfg.yaml"}},
		{"empty username", []string{"--username", ""}},
		{"ttl too long", []string{"--jwt-ttl", "25h"}},
		{"tls cert without key", []string{"--tls-cert", "c.pem"}},
		{"mtls without tls", []string{"--mtls-ca", "ca.pem"}},
		{"self-signed with cert", []string{"--tls-self-signed", "--tls-cert", "c.pem", "--tls-key", "k.pem"}},
		{"negative timeout", []string{"--query-timeout", "-1"}},
		{"negative max concurrent", []string{"--max-concurrent", "-1"}},
		{"zero rate burst", []string{"--rate-limit", "5", "--rate-burst", "0"}},
		{"metrics port clash", []string{"--metrics-port", "31337"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := load(t, tt.args); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestLoad_Durations(t *testing.T) {
	c, err := load(t, []string{"--query-timeout", "300", "--jwt-ttl", "2h"})
	if err != nil || c.QueryTimeout != 300*time.Second || c.JWTTTL != 2*time.Hour {
		t.Fatalf("timeout=%v ttl=%v err=%v", c.QueryTimeout, c.JWTTTL, err)
	}
}

func TestLoad_LogLevel(t *testing.T) {
	c, err := load(t, []string{"--log-level", "warn"})
	if err != nil || c.LogLevel != slog.LevelWarn {
		t.Fatalf("level=%v err=%v", c.LogLevel, err)
	}
}
