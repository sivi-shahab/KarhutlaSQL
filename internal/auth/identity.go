package auth

import "context"

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
	// TokenID (jti) untuk revocation di Fase 4.
	TokenID string
}

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
