package interceptor

import (
	"context"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bankmega/karhutlasql/internal/auth"
	"github.com/bankmega/karhutlasql/internal/metrics"
)

// RateLimit membatasi request RPC per user dengan token bucket; dipasang
// setelah Auth. Satu query Flight SQL = 2 request (GetFlightInfo + DoGet).
// ponytail: satu bucket per user tanpa eviksi (dibatasi jumlah user yang
// lolos allowlist); sweep idle bila jumlah user sangat besar.
type RateLimit struct {
	rate, burst float64
	metrics     *metrics.Metrics
	now         func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// NewRateLimit perUser request/detik dengan burst; perUser ≤ 0 → nil (mati).
func NewRateLimit(perUser float64, burst int, m *metrics.Metrics) *RateLimit {
	if perUser <= 0 {
		return nil
	}
	return &RateLimit{rate: perUser, burst: float64(max(burst, 1)), metrics: m, now: time.Now, buckets: map[string]*bucket{}}
}

func (r *RateLimit) allow(key string) bool {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	b, ok := r.buckets[key]
	if !ok {
		b = &bucket{tokens: r.burst, last: now}
		r.buckets[key] = b
	}
	b.tokens = min(r.burst, b.tokens+now.Sub(b.last).Seconds()*r.rate)
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func (r *RateLimit) check(ctx context.Context, method string) error {
	if r == nil || strings.HasPrefix(method, healthPrefix) {
		return nil
	}
	id, ok := auth.FromContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "must authenticate first")
	}
	if !r.allow(id.Source + ":" + id.Subject) {
		r.metrics.RateLimited(id.Subject)
		return status.Error(codes.ResourceExhausted, "rate limit exceeded")
	}
	return nil
}

func (r *RateLimit) gate(ctx context.Context, method string) (context.Context, error) {
	return ctx, r.check(ctx, method)
}

// Unary interceptor unary; nil = lewat.
func (r *RateLimit) Unary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		ctx, err := r.gate(ctx, info.FullMethod)
		if err != nil {
			return nil, err
		}
		return handler(ctx, req)
	}
}

// Stream interceptor stream; nil = lewat.
func (r *RateLimit) Stream() grpc.StreamServerInterceptor {
	return func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		ctx, err := r.gate(ss.Context(), info.FullMethod)
		if err != nil {
			return err
		}
		return handler(srv, &ctxStream{ServerStream: ss, ctx: ctx})
	}
}
