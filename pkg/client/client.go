// Package client adalah library klien Go KarhutlaSQL (dipakai CLI): koneksi
// gRPC (TLS/mTLS), login Basic → Bearer, dan eksekusi query Flight SQL.
package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/apache/arrow-go/v18/arrow/flight"
	"github.com/apache/arrow-go/v18/arrow/flight/flightsql"
	"golang.org/x/oauth2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
)

// Config parameter koneksi. TLS aktif bila TLS, SkipVerify, CAFile, atau
// CertFile diset.
type Config struct {
	Addr               string // host:port
	TLS                bool
	SkipVerify         bool   // dev saja: sertifikat server tidak diverifikasi
	CAFile             string // CA PEM untuk verifikasi server; kosong = CA sistem
	CertFile, KeyFile  string // client cert untuk mTLS
	Username, Password string
	// TokenSource (SSO, lihat pkg/client/oauth) menggantikan login password:
	// access token dikirim sebagai Bearer di setiap RPC dan di-refresh otomatis.
	TokenSource oauth2.TokenSource
}

func (c Config) tlsEnabled() bool {
	return c.TLS || c.SkipVerify || c.CAFile != "" || c.CertFile != ""
}

// Client koneksi terautentikasi ke server KarhutlaSQL.
type Client struct {
	cl     *flightsql.Client
	bearer func() (string, error)
}

// Dial membuka koneksi dan login (Handshake Basic → Bearer).
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	creds := insecure.NewCredentials()
	if cfg.tlsEnabled() {
		tc, err := tlsConfig(cfg)
		if err != nil {
			return nil, fmt.Errorf("client.Dial: %w", err)
		}
		creds = credentials.NewTLS(tc)
	}
	cl, err := flightsql.NewClientCtx(ctx, cfg.Addr, nil, nil, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("client.Dial: %w", err)
	}
	if cfg.TokenSource != nil {
		return &Client{cl: cl, bearer: func() (string, error) {
			t, err := cfg.TokenSource.Token()
			if err != nil {
				return "", fmt.Errorf("client: token: %w", err)
			}
			return "Bearer " + t.AccessToken, nil
		}}, nil
	}
	authCtx, err := cl.Client.AuthenticateBasicToken(ctx, cfg.Username, cfg.Password)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("client.Dial: login: %w", err), cl.Close())
	}
	md, _ := metadata.FromOutgoingContext(authCtx)
	auth := md.Get("authorization")
	if len(auth) == 0 {
		return nil, errors.Join(errors.New("client.Dial: server returned no bearer token"), cl.Close())
	}
	b := auth[len(auth)-1]
	return &Client{cl: cl, bearer: func() (string, error) { return b, nil }}, nil
}

func tlsConfig(cfg Config) (*tls.Config, error) {
	tc := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: cfg.SkipVerify} //nolint:gosec // opt-in --tls-skip-verify (dev)
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(filepath.Clean(cfg.CAFile))
		if err != nil {
			return nil, err
		}
		tc.RootCAs = x509.NewCertPool()
		if !tc.RootCAs.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("%s: no PEM certificates", cfg.CAFile)
		}
	}
	if cfg.CertFile != "" {
		cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
		if err != nil {
			return nil, err
		}
		tc.Certificates = []tls.Certificate{cert}
	}
	return tc, nil
}

func (c *Client) ctx(ctx context.Context) (context.Context, error) {
	b, err := c.bearer()
	if err != nil {
		return nil, err
	}
	return metadata.AppendToOutgoingContext(ctx, "authorization", b), nil
}

// Query mengeksekusi sql dan mengembalikan stream hasil; pemanggil wajib
// Release. Pembatalan ctx membatalkan query di server.
func (c *Client) Query(ctx context.Context, sql string) (*flight.Reader, error) {
	ctx, err := c.ctx(ctx)
	if err != nil {
		return nil, err
	}
	info, err := c.cl.Execute(ctx, sql)
	if err != nil {
		return nil, fmt.Errorf("client.Query: %w", err)
	}
	return c.fetch(ctx, info)
}

// Tables mengembalikan daftar tabel (Flight SQL GetTables).
func (c *Client) Tables(ctx context.Context) (*flight.Reader, error) {
	ctx, err := c.ctx(ctx)
	if err != nil {
		return nil, err
	}
	info, err := c.cl.GetTables(ctx, &flightsql.GetTablesOpts{})
	if err != nil {
		return nil, fmt.Errorf("client.Tables: %w", err)
	}
	return c.fetch(ctx, info)
}

// ponytail: satu endpoint saja (server KarhutlaSQL selalu mengembalikan satu);
// gabungkan beberapa endpoint bila server lain didukung.
func (c *Client) fetch(ctx context.Context, info *flight.FlightInfo) (*flight.Reader, error) {
	if len(info.Endpoint) != 1 {
		return nil, fmt.Errorf("client.fetch: %d endpoints, want 1", len(info.Endpoint))
	}
	// ctx sudah membawa Bearer (Query/Tables).
	rdr, err := c.cl.DoGet(ctx, info.Endpoint[0].Ticket)
	if err != nil {
		return nil, fmt.Errorf("client.fetch: %w", err)
	}
	return rdr, nil
}

// Close menutup koneksi.
func (c *Client) Close() error { return c.cl.Close() }
