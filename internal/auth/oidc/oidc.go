// Package oidc memverifikasi access token OIDC (JWT) via discovery + JWKS
// (go-oidc: kunci di-cache, diambil ulang saat kid tak dikenal) dan memetakan
// claim ke Identity.
package oidc

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"

	"github.com/bankmega/karhutlasql/internal/auth"
)

// Config konfigurasi verifier.
type Config struct {
	Issuer        string
	Audience      string // wajib ada di claim aud
	UsernameClaim string // default preferred_username (fallback sub)
	GroupsClaim   string // default groups
	// ClockSkew toleransi exp (spec §7: 60s).
	ClockSkew time.Duration
}

// Verifier verifier token OIDC satu issuer.
type Verifier struct {
	cfg Config
	v   *gooidc.IDTokenVerifier
}

// New melakukan discovery issuer. ctx dipakai juga untuk mengambil JWKS
// selama verifier hidup (harus ctx server, bukan ctx request).
func New(ctx context.Context, cfg Config) (*Verifier, error) {
	if cfg.Issuer == "" || cfg.Audience == "" {
		return nil, errors.New("oidc.New: issuer and audience are required")
	}
	if cfg.UsernameClaim == "" {
		cfg.UsernameClaim = "preferred_username"
	}
	if cfg.GroupsClaim == "" {
		cfg.GroupsClaim = "groups"
	}
	p, err := gooidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc.New: discovery: %w", err)
	}
	skew := cfg.ClockSkew
	return &Verifier{cfg: cfg, v: p.Verifier(&gooidc.Config{
		ClientID: cfg.Audience,
		Now:      func() time.Time { return time.Now().Add(-skew) },
	})}, nil
}

// Issuer issuer yang dilayani verifier.
func (v *Verifier) Issuer() string { return v.cfg.Issuer }

// Verify memverifikasi signature, iss, aud, exp lalu memetakan claim.
// Email hanya dipakai bila email_verified (allowlist/policy berbasis email).
// ID sesi = sid (stabil antar refresh token sehingga kill tidak bisa
// dihindari dengan refresh), fallback jti, fallback hash token.
func (v *Verifier) Verify(ctx context.Context, raw string) (auth.Identity, error) {
	tok, err := v.v.Verify(ctx, raw)
	if err != nil {
		return auth.Identity{}, fmt.Errorf("oidc.Verify: %w", err)
	}
	var c map[string]any
	if err := tok.Claims(&c); err != nil {
		return auth.Identity{}, fmt.Errorf("oidc.Verify: %w", err)
	}
	str := func(k string) string { s, _ := c[k].(string); return s }
	id := auth.Identity{Subject: str(v.cfg.UsernameClaim), Source: auth.SourceOIDC, ExpiresAt: tok.Expiry}
	if id.Subject == "" {
		id.Subject = tok.Subject
	}
	if verified, _ := c["email_verified"].(bool); verified {
		id.Email = str("email")
	}
	if gs, ok := c[v.cfg.GroupsClaim].([]any); ok {
		for _, g := range gs {
			if s, ok := g.(string); ok {
				id.Groups = append(id.Groups, s)
			}
		}
	}
	switch {
	case str("sid") != "":
		id.TokenID = "oidc:" + str("sid")
	case str("jti") != "":
		id.TokenID = "oidc:" + str("jti")
	default:
		sum := sha256.Sum256([]byte(raw))
		id.TokenID = "oidc:" + hex.EncodeToString(sum[:16])
	}
	return id, nil
}
