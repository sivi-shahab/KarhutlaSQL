// Package version menyimpan versi build, diisi via -ldflags saat make build.
package version

// Version di-override: -ldflags "-X github.com/bankmega/karhutlasql/internal/version.Version=v1.2.3".
var Version = "dev"
