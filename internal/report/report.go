package report

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/compress"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	"go.yaml.in/yaml/v3"
)

// Def definisi satu report (reports.yaml).
type Def struct {
	// Roles role yang boleh menjalankan; SQL dieksekusi sebagai pemanggil
	// (authz, masking, RLS berlaku).
	Roles []string `yaml:"roles"`
	SQL   string   `yaml:"sql"`
	// Asserts DQ gate: setiap SQL harus menghasilkan tepat satu nilai true
	// pada snapshot yang sama dengan report.
	Asserts []Assert `yaml:"asserts"`
}

// Assert satu cek DQ gate.
type Assert struct {
	Name string `yaml:"name"`
	SQL  string `yaml:"sql"`
}

var namePattern = regexp.MustCompile(`^[a-z0-9_]+$`)

// Load membaca reports.yaml ({reports: {nama: Def}}); path kosong = tanpa report.
func Load(path string) (map[string]Def, error) {
	if path == "" {
		return map[string]Def{}, nil
	}
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("report.Load: %w", err)
	}
	var f struct {
		Reports map[string]Def `yaml:"reports"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("report.Load: %w", err)
	}
	var errs []error
	for name, d := range f.Reports {
		if !namePattern.MatchString(name) || strings.TrimSpace(d.SQL) == "" || len(d.Roles) == 0 {
			errs = append(errs, fmt.Errorf("report %q: name [a-z0-9_], sql and roles are required", name))
		}
		for _, a := range d.Asserts {
			if a.Name == "" || strings.TrimSpace(a.SQL) == "" {
				errs = append(errs, fmt.Errorf("report %q: assert needs name and sql", name))
			}
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("report.Load: %w", err)
	}
	if f.Reports == nil {
		f.Reports = map[string]Def{}
	}
	return f.Reports, nil
}

// Manifest bukti report run (arsitektur §7): ditandatangani Ed25519.
type Manifest struct {
	ReportID   string    `json:"report_id"`
	Name       string    `json:"name"`
	User       string    `json:"user"`
	Source     string    `json:"source"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	// AsOf argumen time travel (versi snapshot DuckLake atau waktu RFC 3339);
	// kosong = data terkini.
	AsOf string `json:"as_of,omitempty"`
	// SnapshotID sidik data sumber pada transaksi report (lihat Tables).
	SnapshotID string `json:"snapshot_id"`
	// LakeSnapshots ID snapshot DuckLake yang dibaca, per catalog sumber.
	LakeSnapshots map[string]int64 `json:"lake_snapshots,omitempty"`
	// Tables sidik isi tiap tabel sumber: "rows:<n>,hash:<x>" (DuckDB versi Engine).
	Tables      map[string]string `json:"tables"`
	Engine      string            `json:"engine"`
	SQLSHA256   string            `json:"sql_sha256"`          // SQL definisi report
	ExecSHA256  string            `json:"executed_sql_sha256"` // setelah rewrite governance
	Asserts     []AssertResult    `json:"asserts"`
	DQValidates []string          `json:"dq_validation_ids"` // Q-05 (Fase 6c)
	Result      Result            `json:"result"`
	KeyID       string            `json:"key_id"`
}

// AssertResult hasil satu assert.
type AssertResult struct {
	Name      string `json:"name"`
	SQLSHA256 string `json:"sql_sha256"`
	Passed    bool   `json:"passed"`
}

// Result file hasil report.
type Result struct {
	File   string `json:"file"` // nama file di sebelah manifest
	Format string `json:"format"`
	SHA256 string `json:"sha256"`
	Rows   int64  `json:"rows"`
	Bytes  int64  `json:"bytes"`
}

// SHA256 hex sha256.
func SHA256(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// WriteParquet menulis seluruh reader sebagai Parquet (zstd).
// ponytail: hasil di memori; tulis bertahap ke objek multipart bila report
// melebihi memori server.
func WriteParquet(rdr array.RecordReader) ([]byte, int64, error) {
	var out bytes.Buffer
	w, err := pqarrow.NewFileWriter(rdr.Schema(), &out,
		parquet.NewWriterProperties(parquet.WithCompression(compress.Codecs.Zstd)), pqarrow.DefaultWriterProps())
	if err != nil {
		return nil, 0, fmt.Errorf("report.WriteParquet: %w", err)
	}
	var rows int64
	for rdr.Next() {
		rec := rdr.RecordBatch()
		if err := w.Write(rec); err != nil {
			return nil, 0, errors.Join(fmt.Errorf("report.WriteParquet: %w", err), w.Close())
		}
		rows += rec.NumRows()
	}
	if err := errors.Join(rdr.Err(), w.Close()); err != nil {
		return nil, 0, fmt.Errorf("report.WriteParquet: %w", err)
	}
	return out.Bytes(), rows, nil
}

// Signer kunci tanda tangan manifest.
type Signer struct {
	priv  ed25519.PrivateKey
	KeyID string
}

// LoadSigner membaca kunci privat Ed25519 PKCS#8 PEM.
func LoadSigner(path string) (*Signer, error) {
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("report.LoadSigner: %w", err)
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("report.LoadSigner: no PEM block")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("report.LoadSigner: %w", err)
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("report.LoadSigner: key is %T, want Ed25519", k)
	}
	return &Signer{priv: priv, KeyID: KeyID(priv.Public().(ed25519.PublicKey))}, nil //nolint:forcetypeassert // Public() Ed25519
}

// Sign menyerialisasi m (dengan KeyID) dan menandatanganinya; mengembalikan
// isi manifest.json dan manifest.json.sig (base64).
func (s *Signer) Sign(m Manifest) ([]byte, []byte, error) {
	m.KeyID = s.KeyID
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("report.Sign: %w", err)
	}
	sig := base64.StdEncoding.EncodeToString(ed25519.Sign(s.priv, b))
	return b, []byte(sig + "\n"), nil
}

// KeyID sidik kunci publik: 16 hex pertama sha256(DER PKIX).
func KeyID(pub ed25519.PublicKey) string {
	der, _ := x509.MarshalPKIXPublicKey(pub) // Ed25519 selalu bisa di-marshal
	return SHA256(der)[:16]
}

// ParsePublicKey kunci publik Ed25519 PEM (PKIX, "PUBLIC KEY").
func ParsePublicKey(b []byte) (ed25519.PublicKey, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("report.ParsePublicKey: no PEM block")
	}
	k, err := x509.ParsePKIXPublicKey(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("report.ParsePublicKey: %w", err)
	}
	pub, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("report.ParsePublicKey: key is %T, want Ed25519", k)
	}
	return pub, nil
}

// Verify memeriksa tanda tangan manifest dan hash file hasil. result = isi
// file hasil.
func Verify(manifest, sig, result []byte, pub ed25519.PublicKey) (Manifest, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sig)))
	if err != nil {
		return Manifest{}, fmt.Errorf("report.Verify: signature encoding: %w", err)
	}
	if !ed25519.Verify(pub, manifest, raw) {
		return Manifest{}, errors.New("report.Verify: invalid signature")
	}
	var m Manifest
	if err := json.Unmarshal(manifest, &m); err != nil {
		return Manifest{}, fmt.Errorf("report.Verify: %w", err)
	}
	if m.KeyID != KeyID(pub) {
		return Manifest{}, fmt.Errorf("report.Verify: manifest key_id %s does not match public key %s", m.KeyID, KeyID(pub))
	}
	if got := SHA256(result); got != m.Result.SHA256 {
		return Manifest{}, fmt.Errorf("report.Verify: result sha256 %s, manifest says %s", got, m.Result.SHA256)
	}
	return m, nil
}
