package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/bankmega/karhutlasql/internal/audit"
	"github.com/bankmega/karhutlasql/internal/auth"
	"github.com/bankmega/karhutlasql/internal/session"
)

// Options dependensi Handler.
type Options struct {
	Health   func(context.Context) error
	Gatherer prometheus.Gatherer
	Sessions *session.Registry
	// Verify memverifikasi Bearer token (sama dengan jalur gRPC).
	Verify func(token string) (auth.Identity, error)
	Audit  *audit.Logger
}

// Handler mux HTTP admin: /healthz dan /metrics tanpa auth; /admin/* wajib
// Bearer dengan role admin.
func Handler(o Options) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := o.Health(ctx); err != nil {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(o.Gatherer, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /admin/sessions", o.admin(func(w http.ResponseWriter, _ *http.Request, _ auth.Identity) {
		infos := o.Sessions.Snapshot()
		for i := range infos {
			if infos[i].SQL != "" {
				infos[i].SQL = o.Audit.Redact(infos[i].SQL)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(infos)
	}))
	mux.HandleFunc("DELETE /admin/sessions/{id}", o.admin(func(w http.ResponseWriter, r *http.Request, id auth.Identity) {
		target := r.PathValue("id")
		if !o.Sessions.Kill(target) {
			http.Error(w, "session not found", http.StatusNotFound)
			return
		}
		o.Audit.Emit(auth.WithIdentity(r.Context(), id), audit.Event{Type: audit.SessionKilled, User: id.Subject, Source: id.Source, Peer: r.RemoteAddr, Target: target, Via: "http"})
		w.WriteHeader(http.StatusNoContent)
	}))
	return mux
}

// admin membungkus h dengan verifikasi Bearer (token belum di-kill) + role admin.
func (o Options) admin(h func(http.ResponseWriter, *http.Request, auth.Identity)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
		if !strings.EqualFold(scheme, "Bearer") {
			http.Error(w, "bearer token required", http.StatusUnauthorized)
			return
		}
		id, err := o.Verify(strings.TrimSpace(token))
		if err != nil || o.Sessions.Revoked(id.TokenID) {
			http.Error(w, "invalid or expired token", http.StatusUnauthorized)
			return
		}
		if !id.HasRole(auth.RoleAdmin) {
			http.Error(w, "role admin required", http.StatusForbidden)
			return
		}
		h(w, r, id)
	}
}

// Serve melayani h di lis sampai ctx selesai.
func Serve(ctx context.Context, lis net.Listener, h http.Handler, log *slog.Logger) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(lis) }()
	log.Info("admin http listening", "addr", lis.Addr().String())
	select {
	case err := <-errc:
		return fmt.Errorf("admin.Serve: %w", err)
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return fmt.Errorf("admin.Serve: %w", err)
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("admin.Serve: %w", err)
	}
	return nil
}
