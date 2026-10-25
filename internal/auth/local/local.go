package local

import (
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"

	"github.com/bankmega/karhutlasql/internal/auth"
)

// Issuer & audience token lokal; OIDC dibedakan dari issuer (arsitektur §4.1).
const (
	Issuer   = "karhutlasql"
	Audience = "karhutlasql"
)

// MaxTTL batas umur token (spec §6: ≤ 24h).
const MaxTTL = 24 * time.Hour

// MinHMACSecret panjang minimum secret HS256 (RFC 7518 §3.2).
const MinHMACSecret = 32

// ErrInvalidCredentials sengaja generik: tidak membedakan user tak dikenal vs password salah.
var ErrInvalidCredentials = errors.New("invalid credentials")

// Config konfigurasi Authenticator. Tepat satu dari HMACSecret / RSAKey diisi.
type Config struct {
	Username string
	// Password plaintext atau hash bcrypt ($2a$/$2b$/$2y$).
	Password   string
	HMACSecret []byte
	RSAKey     *rsa.PrivateKey
	TTL        time.Duration
	Now        func() time.Time // test; default time.Now
}

// Authenticator memverifikasi password dan menerbitkan/memverifikasi JWT lokal.
type Authenticator struct {
	username []byte
	hash     []byte
	method   jwt.SigningMethod
	signKey  any
	verKey   any
	kid      string
	ttl      time.Duration
	now      func() time.Time
}

// dummyHash dipakai saat username salah agar waktu respons setara (anti user enumeration).
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("karhutlasql-dummy"), bcrypt.DefaultCost)

// New memvalidasi cfg dan membuat Authenticator.
func New(cfg Config) (*Authenticator, error) {
	if cfg.Username == "" || cfg.Password == "" {
		return nil, errors.New("local.New: username and password are required")
	}
	if strings.HasPrefix(cfg.Username, JobPrefix) || strings.HasPrefix(cfg.Username, SharePrefix) {
		return nil, fmt.Errorf("local.New: username must not start with %q or %q (reserved for internal identities)", JobPrefix, SharePrefix)
	}
	if cfg.TTL <= 0 || cfg.TTL > MaxTTL {
		return nil, fmt.Errorf("local.New: ttl %s must be in (0, %s]", cfg.TTL, MaxTTL)
	}
	hash := []byte(cfg.Password)
	if _, err := bcrypt.Cost(hash); err != nil {
		if hash, err = bcrypt.GenerateFromPassword([]byte(cfg.Password), bcrypt.DefaultCost); err != nil {
			return nil, fmt.Errorf("local.New: %w", err)
		}
	}
	a := &Authenticator{username: []byte(cfg.Username), hash: hash, ttl: cfg.TTL, now: cfg.Now}
	if a.now == nil {
		a.now = time.Now
	}
	switch {
	case cfg.RSAKey != nil && cfg.HMACSecret != nil:
		return nil, errors.New("local.New: set either HMAC secret or RSA key, not both")
	case cfg.RSAKey != nil:
		a.method, a.signKey, a.verKey = jwt.SigningMethodRS256, cfg.RSAKey, cfg.RSAKey.Public()
		a.kid = keyID(cfg.RSAKey.Public())
	case len(cfg.HMACSecret) >= MinHMACSecret:
		a.method, a.signKey, a.verKey = jwt.SigningMethodHS256, cfg.HMACSecret, cfg.HMACSecret
		a.kid = keyID(cfg.HMACSecret)
	default:
		return nil, fmt.Errorf("local.New: HMAC secret must be at least %d bytes", MinHMACSecret)
	}
	return a, nil
}

// keyID kid stabil dari kunci (bukan kunci itu sendiri); dasar rotasi kunci nanti.
func keyID(k any) string {
	var b []byte
	switch k := k.(type) {
	case []byte:
		b = k
	case crypto.PublicKey:
		if pk, ok := k.(*rsa.PublicKey); ok {
			b = pk.N.Bytes()
		}
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:4])
}

