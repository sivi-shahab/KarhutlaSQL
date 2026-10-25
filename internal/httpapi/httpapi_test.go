package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestParamRecord(t *testing.T) {
	var params []any
	dec := json.NewDecoder(strings.NewReader(`["JKT", 42, 1.5, true, null]`))
	dec.UseNumber()
	if err := dec.Decode(&params); err != nil {
		t.Fatal(err)
	}
	rec, err := paramRecord(params)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Release()
	want := []string{"utf8", "int64", "float64", "bool", "utf8"}
	for i, w := range want {
		if got := rec.Schema().Field(i).Type.String(); got != w {
			t.Errorf("param %d type %s, want %s", i, got, w)
		}
	}
	if !rec.Column(4).IsNull(0) || rec.NumRows() != 1 {
		t.Errorf("null param / rows wrong: %v", rec)
	}
	if _, err := paramRecord([]any{map[string]any{"x": 1}}); err == nil {
		t.Error("object param accepted")
	}
}

func TestStatusMapping(t *testing.T) {
	for c, want := range map[codes.Code]int{
		codes.PermissionDenied: 403, codes.Unauthenticated: 401, codes.InvalidArgument: 400,
		codes.ResourceExhausted: 429, codes.DeadlineExceeded: 504, codes.Internal: 500,
	} {
		w := httptest.NewRecorder()
		writeStatus(w, status.Error(c, "x"))
		if w.Code != want || !strings.Contains(w.Body.String(), `"error":"x"`) {
			t.Errorf("%s → %d %s, want %d", c, w.Code, w.Body, want)
		}
	}
}

func TestMethodAndAuth(t *testing.T) {
	h := Handler(Config{})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/query", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d", w.Code)
	}
	for _, path := range []string{"/api/v1/query", "/api/v1/login"} {
		w = httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(`{"sql":"SELECT 1"}`)))
		if w.Code != http.StatusUnauthorized || w.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("%s without auth = %d %v", path, w.Code, w.Header())
		}
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/api/v1/tables", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("tables without auth = %d", w.Code)
	}
}
