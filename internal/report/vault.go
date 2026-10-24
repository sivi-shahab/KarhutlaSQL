package report

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// VaultConfig kunci Ed25519 di HashiCorp Vault Transit (atau Transit yang
// di-backing HSM lewat managed keys). Token tidak pernah lewat argv: env
// VAULT_TOKEN atau file (mis. sink Vault Agent, dibaca ulang setiap
// tanda tangan agar token yang diperbarui terpakai).
type VaultConfig struct {
	Addr      string // VAULT_ADDR
	Key       string // "<mount>/<nama key>", mis. "transit/report-signing"
	Token     string // nilai VAULT_TOKEN (opsional bila TokenFile)
	TokenFile string
	Namespace string // VAULT_NAMESPACE (opsional)
	CAFile    string // VAULT_CACERT (opsional)
}

// NewVaultSigner Signer yang menandatangani lewat Vault Transit
// (POST /v1/<mount>/sign/<key>); versi key terbaru disematkan saat start
// sehingga key_id manifest konsisten, lalu satu tanda tangan uji diverifikasi.
func NewVaultSigner(ctx context.Context, c VaultConfig) (*Signer, error) {
	mount, key, ok := strings.Cut(c.Key, "/")
	if c.Addr == "" || !ok || mount == "" || key == "" {
		return nil, errors.New("report.NewVaultSigner: need VAULT_ADDR and key <mount>/<name>")
	}
	hc := &http.Client{Timeout: 10 * time.Second}
	if c.CAFile != "" {
		pem, err := os.ReadFile(filepath.Clean(c.CAFile))
		if err != nil {
			return nil, fmt.Errorf("report.NewVaultSigner: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("report.NewVaultSigner: %s: no PEM certificates", c.CAFile)
		}
		hc.Transport = &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}
	}
	v := &vault{c: c, hc: hc, base: strings.TrimSuffix(c.Addr, "/") + "/v1/" + mount}
	var meta struct {
		Data struct {
			Type          string `json:"type"`
			LatestVersion int    `json:"latest_version"`
			Keys          map[string]struct {
				PublicKey string `json:"public_key"`
			} `json:"keys"`
		} `json:"data"`
	}
	if err := v.do(ctx, http.MethodGet, "/keys/"+key, nil, &meta); err != nil {
		return nil, err
	}
	ver := strconv.Itoa(meta.Data.LatestVersion)
	pub, err := base64.StdEncoding.DecodeString(meta.Data.Keys[ver].PublicKey)
	if meta.Data.Type != "ed25519" || err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("report.NewVaultSigner: key %s must be type ed25519 (got %q)", c.Key, meta.Data.Type)
	}
	sign := func(ctx context.Context, msg []byte) ([]byte, error) {
		var out struct {
			Data struct {
				Signature string `json:"signature"`
			} `json:"data"`
		}
		body := map[string]any{"input": base64.StdEncoding.EncodeToString(msg), "key_version": meta.Data.LatestVersion}
		if err := v.do(ctx, http.MethodPost, "/sign/"+key, body, &out); err != nil {
			return nil, err
		}
		parts := strings.Split(out.Data.Signature, ":") // vault:v<N>:<base64>
		raw, err := base64.StdEncoding.DecodeString(parts[len(parts)-1])
		if err != nil || len(raw) != ed25519.SignatureSize {
			return nil, errors.New("vault: unexpected signature format")
		}
		return raw, nil
	}
	probe := []byte("karhutlasql signer self-test")
	sig, err := sign(ctx, probe)
	if err != nil {
		return nil, fmt.Errorf("report.NewVaultSigner: %w", err)
	}
	if !ed25519.Verify(pub, probe, sig) {
		return nil, errors.New("report.NewVaultSigner: self-test signature does not verify with the key's public key")
	}
	return &Signer{KeyID: KeyID(ed25519.PublicKey(pub)), sign: sign}, nil
}

type vault struct {
	c    VaultConfig
	hc   *http.Client
	base string
}

func (v *vault) token() (string, error) {
	if v.c.TokenFile != "" {
		b, err := os.ReadFile(filepath.Clean(v.c.TokenFile))
		if err != nil {
			return "", err
		}
		return strings.TrimSpace(string(b)), nil
	}
	if v.c.Token == "" {
		return "", errors.New("no Vault token (VAULT_TOKEN or token file)")
	}
	return v.c.Token, nil
}

func (v *vault) do(ctx context.Context, method, path string, body, out any) error {
	tok, err := v.token()
	if err != nil {
		return fmt.Errorf("vault: %w", err)
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body) // map string/int: tidak pernah error
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, v.base+path, rd)
	if err != nil {
		return fmt.Errorf("vault: %w", err)
	}
	req.Header.Set("X-Vault-Token", tok)
	if v.c.Namespace != "" {
		req.Header.Set("X-Vault-Namespace", v.c.Namespace)
	}
	resp, err := v.hc.Do(req)
	if err != nil {
		return fmt.Errorf("vault: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("vault: %s %s: HTTP %d", method, path, resp.StatusCode) // body Vault bisa memuat detail; tidak diteruskan
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("vault: %w", err)
	}
	return nil
}
