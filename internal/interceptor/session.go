package interceptor

import (
	"context"
	"errors"
	"strings"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bankmega/karhutlasql/internal/audit"
	"github.com/bankmega/karhutlasql/internal/auth"
	"github.com/bankmega/karhutlasql/internal/metrics"
	"github.com/bankmega/karhutlasql/internal/session"
)

// Session mengaitkan sesi registry ke ctx; dipasang setelah Auth. Token sesi
// yang sudah di-kill ditolak UNAUTHENTICATED.
type Session struct {
	reg     *session.Registry
	audit   *audit.Logger
	metrics *metrics.Metrics
}

// NewSession membuat interceptor sesi. m boleh nil.
func NewSession(reg *session.Registry, a *audit.Logger, m *metrics.Metrics) *Session {
	return &Session{reg: reg, audit: a, metrics: m}
}

// Unary interceptor unary.
func (s *Session) Unary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if strings.HasPrefix(info.FullMethod, healthPrefix) {
			return handler(ctx, req)
		}
		ctx, err := s.attach(ctx)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// Stream interceptor stream.
func (s *Session) Stream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if strings.HasPrefix(info.FullMethod, healthPrefix) {
			return handler(srv, ss)
		}
		ctx, err := s.attach(ss.Context())
		if err != nil {
			return err
		}
		return handler(srv, &ctxStream{ServerStream: ss, ctx: ctx})
	}
}

func (s *Session) attach(ctx context.Context) (context.Context, error) {
	id, ok := auth.FromContext(ctx)
	if !ok {
		return ctx, status.Error(codes.Unauthenticated, "must authenticate first")
	}
	sess, err := s.reg.Attach(id, peerAddr(ctx))
	if err != nil {
		if errors.Is(err, session.ErrRevoked) {
			s.audit.Emit(ctx, audit.Event{Type: audit.LoginFail, User: id.Subject, Source: id.Source, Peer: peerAddr(ctx), Error: err.Error()})
			s.metrics.AuthFailure("session_revoked")
			return ctx, status.Error(codes.Unauthenticated, "session killed")
		}
		return ctx, status.Errorf(codes.Unauthenticated, "%v", err)
	}
	return session.WithSession(ctx, sess), nil
}
