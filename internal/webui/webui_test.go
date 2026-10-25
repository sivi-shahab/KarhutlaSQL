package webui

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler(t *testing.T) {
	h := Handler()
	get := func(path, method string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), method, path, nil))
		return w
	}
	for _, p := range []string{"/", "/some/spa/route"} {
		w := get(p, http.MethodGet)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `<div id="app">`) {
			t.Errorf("%s = %d", p, w.Code)
		}
		for k, want := range map[string]string{"X-Frame-Options": "DENY", "X-Content-Type-Options": "nosniff", "Cache-Control": "no-cache"} {
			if got := w.Header().Get(k); got != want {
				t.Errorf("%s %s = %q, want %q", p, k, got, want)
			}
		}
		if csp := w.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe") {
			t.Errorf("csp = %q", csp)
		}
	}
	assets, _ := fs.Glob(dist, "dist/assets/*.js")
	if len(assets) == 0 {
		t.Fatal("no built assets embedded (run make webui)")
	}
	w := get(strings.TrimPrefix(assets[0], "dist"), http.MethodGet)
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Cache-Control"), "immutable") {
		t.Errorf("asset = %d %q", w.Code, w.Header().Get("Cache-Control"))
	}
	if w := get("/", http.MethodPost); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST / = %d", w.Code)
	}
}
