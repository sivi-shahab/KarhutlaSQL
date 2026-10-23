package interceptor

import (
	"context"
	"encoding/base64"
	"log/slog"
	"strings"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/bankmega/karhutlasql/internal/audit"
	"github.com/bankmega/karhutlasql/internal/auth"
	"github.com/bankmega/karhutlasql/internal/auth/local"
	"github.com/bankmega/karhutlasql/internal/metrics"
)

const (
	authHeader      = "authorization"
	handshakeMethod = "/arrow.flight.protocol.FlightService/Handshake"
	healthPrefix    = "/grpc.health.v1.Health/"
)

// Auth menegakkan autentikasi di semua RPC kecuali gRPC health:
// Handshake menerima Basic → menerbitkan Bearer (header & trailer, sesuai
// klien Flight Go/Java); RPC lain wajib Bearer yang valid.
type Auth struct {
	local   *local.Authenticator
	audit   *audit.Logger
	metrics *metrics.Metrics
	log     *slog.Logger
}

// NewAuth membuat interceptor auth. m boleh nil.
func NewAuth(l *local.Authenticator, a *audit.Logger, m *metrics.Metrics, log *slog.Logger) *Auth {
	return &Auth{local: l, audit: a, metrics: m, log: log}
}

// Unary interceptor unary.
func (a *Auth) Unary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx = withRequestID(ctx)
		if strings.HasPrefix(info.FullMethod, healthPrefix) {
			return handler(ctx, req)
		}
		ctx, err := a.bearer(ctx)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// Stream interceptor stream (termasuk Handshake).
func (a *Auth) Stream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx := withRequestID(ss.Context())
		var err error
		switch {
		case strings.HasPrefix(info.FullMethod, healthPrefix):
		case info.FullMethod == handshakeMethod:
			ctx, err = a.handshake(ctx, ss)
		default:
			ctx, err = a.bearer(ctx)
		}
		if err != nil {
			return err
		}
		return handler(srv, &ctxStream{ServerStream: ss, ctx: ctx})
	}
}

func withRequestID(ctx context.Context) context.Context {
	return audit.WithRequestID(ctx, uuid.NewString())
}

func credential(ctx context.Context) (scheme, cred string) {
	md, _ := metadata.FromIncomingContext(ctx)
	vals := md.Get(authHeader)
	if len(vals) == 0 {
		return "", ""
	}
	scheme, cred, _ = strings.Cut(strings.TrimSpace(vals[0]), " ")
	return scheme, strings.TrimLeft(cred, " ")
}

func peerAddr(ctx context.Context) string {
	if p, ok := peer.FromContext(ctx); ok && p.Addr != nil {
		return p.Addr.String()
	}
	return ""
}

var errUnauthenticated = status.Error(codes.Unauthenticated, "invalid credentials")

func (a *Auth) handshake(ctx context.Context, ss grpc.ServerStream) (context.Context, error) {
	scheme, cred := credential(ctx)
	if strings.EqualFold(scheme, "Bearer") { // klien memvalidasi token lama
		return a.bearer(ctx)
	}
	if !strings.EqualFold(scheme, "Basic") || cred == "" {
		a.fail(ctx, "", "missing basic credentials", "missing_credentials")
		return ctx, status.Error(codes.Unauthenticated, "handshake requires Basic credentials")
	}
	raw, err := base64.StdEncoding.DecodeString(cred)
	if err != nil {
		if raw, err = base64.RawStdEncoding.DecodeString(cred); err != nil {
			a.fail(ctx, "", "malformed basic credentials", "malformed_credentials")
			return ctx, errUnauthenticated
		}
	}
	user, pass, ok := strings.Cut(string(raw), ":")
	if !ok {
		a.fail(ctx, "", "malformed basic credentials", "malformed_credentials")
		return ctx, errUnauthenticated
	}
	token, id, err := a.local.Login(user, pass)
	if err != nil {
		a.fail(ctx, user, err.Error(), "invalid_credentials")
		return ctx, errUnauthenticated
	}
	md := metadata.Pairs(authHeader, "Bearer "+token)
	if err := ss.SetHeader(md); err != nil {
		return ctx, status.Errorf(codes.Internal, "interceptor.handshake: %v", err)
	}
	ss.SetTrailer(md)
	a.audit.Emit(ctx, audit.Event{Type: audit.LoginOK, User: id.Subject, Source: id.Source, Peer: peerAddr(ctx)})
	return auth.WithIdentity(ctx, id), nil
}

func (a *Auth) bearer(ctx context.Context) (context.Context, error) {
	scheme, cred := credential(ctx)
	if !strings.EqualFold(scheme, "Bearer") || cred == "" {
		return ctx, status.Error(codes.Unauthenticated, "must authenticate first")
	}
	id, err := a.local.Verify(cred)
	if err != nil {
		a.fail(ctx, "", "invalid bearer token", "invalid_token")
		a.log.Debug("bearer rejected", "err", err, "request_id", audit.RequestID(ctx))
		return ctx, status.Error(codes.Unauthenticated, "invalid or expired token")
	}
	return auth.WithIdentity(ctx, id), nil
}

// fail mencatat login_fail; label = alasan dari himpunan tetap (label metric).
func (a *Auth) fail(ctx context.Context, user, reason, label string) {
	a.audit.Emit(ctx, audit.Event{Type: audit.LoginFail, User: user, Peer: peerAddr(ctx), Error: reason})
	a.metrics.AuthFailure(label)
}

type ctxStream struct {
	grpc.ServerStream
	ctx context.Context
}

func (s *ctxStream) Context() context.Context { return s.ctx }
