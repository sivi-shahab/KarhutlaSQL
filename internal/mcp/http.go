package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

// Verifier memverifikasi bearer token pemanggil (lokal/OIDC, allowlist,
// revocation): user stabil untuk pengikatan sesi MCP dan waktu kedaluwarsa.
type Verifier func(ctx context.Context, token string) (user string, exp time.Time, err error)

const prmPath = "/.well-known/oauth-protected-resource"

// Handler endpoint Streamable HTTP "/mcp" (OAuth 2.1 resource server: Bearer
// wajib, 401 + WWW-Authenticate) dan metadata RFC 9728 bila resourceURL diset
// (authServers = issuer OIDC). Token diteruskan ke Flight SQL per tool call.
func Handler(s *sdk.Server, verify Verifier, resourceURL string, authServers []string) (http.Handler, error) {
	mux := http.NewServeMux()
	opts := &auth.RequireBearerTokenOptions{ClockSkew: time.Minute}
	if resourceURL != "" {
		u, err := url.Parse(resourceURL)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("mcp.Handler: resource url %q: want absolute URL", resourceURL)
		}
		mux.Handle("GET "+prmPath, auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
			Resource: resourceURL, AuthorizationServers: authServers, BearerMethodsSupported: []string{"header"}}))
		opts.ResourceMetadataURL = u.Scheme + "://" + u.Host + prmPath
	}
	tv := func(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
		user, exp, err := verify(ctx, token)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", auth.ErrInvalidToken, err)
		}
		return &auth.TokenInfo{UserID: user, Expiration: exp, Extra: map[string]any{TokenKey: token}}, nil
	}
	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return s }, &sdk.StreamableHTTPOptions{SessionTimeout: 30 * time.Minute})
	mux.Handle("/mcp", auth.RequireBearerToken(tv, opts)(h))
	return mux, nil
}
