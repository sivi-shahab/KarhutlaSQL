// Package objstore menyimpan objek append-only (access-history, report):
// direktori lokal (dev/test) atau bucket S3/MinIO dengan object-lock (WORM).
// Objek tidak pernah ditimpa: Put pada key yang sudah ada → error.
package objstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// ErrExists key sudah ada (append-only).
var ErrExists = errors.New("object already exists")

// Store penyimpanan objek append-only. Key memakai "/" sebagai pemisah.
type Store interface {
	Put(ctx context.Context, key string, data []byte) error
	Get(ctx context.Context, key string) ([]byte, error)
	// List key berawalan prefix, terurut.
	List(ctx context.Context, prefix string) ([]string, error)
	// String lokasi untuk ditampilkan (tanpa kredensial).
	String() string
}

// Dir Store di direktori lokal (file 0600, direktori 0700). Bukan WORM:
// hanya untuk dev/test.
type Dir string

func (d Dir) path(key string) (string, error) {
	if key == "" || strings.Contains(key, "..") || strings.HasPrefix(key, "/") {
		return "", fmt.Errorf("objstore: invalid key %q", key)
	}
	return filepath.Join(string(d), filepath.FromSlash(key)), nil
}

// Put lihat Store.
func (d Dir) Put(_ context.Context, key string, data []byte) error {
	p, err := d.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return fmt.Errorf("objstore.Put: %w", err)
	}
	f, err := os.OpenFile(filepath.Clean(p), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if errors.Is(err, fs.ErrExist) {
		return fmt.Errorf("objstore.Put %s: %w", key, ErrExists)
	}
	if err != nil {
		return fmt.Errorf("objstore.Put: %w", err)
	}
	_, err = f.Write(data)
	if err = errors.Join(err, f.Sync(), f.Close()); err != nil {
		return fmt.Errorf("objstore.Put: %w", err)
	}
	return nil
}

// Get lihat Store.
func (d Dir) Get(_ context.Context, key string) ([]byte, error) {
	p, err := d.path(key)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Clean(p))
	if err != nil {
		return nil, fmt.Errorf("objstore.Get: %w", err)
	}
	return b, nil
}

// List lihat Store.
func (d Dir) List(_ context.Context, prefix string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(string(d), func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if e.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(string(d), p)
		if err != nil {
			return err
		}
		if key := filepath.ToSlash(rel); strings.HasPrefix(key, prefix) {
			out = append(out, key)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("objstore.List: %w", err)
	}
	sort.Strings(out)
	return out, nil
}

func (d Dir) String() string { return "file://" + string(d) }

// S3Config koneksi bucket S3/MinIO.
type S3Config struct {
	Endpoint, Bucket, Region string
	AccessKey, SecretKey     string
	Insecure                 bool // http, bukan https (dev)
	// Retention retensi object-lock mode COMPLIANCE per objek (tidak bisa
	// diperpendek/dihapus sebelum lewat, bahkan oleh root bucket).
	Retention time.Duration
}

// S3 Store di bucket S3 dengan object-lock.
type S3 struct {
	c   *minio.Client
	cfg S3Config
}

// NewS3 memastikan bucket ada dan object-lock aktif (fail-fast: tanpa
// object-lock access-history tidak WORM).
func NewS3(ctx context.Context, cfg S3Config) (*S3, error) {
	if cfg.Retention <= 0 {
		return nil, errors.New("objstore.NewS3: retention must be > 0")
	}
	c, err := minio.New(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: !cfg.Insecure, Region: cfg.Region,
	})
	if err != nil {
		return nil, fmt.Errorf("objstore.NewS3: %w", err)
	}
	enabled, _, _, _, err := c.GetObjectLockConfig(ctx, cfg.Bucket)
	if err != nil {
		return nil, fmt.Errorf("objstore.NewS3: bucket %s object-lock: %w", cfg.Bucket, err)
	}
	if enabled != "Enabled" {
		return nil, fmt.Errorf("objstore.NewS3: bucket %s must have object-lock enabled", cfg.Bucket)
	}
	return &S3{c: c, cfg: cfg}, nil
}

// Put lihat Store. Key yang sudah ada ditolak (If-None-Match), objek dikunci
// COMPLIANCE sampai now+Retention.
func (s *S3) Put(ctx context.Context, key string, data []byte) error {
	until := time.Now().Add(s.cfg.Retention).UTC()
	opts := minio.PutObjectOptions{Mode: minio.Compliance, RetainUntilDate: until, ContentType: "application/octet-stream"}
	opts.SetMatchETagExcept("*")
	_, err := s.c.PutObject(ctx, s.cfg.Bucket, key, bytes.NewReader(data), int64(len(data)), opts)
	if err != nil {
		if minio.ToErrorResponse(err).Code == "PreconditionFailed" {
			return fmt.Errorf("objstore.Put %s: %w", key, ErrExists)
		}
		return fmt.Errorf("objstore.Put: %w", err)
	}
	return nil
}

// Get lihat Store.
func (s *S3) Get(ctx context.Context, key string) ([]byte, error) {
	o, err := s.c.GetObject(ctx, s.cfg.Bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("objstore.Get: %w", err)
	}
	defer func() { _ = o.Close() }()
	b, err := io.ReadAll(o)
	if err != nil {
		return nil, fmt.Errorf("objstore.Get: %w", err)
	}
	return b, nil
}

// List lihat Store.
func (s *S3) List(ctx context.Context, prefix string) ([]string, error) {
	var out []string
	for o := range s.c.ListObjects(ctx, s.cfg.Bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if o.Err != nil {
			return nil, fmt.Errorf("objstore.List: %w", o.Err)
		}
		out = append(out, o.Key)
	}
	sort.Strings(out)
	return out, nil
}

func (s *S3) String() string { return "s3://" + s.cfg.Bucket + " @ " + s.cfg.Endpoint }

// FilePutter Store yang menyimpan file secara streaming dengan retensi per
// objek (backup bertingkat: retensi berbeda per tingkat). retention 0 =
// retensi default Store.
type FilePutter interface {
	Store
	PutFile(ctx context.Context, key, path string, retention time.Duration) error
}

// PutFile lihat FilePutter (Dir tidak punya retensi).
func (d Dir) PutFile(ctx context.Context, key, path string, _ time.Duration) error {
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return fmt.Errorf("objstore.PutFile: %w", err)
	}
	return d.Put(ctx, key, b) // ponytail: Dir hanya dev/test, file dimuat utuh
}

// PutFile lihat FilePutter: stream dari file, dikunci COMPLIANCE sampai
// now+retention.
func (s *S3) PutFile(ctx context.Context, key, path string, retention time.Duration) error {
	f, err := os.Open(filepath.Clean(path))
	if err != nil {
		return fmt.Errorf("objstore.PutFile: %w", err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("objstore.PutFile: %w", err)
	}
	if retention <= 0 {
		retention = s.cfg.Retention
	}
	opts := minio.PutObjectOptions{Mode: minio.Compliance, RetainUntilDate: time.Now().Add(retention).UTC(), ContentType: "application/octet-stream"}
	opts.SetMatchETagExcept("*")
	if _, err := s.c.PutObject(ctx, s.cfg.Bucket, key, f, st.Size(), opts); err != nil {
		if minio.ToErrorResponse(err).Code == "PreconditionFailed" {
			return fmt.Errorf("objstore.PutFile %s: %w", key, ErrExists)
		}
		return fmt.Errorf("objstore.PutFile: %w", err)
	}
	return nil
}
