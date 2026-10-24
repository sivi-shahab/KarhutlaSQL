//go:build duckdb_arrow

package catalogsync

import (
	"context"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bankmega/karhutlasql/internal/audit"
	"github.com/bankmega/karhutlasql/internal/engine/duckdb"
)

func TestColumnMapping(t *testing.T) {
	for _, c := range []struct{ duck, text, name, json string }{
		{"BIGINT", "bigint", "LONG", `"type":"long"`},
		{"DECIMAL(15,2)", "decimal(15,2)", "DECIMAL", `"type":"decimal(15,2)"`},
		{"TIMESTAMP WITH TIME ZONE", "timestamp with time zone", "TIMESTAMP", `"type":"timestamp"`},
		{"INTEGER[]", "integer[]", "STRING", `"type":"string"`},
	} {
		u := column(0, Column{Name: "x", Type: c.duck, Nullable: true})
		if u.TypeText != c.text || u.TypeName != c.name || !strings.Contains(u.TypeJSON, c.json) {
			t.Errorf("%s → %+v", c.duck, u)
		}
	}
}

type sink struct{ events []audit.Event }

func (s *sink) Emit(e audit.Event) error { s.events = append(s.events, e); return nil }

// TestSyncUC terhadap Unity Catalog OSS asli: KARHUTLASQL_TEST_UC=http://127.0.0.1:8080
// (integration Python menjalankan container sendiri).
func TestSyncUC(t *testing.T) {
	endpoint := os.Getenv("KARHUTLASQL_TEST_UC")
	if endpoint == "" {
		t.Skip("KARHUTLASQL_TEST_UC tidak diset")
	}
	ctx := context.Background()
	eng, err := duckdb.Open(ctx, duckdb.Config{InitSQL: `ATTACH ':memory:' AS csync; CREATE TABLE csync.main.t (nik VARCHAR, saldo DECIMAL(15,2) NOT NULL);
		COMMENT ON COLUMN csync.main.t.nik IS 'NIK nasabah'`})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = eng.Close() }()
	c := NewClient(endpoint, "")
	tags := map[string]string{"nik": "pii.nik"}
	events := &sink{}
	s := New(c, Source{Engine: eng, Endpoint: "grpc://k:31337", Tags: func(string) map[string]string { return tags }},
		audit.New(events, false, func(error) {}))
	defer func() { _ = c.do(ctx, http.MethodDelete, "/catalogs/csync?force=true", nil, nil) }()

	get := func() ucTable {
		var u ucTable
		if err := c.do(ctx, http.MethodGet, "/tables/csync.main.t", nil, &u); err != nil {
			t.Fatal(err)
		}
		return u
	}
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	u := get()
	if u.Properties["karhutla.classification.nik"] != "pii.nik" || u.Properties["karhutla.dq.status"] != "none" ||
		len(u.Columns) != 2 || u.Columns[1].Nullable || u.Columns[0].Comment == nil || !strings.HasPrefix(u.StorageLocation, unreadable) {
		t.Fatalf("table %+v", u)
	}
	// Perubahan manual di UC ditimpa; tabel baru & yang dihapus ikut.
	if err := c.do(ctx, http.MethodDelete, "/tables/csync.main.t", nil, nil); err != nil {
		t.Fatal(err)
	}
	u.Properties["karhutla.classification.nik"] = "public"
	if err := c.do(ctx, http.MethodPost, "/tables", u, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Exec(ctx, "CREATE TABLE csync.main.baru (i INTEGER); CREATE SCHEMA csync.lain; CREATE TABLE csync.lain.x (i INTEGER)"); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := get().Properties["karhutla.classification.nik"]; got != "pii.nik" {
		t.Errorf("manual UC edit not reverted: %s", got)
	}
	if _, err := eng.Exec(ctx, "DROP TABLE csync.main.baru; DROP TABLE csync.lain.x; DROP SCHEMA csync.lain"); err != nil {
		t.Fatal(err)
	}
	if err := s.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.do(ctx, http.MethodGet, "/tables/csync.main.baru", nil, nil); err == nil {
		t.Error("dropped table still in UC")
	}
	if err := c.do(ctx, http.MethodGet, "/schemas/csync.lain", nil, nil); err == nil {
		t.Error("dropped schema still in UC")
	}

	var got []string
	for _, e := range events.events {
		got = append(got, e.Detail+" "+e.Object+" "+e.Status)
	}
	if want := "create csync OK|create csync.main OK|create csync.main.t OK|create csync.lain OK|create csync.lain.x OK|create csync.main.baru OK|update csync.main.t OK|delete csync.lain OK|delete csync.main.baru OK"; strings.Join(got, "|") != want {
		t.Errorf("audit catalog_sync:\n%s\nwant\n%s", strings.Join(got, "|"), want)
	}

	// Catalog bawaan UC (bukan milik KarhutlaSQL) tidak disentuh.
	if err := c.do(ctx, http.MethodGet, "/catalogs/unity", nil, nil); err != nil {
		t.Errorf("unmanaged catalog: %v", err)
	}

	// Trigger tidak memblokir; Run berhenti saat ctx selesai.
	s.Trigger()
	s.Trigger()
	rctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { s.Run(rctx, time.Hour, func(err error) { t.Error(err) }); close(done) }()
	<-done
}
