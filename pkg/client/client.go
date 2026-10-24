// Package client adalah library klien Go KarhutlaSQL (dipakai CLI): koneksi
// gRPC (TLS/mTLS), login Basic → Bearer, dan eksekusi query Flight SQL.
package client

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
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
	// NoLogin: tanpa login; setiap panggilan wajib membawa token lewat
	// WithBearer (mis. gateway yang meneruskan token pemanggil).
	NoLogin bool
	// Metadata header gRPC tambahan di setiap RPC (mis. kanal "mcp").
	Metadata map[string]string
	// DialOptions opsi grpc tambahan (mis. dialer in-process).
	DialOptions []grpc.DialOption
}

func (c Config) tlsEnabled() bool {
	return c.TLS || c.SkipVerify || c.CAFile != "" || c.CertFile != ""
}

// Client koneksi terautentikasi ke server KarhutlaSQL.
type Client struct {
	cl     *flightsql.Client
	bearer func() (string, error)
	md     []string
}

type bearerKey struct{}

// WithBearer membuat panggilan dengan ctx ini memakai token (tanpa "Bearer ")
// menggantikan kredensial Client.
func WithBearer(ctx context.Context, token string) context.Context {
	return context.WithValue(ctx, bearerKey{}, "Bearer "+token)
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
	cl, err := flightsql.NewClientCtx(ctx, cfg.Addr, nil, nil, append([]grpc.DialOption{grpc.WithTransportCredentials(creds)}, cfg.DialOptions...)...)
	if err != nil {
		return nil, fmt.Errorf("client.Dial: %w", err)
	}
	var md []string
	for k, v := range cfg.Metadata {
		md = append(md, k, v)
	}
	if cfg.NoLogin {
		return &Client{cl: cl, md: md, bearer: func() (string, error) { return "", errors.New("client: no bearer token in context") }}, nil
	}
	if cfg.TokenSource != nil {
		return &Client{cl: cl, md: md, bearer: func() (string, error) {
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
	out, _ := metadata.FromOutgoingContext(authCtx)
	auth := out.Get("authorization")
	if len(auth) == 0 {
		return nil, errors.Join(errors.New("client.Dial: server returned no bearer token"), cl.Close())
	}
	b := auth[len(auth)-1]
	return &Client{cl: cl, md: md, bearer: func() (string, error) { return b, nil }}, nil
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
	b, ok := ctx.Value(bearerKey{}).(string)
	if !ok {
		var err error
		if b, err = c.bearer(); err != nil {
			return nil, err
		}
	}
	return metadata.AppendToOutgoingContext(ctx, append([]string{"authorization", b}, c.md...)...), nil
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

// Action menjalankan Flight DoAction typ dengan body (JSON) dan mengembalikan
// body hasil pertama (nil bila tidak ada).
func (c *Client) Action(ctx context.Context, typ string, body []byte) ([]byte, error) {
	ctx, err := c.ctx(ctx)
	if err != nil {
		return nil, err
	}
	stream, err := c.cl.Client.DoAction(ctx, &flight.Action{Type: typ, Body: body})
	if err != nil {
		return nil, fmt.Errorf("client.Action: %w", err)
	}
	var out []byte
	for {
		res, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("client.Action: %w", err)
		}
		if out == nil {
			out = res.GetBody()
		}
	}
}

// Exec mengeksekusi statement tanpa hasil baris (DDL/DML) dan mengembalikan
// jumlah baris terdampak.
func (c *Client) Exec(ctx context.Context, sql string) (int64, error) {
	ctx, err := c.ctx(ctx)
	if err != nil {
		return 0, err
	}
	n, err := c.cl.ExecuteUpdate(ctx, sql)
	if err != nil {
		return 0, fmt.Errorf("client.Exec: %w", err)
	}
	return n, nil
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

// GetTables daftar tabel yang boleh dilihat pemanggil, opsional dengan
// schema Arrow per tabel (opts.IncludeSchema).
func (c *Client) GetTables(ctx context.Context, opts *flightsql.GetTablesOpts) (*flight.Reader, error) {
	ctx, err := c.ctx(ctx)
	if err != nil {
		return nil, err
	}
	info, err := c.cl.GetTables(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("client.GetTables: %w", err)
	}
	return c.fetch(ctx, info)
}

// Catalogs daftar catalog yang boleh dilihat pemanggil.
func (c *Client) Catalogs(ctx context.Context) (*flight.Reader, error) {
	ctx, err := c.ctx(ctx)
	if err != nil {
		return nil, err
	}
	info, err := c.cl.GetCatalogs(ctx)
	if err != nil {
		return nil, fmt.Errorf("client.Catalogs: %w", err)
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
