package admin

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/bankmega/karhutlasql/internal/audit"
	"github.com/bankmega/karhutlasql/internal/auth"
	"github.com/bankmega/karhutlasql/internal/session"
)

type sink struct{ events []audit.Event }

func (s *sink) Emit(e audit.Event) error { s.events = append(s.events, e); return nil }

func TestHandler(t *testing.T) {
	exp := time.Now().Add(time.Hour)
	ids := map[string]auth.Identity{
		"tok-admin": {Subject: "admin", Roles: []string{auth.RoleAdmin}, TokenID: "S1", ExpiresAt: exp},
		"tok-user":  {Subject: "ana", TokenID: "S2", ExpiresAt: exp},
		"tok-other": {Subject: "admin", Roles: []string{auth.RoleAdmin}, TokenID: "S3", ExpiresAt: exp},
	}
	reg := session.NewRegistry(time.Hour)
	for _, id := range ids {
		if _, err := reg.Attach(id, "peer"); err != nil {
			t.Fatal(err)
		}
	}
	pr := prometheus.NewRegistry()
	pr.MustRegister(prometheus.NewCounter(prometheus.CounterOpts{Name: "x_total"}))
	sk := &sink{}
	h := Handler(Options{
		Health:   func(context.Context) error { return nil },
		Gatherer: pr,
		Sessions: reg,
		Verify: func(tok string) (auth.Identity, error) {
			if id, ok := ids[tok]; ok {
				return id, nil
			}
			return auth.Identity{}, errors.New("bad")
		},
		Audit: audit.New(sk, false, nil),
	})
	do := func(method, path, tok string) *httptest.ResponseRecorder {
		r := httptest.NewRequestWithContext(context.Background(), method, path, nil)
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	tests := []struct {
		method, path, tok string
		want              int
	}{
		{"GET", "/healthz", "", 200},
		{"GET", "/metrics", "", 200},
		{"GET", "/admin/sessions", "", 401},
		{"GET", "/admin/sessions", "garbage", 401},
		{"GET", "/admin/sessions", "tok-user", 403},
		{"DELETE", "/admin/sessions/S2", "tok-user", 403},
		{"GET", "/admin/sessions", "tok-admin", 200},
		{"DELETE", "/admin/sessions/nope", "tok-admin", 404},
		{"DELETE", "/admin/sessions/S3", "tok-admin", 204},
		{"GET", "/admin/sessions", "tok-other", 401}, // token sesi yang di-kill
	}
	for _, tt := range tests {
		if w := do(tt.method, tt.path, tt.tok); w.Code != tt.want {
			t.Errorf("%s %s (%s) = %d %s, want %d", tt.method, tt.path, tt.tok, w.Code, w.Body, tt.want)
		}
	}
	if w := do("GET", "/metrics", ""); !strings.Contains(w.Body.String(), "x_total 0") {
		t.Errorf("/metrics body = %s", w.Body)
	}
	if w := do("GET", "/admin/sessions", "tok-admin"); !strings.Contains(w.Body.String(), `"ID":"S1"`) || strings.Contains(w.Body.String(), `"S3"`) {
		t.Errorf("sessions body = %s", w.Body)
	}
	if len(sk.events) != 1 || sk.events[0].Type != audit.SessionKilled || sk.events[0].Target != "S3" || sk.events[0].SessionID != "S1" {
		t.Errorf("audit = %+v", sk.events)
	}
}
