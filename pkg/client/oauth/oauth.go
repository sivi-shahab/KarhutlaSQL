// Package oauth login SSO di sisi klien (spec E-10): OAuth2 Device Code
// (default, cocok untuk SSH/headless) atau Authorization Code + PKCE lewat
// browser dengan redirect loopback; token disimpan di file 0600 dan
// di-refresh otomatis.
package oauth

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// RefreshBefore token di-refresh bila sisa umurnya kurang dari ini (§9.1).
const RefreshBefore = 60 * time.Second

// Config klien OIDC (public client, tanpa secret).
type Config struct {
	Issuer, ClientID string
	Scopes           []string // default: openid profile email
}

func (c Config) oauth2(ctx context.Context, redirect string) (*oauth2.Config, error) {
	p, err := gooidc.NewProvider(ctx, c.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oauth: discovery: %w", err)
	}
	scopes := c.Scopes
	if len(scopes) == 0 {
		scopes = []string{gooidc.ScopeOpenID, "profile", "email"}
	}
	return &oauth2.Config{ClientID: c.ClientID, Endpoint: p.Endpoint(), Scopes: scopes, RedirectURL: redirect}, nil
}

// DeviceLogin menjalankan Device Code flow; instruksi untuk user ditulis ke w.
func DeviceLogin(ctx context.Context, c Config, w io.Writer) (*oauth2.Token, error) {
	conf, err := c.oauth2(ctx, "")
	if err != nil {
		return nil, err
	}
	if conf.Endpoint.DeviceAuthURL == "" {
		return nil, errors.New("oauth: issuer does not support device authorization")
	}
	// PKCE juga di device flow (RFC 8628 + 7636): diwajibkan Keycloak bila
	// client mengatur code_challenge_method; IdP lain mengabaikannya.
	verifier := oauth2.GenerateVerifier()
	da, err := conf.DeviceAuth(ctx, oauth2.S256ChallengeOption(verifier))
	if err != nil {
		return nil, fmt.Errorf("oauth: device auth: %w", err)
	}
	uri := da.VerificationURIComplete
	if uri == "" {
		uri = da.VerificationURI
	}
	_, _ = fmt.Fprintf(w, "Buka %s\nlalu masukkan kode: %s\n", uri, da.UserCode)
	tok, err := conf.DeviceAccessToken(ctx, da, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, fmt.Errorf("oauth: device token: %w", err)
	}
	return tok, nil
}

// BrowserLogin menjalankan Authorization Code + PKCE dengan redirect ke
// http://127.0.0.1:<port>/callback. URL login ditulis ke w dan dicoba dibuka
// di browser (gagal membuka bukan error).
func BrowserLogin(ctx context.Context, c Config, w io.Writer) (*oauth2.Token, error) {
	lis, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("oauth: %w", err)
	}
	defer func() { _ = lis.Close() }()
	conf, err := c.oauth2(ctx, fmt.Sprintf("http://%s/callback", lis.Addr()))
	if err != nil {
		return nil, err
	}
	verifier, state := oauth2.GenerateVerifier(), randomHex()
	type result struct {
		code string
		err  error
	}
	ch := make(chan result, 1)
	var once sync.Once
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		res := result{code: q.Get("code")}
		switch {
		case r.URL.Path != "/callback":
			http.NotFound(rw, r)
			return
		case q.Get("state") != state:
			res.err = errors.New("oauth: state mismatch")
		case q.Get("error") != "":
			res.err = fmt.Errorf("oauth: %s: %s", q.Get("error"), q.Get("error_description"))
		}
		if res.err != nil {
			http.Error(rw, "Login gagal; kembali ke terminal.", http.StatusBadRequest)
		} else {
			_, _ = io.WriteString(rw, "Login berhasil; kembali ke terminal.\n")
		}
		once.Do(func() { ch <- res })
	})}
	go func() { _ = srv.Serve(lis) }()
	defer func() { _ = srv.Close() }()

	url := conf.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier))
	_, _ = fmt.Fprintf(w, "Buka di browser untuk login:\n%s\n", url)
	openBrowser(ctx, url)
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case res := <-ch:
		if res.err != nil {
			return nil, res.err
		}
		tok, err := conf.Exchange(ctx, res.code, oauth2.VerifierOption(verifier))
		if err != nil {
			return nil, fmt.Errorf("oauth: exchange: %w", err)
		}
		return tok, nil
	}
}

func randomHex() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read tidak pernah gagal (Go ≥ 1.24)
	return hex.EncodeToString(b)
}

func openBrowser(ctx context.Context, url string) {
	cmd := "xdg-open"
	if runtime.GOOS == "darwin" {
		cmd = "open"
	}
	_ = exec.CommandContext(ctx, cmd, url).Start() //nolint:gosec // URL dari discovery issuer yang dikonfigurasi user
}

// FileStore menyimpan token per issuer+client di file 0600.
// ponytail: file saja; keyring OS bila ada kebutuhan desktop.
type FileStore struct{ Path string }

type stored struct {
	Issuer   string        `json:"issuer"`
	ClientID string        `json:"client_id"`
	Token    *oauth2.Token `json:"token"`
}

// Load token tersimpan untuk c; error bila tidak ada atau milik issuer lain.
func (s FileStore) Load(c Config) (*oauth2.Token, error) {
	b, err := os.ReadFile(filepath.Clean(s.Path))
	if err != nil {
		return nil, fmt.Errorf("oauth: no stored token (%w); run with --oidc-login", err)
	}
	var st stored
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("oauth: %s: %w", s.Path, err)
	}
	if st.Issuer != c.Issuer || st.ClientID != c.ClientID || st.Token == nil {
		return nil, errors.New("oauth: stored token belongs to another issuer/client; run with --oidc-login")
	}
	return st.Token, nil
}

// Save menulis token (direktori 0700, file 0600).
func (s FileStore) Save(c Config, tok *oauth2.Token) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return fmt.Errorf("oauth: %w", err)
	}
	b, err := json.Marshal(stored{Issuer: c.Issuer, ClientID: c.ClientID, Token: tok})
	if err != nil {
		return fmt.Errorf("oauth: %w", err)
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return fmt.Errorf("oauth: %w", err)
	}
	return os.Rename(tmp, s.Path)
}

// TokenSource me-refresh tok RefreshBefore sebelum kedaluwarsa dan menyimpan
// token baru ke store.
func TokenSource(ctx context.Context, c Config, tok *oauth2.Token, store FileStore) (oauth2.TokenSource, error) {
	conf, err := c.oauth2(ctx, "")
	if err != nil {
		return nil, err
	}
	src := oauth2.ReuseTokenSourceWithExpiry(tok, conf.TokenSource(ctx, tok), RefreshBefore)
	return &savingSource{src: src, last: tok.AccessToken, save: func(t *oauth2.Token) error { return store.Save(c, t) }}, nil
}

type savingSource struct {
	src  oauth2.TokenSource
	save func(*oauth2.Token) error

	mu   sync.Mutex
	last string
}

func (s *savingSource) Token() (*oauth2.Token, error) {
	t, err := s.src.Token()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.AccessToken != s.last {
		s.last = t.AccessToken
		if err := s.save(t); err != nil {
			return nil, err
		}
	}
	return t, nil
}