// Login memverifikasi kredensial dan menerbitkan token.
func (a *Authenticator) Login(username, password string) (string, auth.Identity, error) {
	userOK := subtle.ConstantTimeCompare([]byte(username), a.username) == 1
	hash := a.hash
	if !userOK {
		hash = dummyHash
	}
	if err := bcrypt.CompareHashAndPassword(hash, []byte(password)); err != nil || !userOK {
		return "", auth.Identity{}, ErrInvalidCredentials
	}
	return a.issue(username)
}

// JobPrefix awalan subject identitas job terjadwal (internal/jobs); user lokal
// tidak boleh memakainya.
const JobPrefix = "job:"

// SharePrefix awalan subject identitas penerima Delta Sharing (internal/sharing).
const SharePrefix = "share:"

// IssueShare menerbitkan token lokal untuk penerima Delta Sharing
// "share:<recipient>" — hanya dipanggil server sharing di dalam proses
// setelah bearer token penerima diverifikasi; izin dari subjects policy.
func (a *Authenticator) IssueShare(recipient string) (string, auth.Identity, error) {
	if recipient == "" {
		return "", auth.Identity{}, errors.New("local.IssueShare: empty recipient")
	}
	return a.issue(SharePrefix + recipient)
}

// IssueJob menerbitkan token lokal untuk identitas job terjadwal
// "job:<name>" tanpa password — hanya dipanggil runner job di dalam proses
// server; izinnya tetap dari subjects policy ({ user: "job:<name>" }).
func (a *Authenticator) IssueJob(name string) (string, auth.Identity, error) {
	if name == "" {
		return "", auth.Identity{}, errors.New("local.IssueJob: empty job name")
	}
	return a.issue(JobPrefix + name)
}

func (a *Authenticator) issue(subject string) (string, auth.Identity, error) {
	now := a.now()
	id := auth.Identity{Subject: subject, Source: auth.SourceLocal, TokenID: uuid.NewString(), ExpiresAt: now.Add(a.ttl)}
	tok := jwt.NewWithClaims(a.method, jwt.RegisteredClaims{
		Issuer:    Issuer,
		Subject:   id.Subject,
		Audience:  jwt.ClaimStrings{Audience},
		IssuedAt:  jwt.NewNumericDate(now),
		NotBefore: jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(id.ExpiresAt),
		ID:        id.TokenID,
	})
	tok.Header["kid"] = a.kid
	s, err := tok.SignedString(a.signKey)
	if err != nil {
		return "", auth.Identity{}, fmt.Errorf("local.issue: %w", err)
	}
	return s, id, nil
}

// Verify memverifikasi token lokal (alg, kid, iss, aud, exp, nbf).
func (a *Authenticator) Verify(token string) (auth.Identity, error) {
	var claims jwt.RegisteredClaims
	_, err := jwt.ParseWithClaims(strings.TrimSpace(token), &claims, func(t *jwt.Token) (any, error) {
		if kid, _ := t.Header["kid"].(string); kid != a.kid {
			return nil, errors.New("unknown kid")
		}
		return a.verKey, nil
	},
		jwt.WithValidMethods([]string{a.method.Alg()}),
		jwt.WithIssuer(Issuer),
		jwt.WithAudience(Audience),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		// Toleransi jam mundur (mis. sinkronisasi NTP/WSL) seperti ClockSkew OIDC:
		// tanpa ini token yang baru diterbitkan bisa ditolak "iat di masa depan".
		jwt.WithLeeway(time.Minute),
		jwt.WithTimeFunc(a.now),
	)
	if err != nil {
		return auth.Identity{}, fmt.Errorf("local.Verify: %w", err)
	}
	return auth.Identity{Subject: claims.Subject, Source: auth.SourceLocal, TokenID: claims.ID, ExpiresAt: claims.ExpiresAt.Time}, nil
}
