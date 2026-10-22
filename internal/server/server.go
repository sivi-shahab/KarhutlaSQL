package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/apache/arrow-go/v18/arrow/flight"
	fsql "github.com/apache/arrow-go/v18/arrow/flight/flightsql"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// DrainTimeout batas graceful shutdown sebelum koneksi diputus paksa (spec §6: ≤ 30s).
const DrainTimeout = 30 * time.Second

// Options konfigurasi Serve.
type Options struct {
	// TLS nil = plaintext (hanya dev; spec §6 mewajibkan TLS di prod).
	TLS    *tls.Config
	Unary  []grpc.UnaryServerInterceptor
	Stream []grpc.StreamServerInterceptor
	// Health dipanggil oleh gRPC health Check; nil = selalu SERVING.
	Health func(context.Context) error
	Drain  time.Duration
}

// Serve melayani srv di lis sampai ctx selesai, lalu drain maksimal opts.Drain
// sebelum menghentikan paksa. Mengembalikan nil pada shutdown normal.
func Serve(ctx context.Context, lis net.Listener, srv fsql.Server, opts Options, log *slog.Logger) error {
	gopts := []grpc.ServerOption{grpc.ChainUnaryInterceptor(opts.Unary...), grpc.ChainStreamInterceptor(opts.Stream...)}
	if opts.TLS != nil {
		gopts = append(gopts, grpc.Creds(credentials.NewTLS(opts.TLS)))
	}
	gs := grpc.NewServer(gopts...)
	flight.RegisterFlightServiceServer(gs, fsql.NewFlightServer(srv))
	healthpb.RegisterHealthServer(gs, &healthServer{check: opts.Health})

	errc := make(chan error, 1)
	go func() { errc <- gs.Serve(lis) }()
	log.Info("listening", "addr", lis.Addr().String(), "tls", opts.TLS != nil, "mtls", opts.TLS != nil && opts.TLS.ClientAuth == tls.RequireAndVerifyClientCert)

	select {
	case err := <-errc:
		return fmt.Errorf("server.Serve: %w", err)
	case <-ctx.Done():
	}

	log.Info("shutting down", "drain", opts.Drain.String())
	stopped := make(chan struct{})
	go func() { gs.GracefulStop(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(opts.Drain):
		log.Warn("drain timeout, forcing stop")
		gs.Stop()
	}
	if err := <-errc; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
		return fmt.Errorf("server.Serve: %w", err)
	}
	return nil
}

type healthServer struct {
	healthpb.UnimplementedHealthServer
	check func(context.Context) error
}

func (h *healthServer) Check(ctx context.Context, _ *healthpb.HealthCheckRequest) (*healthpb.HealthCheckResponse, error) {
	if h.check != nil {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := h.check(cctx); err != nil {
			return &healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_NOT_SERVING}, nil
		}
	}
	return &healthpb.HealthCheckResponse{Status: healthpb.HealthCheckResponse_SERVING}, nil
}

// LoadTLS membaca sertifikat server; caFile tidak kosong = mTLS wajib.
// File kunci harus tidak bisa dibaca group/other (CLAUDE.md: secret file 0600).
func LoadTLS(certFile, keyFile, caFile string) (*tls.Config, error) {
	if err := CheckSecretFile(keyFile); err != nil {
		return nil, fmt.Errorf("server.LoadTLS: %w", err)
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("server.LoadTLS: %w", err)
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	if caFile != "" {
		pem, err := os.ReadFile(filepath.Clean(caFile))
		if err != nil {
			return nil, fmt.Errorf("server.LoadTLS: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("server.LoadTLS: no certificates in %s", caFile)
		}
		cfg.ClientCAs, cfg.ClientAuth = pool, tls.RequireAndVerifyClientCert
	}
	return cfg, nil
}

// SelfSignedTLS membuat sertifikat ECDSA P-256 self-signed (berlaku 1 tahun)
// untuk localhost dan hosts. Kunci hanya di memori, tidak pernah ke disk;
// klien harus memakai --tls-skip-verify. Dev/container saja.
func SelfSignedTLS(hosts ...string) (*tls.Config, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("server.SelfSignedTLS: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, fmt.Errorf("server.SelfSignedTLS: %w", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "karhutlasql-self-signed"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
		IPAddresses:  []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	}
	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else if h != "" {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("server.SelfSignedTLS: %w", err)
	}
	cert := tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, nil
}

// CheckSecretFile menolak file secret yang bisa dibaca group/other.
func CheckSecretFile(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s has mode %v; secret files must be 0600 or stricter", path, fi.Mode().Perm())
	}
	return nil
}
