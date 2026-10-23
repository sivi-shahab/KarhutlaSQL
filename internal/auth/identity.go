package auth

import (
	"context"
	"slices"
	"time"
)

// Sumber identitas.
const (
	SourceLocal = "local"
	SourceOIDC  = "oidc"
)

// Identity identitas terautentikasi untuk satu request.
type Identity struct {
	Subject string
	Email   string
	Groups  []string
	Roles   []string
	Source  string
	// TokenID (jti) = ID sesi; kunci revocation (kill session).
	TokenID   string
	ExpiresAt time.Time
	// Attrs claim string token OIDC (placeholder row filter {{claim.x}}).
	Attrs map[string]string
}

// RoleAdmin boleh melihat semua sesi dan kill session.
const RoleAdmin = "admin"

// HasRole melaporkan apakah id memiliki role.
func (id Identity) HasRole(role string) bool { return slices.Contains(id.Roles, role) }

type ctxKey struct{}

// WithIdentity menyimpan id di ctx.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext mengambil identitas dari ctx.
func FromContext(ctx context.Context) (Identity, bool) {
	id, ok := ctx.Value(ctxKey{}).(Identity)
	return id, ok
}
