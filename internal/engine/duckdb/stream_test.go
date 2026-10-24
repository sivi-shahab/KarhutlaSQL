//go:build duckdb_arrow

package duckdb

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"
	"time"

	duckdbdrv "github.com/duckdb/duckdb-go/v2"

	"github.com/bankmega/karhutlasql/internal/engine"
)

type ctxKey struct{}

// ctxUDF ctx_value() mengembalikan nilai ctxKey dari context query — UDF
// server (identitas, nonce masking) bergantung pada propagasi ini.
type ctxUDF struct{}

func (ctxUDF) Config() duckdbdrv.ScalarFuncConfig {
	v, _ := duckdbdrv.NewTypeInfo(duckdbdrv.TYPE_VARCHAR)
	return duckdbdrv.ScalarFuncConfig{ResultTypeInfo: v, Volatile: true}
}

func (ctxUDF) Executor() duckdbdrv.ScalarFuncExecutor {
	return duckdbdrv.ScalarFuncExecutor{RowContextExecutor: func(ctx context.Context, _ []driver.Value) (any, error) {
		v, _ := ctx.Value(ctxKey{}).(string)
		return v, nil
	}}
}

func TestStreaming(t *testing.T) {
	ctx := context.Background()
	e, err := Open(ctx, Config{StreamingBufferSize: "1MB", Unlocked: true,
		Setup: func(ctx context.Context, c *sql.Conn) error {
			return duckdbdrv.RegisterScalarUDF(c, "ctx_value", ctxUDF{})
		}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close() }()
	if err := e.Streaming(); err != nil {
		t.Fatal(err)
	}

	t.Run("first chunk before full execution", func(t *testing.T) {
		// 200 jt baris tidak muat dimaterialisasi cepat; streaming memberi chunk pertama seketika.
		start := time.Now()
		rdr, err := e.Query(ctx, "SELECT range FROM range(200000000)")
		if err != nil {
			t.Fatal(err)
		}
		if !rdr.Next() || rdr.RecordBatch().NumRows() == 0 {
			t.Fatal(rdr.Err())
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("first chunk after %v", d)
		}
		rdr.Release() // berhenti lebih awal: sisa eksekusi dihentikan
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("early release took %v", d)
		}
	})
	t.Run("context reaches UDF", func(t *testing.T) {
		qctx := context.WithValue(ctx, ctxKey{}, "nonce-123")
		rdr, err := e.Query(qctx, "SELECT DISTINCT ctx_value() FROM range(100000)")
		if got := rows(t, rdr, err); len(got) != 1 || got[0] != "nonce-123" {
			t.Fatalf("got %v", got)
		}
		// Tidak bocor ke statement berikutnya tanpa nilai.
		rdr, err = e.Query(ctx, "SELECT ctx_value()")
		if got := rows(t, rdr, err); got[0] != "" {
			t.Fatalf("stale ctx: %v", got)
		}
	})
	t.Run("error mid-stream", func(t *testing.T) {
		rdr, err := e.Query(ctx, "SELECT CASE WHEN range > 500000 THEN error('boom') ELSE range END FROM range(1000000)")
		if err == nil {
			for rdr.Next() {
			}
			err = rdr.Err()
			rdr.Release()
		}
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("cancel mid-stream", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		rdr, err := e.Query(cctx, "SELECT range FROM range(1000000000000)")
		if err != nil {
			t.Fatal(err)
		}
		rdr.Next()
		cancel()
		start := time.Now()
		for rdr.Next() {
		}
		if err := rdr.Err(); !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v", err)
		}
		rdr.Release()
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("cancel took %v", d)
		}
	})
	t.Run("syntax error and params fall back", func(t *testing.T) {
		if _, err := e.Query(ctx, "SELEC 1"); err == nil {
			t.Error("syntax error must fail")
		}
		rdr, err := e.Query(ctx, "SELECT $1::INTEGER * 2", 21)
		if got := rows(t, rdr, err); got[0] != "42" {
			t.Errorf("param: %v", got)
		}
	})
	t.Run("next after end", func(t *testing.T) {
		rdr, err := e.Query(ctx, "SELECT 1")
		if err != nil {
			t.Fatal(err)
		}
		defer rdr.Release()
		for rdr.Next() {
		}
		if rdr.Next() || rdr.Err() != nil {
			t.Fatalf("second Next after end: err %v", rdr.Err())
		}
	})
	t.Run("stats", func(t *testing.T) {
		var st engine.Stats
		rdr, err := e.Query(engine.WithStats(ctx, &st), "SELECT range % 7, count(*) FROM range(100000) GROUP BY 1")
		rows(t, rdr, err)
		if st.PlanHash == "" || st.PeakMemory <= 0 {
			t.Errorf("stats %+v", st)
		}
	})
}

// TestStreaming_Permission: error izin guardrail tetap bertipe di jalur streaming.
func TestStreaming_Permission(t *testing.T) {
	e, err := Open(context.Background(), Config{StreamingBufferSize: "1MB", AllowedDirectories: []string{t.TempDir() + "/"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close() }()
	if _, err := e.Query(context.Background(), "SELECT * FROM read_csv('/etc/passwd')"); !errors.Is(err, engine.ErrPermission) {
		t.Fatalf("err = %v", err)
	}
}

func TestQuery_Materialized(t *testing.T) {
	e, err := Open(context.Background(), Config{Unlocked: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close() }()
	if e.Streaming() == nil {
		t.Fatal("streaming must be off without StreamingBufferSize")
	}
	rdr, err := e.Query(context.Background(), "SELECT * FROM range(100000)")
	if got := len(rows(t, rdr, err)); got != 100000 {
		t.Fatalf("rows = %d", got)
	}
}

func TestStreaming_Guardrail(t *testing.T) {
	e, err := Open(context.Background(), Config{StreamingBufferSize: "1MB", AllowedDirectories: []string{t.TempDir() + "/"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = e.Close() }()
	rdr, err := e.Query(context.Background(), "SELECT 42")
	if got := rows(t, rdr, err); got[0] != "42" {
		t.Fatal(got)
	}
}
