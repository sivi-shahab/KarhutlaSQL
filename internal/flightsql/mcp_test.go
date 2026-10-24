//go:build duckdb_arrow

package flightsql

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/oauth2"

	"github.com/bankmega/karhutlasql/internal/audit"
	"github.com/bankmega/karhutlasql/internal/auth"
	"github.com/bankmega/karhutlasql/internal/mcp"
	"github.com/bankmega/karhutlasql/internal/verified"
	"github.com/bankmega/karhutlasql/pkg/client"
)

const testVerified = `
semantic_model: |
  tables:
    warehouse.main.nasabah: { description: nasabah per cabang }
verified_queries:
  nasabah_per_cabang:
    question: Berapa jumlah nasabah per cabang?
    sql: SELECT branch_code, count(*) AS n FROM warehouse.main.nasabah GROUP BY 1 ORDER BY 1
    verified_by: dian@bankmega.com
    verified_at: 2026-10-01T00:00:00Z
`

// mcpSession sesi MCP (transport in-memory) atas server test sebagai user.
func mcpSession(t *testing.T, g *govEnv, user string, maxRows int) *sdk.ClientSession {
	t.Helper()
	ctx := context.Background()
	cl, err := client.Dial(ctx, client.Config{Addr: g.addr, TokenSource: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: g.tok[user]}),
		Metadata: map[string]string{auth.ChannelHeader: auth.ChannelMCP}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Close() })
	st, ct := sdk.NewInMemoryTransports()
	if _, err := mcp.New(cl, "test", mcp.Options{MaxRows: maxRows}).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// call menjalankan tool; hasil = isi JSON terstruktur, atau teks error.
func call(t *testing.T, cs *sdk.ClientSession, tool string, args map[string]any, out any) string {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	if res.IsError {
		return res.Content[0].(*sdk.TextContent).Text
	}
	b, _ := json.Marshal(res.StructuredContent)
	if err := json.Unmarshal(b, out); err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return ""
}

// MCP (Fase 8c): tools di atas Flight SQL, profil ai_sandbox untuk semua
// pemanggil, governance tetap per identitas.
func TestMCP(t *testing.T) {
	path := filepath.Join(t.TempDir(), "verified.yaml")
	if err := os.WriteFile(path, []byte(testVerified), 0o600); err != nil {
		t.Fatal(err)
	}
	vs, err := verified.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	g := startGov(t, func(o *envOpts) { o.verified = vs })
	ops, analyst := mcpSession(t, g, "ops", 0), mcpSession(t, g, "analyst", 0)

	var tables mcp.Tables
	if e := call(t, analyst, "list_tables", map[string]any{"catalog": "warehouse"}, &tables); e != "" || len(tables.Tables) != 1 || tables.Tables[0].Table != "nasabah" {
		t.Errorf("list_tables %+v %s", tables, e)
	}
	var desc mcp.Describe
	if e := call(t, analyst, "describe_table", map[string]any{"catalog": "warehouse", "schema": "main", "table": "nasabah"}, &desc); e != "" || len(desc.Columns) != 5 {
		t.Errorf("describe_table %+v %s", desc, e)
	}
	// Governance per identitas: analyst melihat NIK ter-mask.
	var res mcp.Result
	if e := call(t, analyst, "run_query", map[string]any{"sql": "SELECT nik FROM warehouse.nasabah ORDER BY nik"}, &res); e != "" ||
		len(res.Rows) == 0 || strings.Contains(res.Rows[0]["nik"].(string), g.secret) {
		t.Errorf("analyst run_query %+v %s", res, e)
	}
	// Profil ai_sandbox berlaku juga untuk admin: SELECT-only, max_rows 3 (policy).
	if e := call(t, ops, "run_query", map[string]any{"sql": "INSERT INTO sandbox.main.s VALUES ('x')"}, &res); !strings.Contains(e, "SELECT-only") {
		t.Errorf("admin write via mcp: %q", e)
	}
	if e := call(t, ops, "run_query", map[string]any{"sql": "SELECT * FROM range(10)"}, &res); e != "" || res.RowCount != 3 {
		t.Errorf("admin rows via mcp: %+v %s", res, e)
	}
	if e := call(t, mcpSession(t, g, "ops", 2), "run_query", map[string]any{"sql": "SELECT * FROM range(10)"}, &res); e != "" || res.RowCount != 2 || !res.Truncated {
		t.Errorf("mcp output cap: %+v %s", res, e)
	}
	if got, err := g.rows("ops", "SELECT * FROM range(10)"); err != nil || len(got) != 10 { // tanpa kanal mcp: admin tidak terbatas
		t.Errorf("admin without mcp channel: %d rows %v", len(got), err)
	}
	// Verified queries: daftar + eksekusi sebagai pemanggil.
	var vl mcp.VerifiedList
	if e := call(t, analyst, "list_verified_queries", nil, &vl); e != "" || len(vl.Queries) != 1 || vl.Queries[0].VerifiedBy != "dian@bankmega.com" {
		t.Errorf("list_verified_queries %+v %s", vl, e)
	}
	if e := call(t, analyst, "run_verified_query", map[string]any{"name": "nasabah_per_cabang"}, &res); e != "" || res.RowCount != 2 {
		t.Errorf("run_verified_query %+v %s", res, e)
	}
	rr, err := analyst.ReadResource(context.Background(), &sdk.ReadResourceParams{URI: "karhutla://semantic-model"})
	if err != nil || !strings.Contains(rr.Contents[0].Text, "nasabah per cabang") {
		t.Errorf("semantic model %v %v", rr, err)
	}
	// Audit menandai kanal.
	mcpEvents := 0
	for _, e := range g.audit.ofType(audit.StatementEnd) {
		if e.Channel == auth.ChannelMCP {
			mcpEvents++
		}
	}
	if mcpEvents < 6 {
		t.Errorf("statement_end with channel mcp: %d", mcpEvents)
	}
}
