//go:build duckdb_arrow

package flightsql

import (
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Span statement/authorize/queue (Fase 8b); atribut tanpa teks SQL.
func TestTracing(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	g := startGov(t)
	if _, err := g.rows("analyst", "SELECT nik FROM warehouse.nasabah WHERE nama = 'Nama 7'"); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, s := range rec.Ended() {
		names[s.Name()] = true
		if s.Name() != "statement" {
			continue
		}
		attrs := map[string]string{}
		for _, a := range s.Attributes() {
			attrs[string(a.Key)] = a.Value.String()
			if strings.Contains(a.Value.String(), "Nama 7") {
				t.Errorf("SQL text leaked into span attribute %s", a.Key)
			}
		}
		if attrs["enduser.id"] != "analyst" || attrs["karhutla.rows"] != "1" || len(attrs["karhutla.sql_sha256"]) != 64 || attrs["karhutla.status"] != "OK" {
			t.Errorf("statement attrs %v", attrs)
		}
	}
	for _, n := range []string{"statement", "authorize", "queue"} {
		if !names[n] {
			t.Errorf("span %q missing (got %v)", n, names)
		}
	}
}
