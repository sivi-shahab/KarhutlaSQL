// Package mcp server Model Context Protocol (Fase 8c) untuk agen AI: tools
// eksplorasi skema, query, dan verified queries di atas Flight SQL
// (pkg/client). Paket ini tidak menegakkan izin apa pun: setiap tool call
// menjadi statement Flight SQL dengan token pemanggil dan header kanal
// "mcp", sehingga authz, masking, RLS, batas profil ai_sandbox, kuota, dan
// audit ditegakkan server seperti klien lain.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/flight"
	"github.com/apache/arrow-go/v18/arrow/flight/flightsql"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/bankmega/karhutlasql/internal/classification"
	"github.com/bankmega/karhutlasql/pkg/client"
)

// Options batas keluaran tool (di atas batas profil server).
type Options struct {
	MaxRows  int // baris per hasil; default 500
	MaxBytes int // perkiraan byte JSON per hasil; default 1 MiB
}

// TokenKey kunci TokenInfo.Extra berisi bearer token mentah pemanggil
// (transport HTTP); tanpa itu (stdio) kredensial Client dipakai.
const TokenKey = "karhutla.token" //nolint:gosec // G101: nama kunci map, bukan kredensial

type tools struct {
	cl *client.Client
	o  Options
}

// New server MCP di atas cl. cl wajib dibuat dengan metadata
// auth.ChannelHeader = auth.ChannelMCP agar server menerapkan profil ai_sandbox.
func New(cl *client.Client, version string, o Options) *sdk.Server {
	if o.MaxRows <= 0 {
		o.MaxRows = 500
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = 1 << 20
	}
	t := &tools{cl: cl, o: o}
	s := sdk.NewServer(&sdk.Implementation{Name: "karhutlasql", Version: version}, &sdk.ServerOptions{
		Instructions: "KarhutlaSQL: data warehouse ber-governance. Mulai dari list_verified_queries dan resource " +
			"karhutla://semantic-model; pakai verified query bila cocok. Hanya satu SELECT per run_query; kolom PII " +
			"bisa ter-mask dan baris bisa difilter sesuai izin Anda.",
	})
	readOnly := &sdk.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: new(bool)}
	sdk.AddTool(s, &sdk.Tool{Name: "list_catalogs", Description: "Daftar catalog yang boleh Anda baca.", Annotations: readOnly}, t.listCatalogs)
	sdk.AddTool(s, &sdk.Tool{Name: "list_tables", Description: "Daftar tabel yang boleh Anda baca, opsional per catalog/schema.", Annotations: readOnly}, t.listTables)
	sdk.AddTool(s, &sdk.Tool{Name: "describe_table", Description: "Kolom dan tipe satu tabel.", Annotations: readOnly}, t.describeTable)
	sdk.AddTool(s, &sdk.Tool{Name: "run_query", Description: "Jalankan satu SELECT (dialek DuckDB). Hasil dibatasi baris/byte; PII bisa ter-mask.", Annotations: readOnly}, t.runQuery)
	sdk.AddTool(s, &sdk.Tool{Name: "list_verified_queries", Description: "Pertanyaan bisnis dengan SQL yang sudah diverifikasi manusia.", Annotations: readOnly}, t.listVerified)
	sdk.AddTool(s, &sdk.Tool{Name: "run_verified_query", Description: "Jalankan verified query berdasarkan nama.", Annotations: readOnly}, t.runVerified)
	s.AddResource(&sdk.Resource{URI: "karhutla://semantic-model", Name: "semantic-model", MIMEType: "application/yaml",
		Description: "Semantic model: tabel, relasi, metrik, dimensi."}, t.semanticModel)
	s.AddResource(&sdk.Resource{URI: "karhutla://verified-queries", Name: "verified-queries", MIMEType: "application/json",
		Description: "Verified queries (pertanyaan → SQL)."}, t.verifiedResource)
	return s
}

// caller ctx dengan token pemanggil (HTTP) bila ada.
func caller(ctx context.Context, extra *sdk.RequestExtra) context.Context {
	if extra != nil && extra.TokenInfo != nil {
		if tok, ok := extra.TokenInfo.Extra[TokenKey].(string); ok && tok != "" {
			return client.WithBearer(ctx, tok)
		}
	}
	return ctx
}

// Result hasil tabular tool.
type Result struct {
	Columns   []Column         `json:"columns"`
	Rows      []map[string]any `json:"rows"`
	RowCount  int              `json:"row_count"`
	Truncated bool             `json:"truncated"`
}

// Column kolom hasil.
type Column struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable,omitempty"`
}

// read mengubah stream menjadi Result (berhenti di batas baris/byte; sisa
// stream dibatalkan saat Release).
func (t *tools) read(rdr *flight.Reader) (Result, error) {
	defer rdr.Release()
	res := Result{Rows: []map[string]any{}}
	for _, f := range rdr.Schema().Fields() {
		res.Columns = append(res.Columns, Column{Name: f.Name, Type: f.Type.String()})
	}
	size := 0
	for rdr.Next() {
		rec := rdr.RecordBatch()
		for i := range int(rec.NumRows()) {
			if len(res.Rows) >= t.o.MaxRows || size >= t.o.MaxBytes {
				res.Truncated = true
				res.RowCount = len(res.Rows)
				return res, nil
			}
			row := make(map[string]any, rec.NumCols())
			for c, col := range rec.Columns() {
				row[rec.ColumnName(c)] = col.GetOneForMarshal(i)
			}
			b, err := json.Marshal(row)
			if err != nil {
				return Result{}, fmt.Errorf("mcp: %w", err)
			}
			size += len(b)
			res.Rows = append(res.Rows, row)
		}
	}
	res.RowCount = len(res.Rows)
	return res, rdr.Err()
}

func (t *tools) query(ctx context.Context, sql string) (Result, error) {
	rdr, err := t.cl.Query(ctx, sql)
	if err != nil {
		return Result{}, cleanErr(err)
	}
	res, err := t.read(rdr)
	return res, cleanErr(err)
}

// cleanErr pesan error server tanpa pembungkus berlapis klien.
func cleanErr(err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	if i := strings.Index(msg, "desc = "); i >= 0 {
		msg = msg[i+len("desc = "):]
	}
	return errors.New(msg)
}

type empty struct{}

// Catalogs hasil list_catalogs.
type Catalogs struct {
	Catalogs []string `json:"catalogs"`
}

func (t *tools) listCatalogs(ctx context.Context, req *sdk.CallToolRequest, _ empty) (*sdk.CallToolResult, Catalogs, error) {
	rdr, err := t.cl.Catalogs(caller(ctx, req.Extra))
	if err != nil {
		return nil, Catalogs{}, cleanErr(err)
	}
	res, err := t.read(rdr)
	out := Catalogs{Catalogs: []string{}}
	for _, r := range res.Rows {
		if s, ok := r["catalog_name"].(string); ok {
			out.Catalogs = append(out.Catalogs, s)
		}
	}
	return nil, out, cleanErr(err)
}

// TablesIn argumen list_tables.
type TablesIn struct {
	Catalog string `json:"catalog,omitempty" jsonschema:"filter catalog (opsional)"`
	Schema  string `json:"schema,omitempty" jsonschema:"filter schema (opsional)"`
}

// Table satu tabel.
type Table struct {
	Catalog string `json:"catalog"`
	Schema  string `json:"schema"`
	Table   string `json:"table"`
	Type    string `json:"type"`
}

// Tables hasil list_tables.
type Tables struct {
	Tables    []Table `json:"tables"`
	Truncated bool    `json:"truncated"`
}

func opt(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func (t *tools) listTables(ctx context.Context, req *sdk.CallToolRequest, in TablesIn) (*sdk.CallToolResult, Tables, error) {
	rdr, err := t.cl.GetTables(caller(ctx, req.Extra), &flightsql.GetTablesOpts{Catalog: opt(in.Catalog), DbSchemaFilterPattern: opt(in.Schema)})
	if err != nil {
		return nil, Tables{}, cleanErr(err)
	}
	res, err := t.read(rdr)
	out := Tables{Tables: []Table{}, Truncated: res.Truncated}
	for _, r := range res.Rows {
		str := func(k string) string { s, _ := r[k].(string); return s }
		out.Tables = append(out.Tables, Table{Catalog: str("catalog_name"), Schema: str("db_schema_name"), Table: str("table_name"), Type: str("table_type")})
	}
	return nil, out, cleanErr(err)
}

// DescribeIn argumen describe_table.
type DescribeIn struct {
	Catalog string `json:"catalog"`
	Schema  string `json:"schema"`
	Table   string `json:"table"`
}

// Describe hasil describe_table.
type Describe struct {
	Table   string   `json:"table"`
	Columns []Column `json:"columns"`
}

func (t *tools) describeTable(ctx context.Context, req *sdk.CallToolRequest, in DescribeIn) (*sdk.CallToolResult, Describe, error) {
	ctx = caller(ctx, req.Extra)
	// Pola LIKE: escape _ dan % agar hanya nama persis yang cocok.
	esc := strings.NewReplacer(`\`, `\\`, `_`, `\_`, `%`, `\%`)
	rdr, err := t.cl.GetTables(ctx, &flightsql.GetTablesOpts{Catalog: opt(in.Catalog), DbSchemaFilterPattern: opt(esc.Replace(in.Schema)),
		TableNameFilterPattern: opt(esc.Replace(in.Table)), IncludeSchema: true})
	if err != nil {
		return nil, Describe{}, cleanErr(err)
	}
	defer rdr.Release()
	for rdr.Next() {
		rec := rdr.RecordBatch()
		cat, sch, tbl := rec.Column(0).(*array.String), rec.Column(1).(*array.String), rec.Column(2).(*array.String) //nolint:forcetypeassert // skema Flight SQL GetTables
		raw := rec.Column(4).(*array.Binary)                                                                         //nolint:forcetypeassert // table_schema
		for i := range int(rec.NumRows()) {
			if !strings.EqualFold(cat.Value(i), in.Catalog) || !strings.EqualFold(sch.Value(i), in.Schema) || !strings.EqualFold(tbl.Value(i), in.Table) {
				continue
			}
			ts, err := flight.DeserializeSchema(raw.Value(i), nil)
			if err != nil {
				return nil, Describe{}, fmt.Errorf("mcp: schema: %w", err)
			}
			out := Describe{Table: strings.Join([]string{cat.Value(i), sch.Value(i), tbl.Value(i)}, ".")}
			for _, f := range ts.Fields() {
				out.Columns = append(out.Columns, Column{Name: f.Name, Type: f.Type.String(), Nullable: f.Nullable})
			}
			return nil, out, nil
		}
	}
	if err := rdr.Err(); err != nil {
		return nil, Describe{}, cleanErr(err)
	}
	return nil, Describe{}, fmt.Errorf("table %s.%s.%s not found or not readable", in.Catalog, in.Schema, in.Table)
}

// QueryIn argumen run_query.
type QueryIn struct {
	SQL string `json:"sql" jsonschema:"satu statement SELECT (dialek DuckDB)"`
}

func (t *tools) runQuery(ctx context.Context, req *sdk.CallToolRequest, in QueryIn) (*sdk.CallToolResult, Result, error) {
	res, err := t.query(caller(ctx, req.Extra), in.SQL)
	return nil, res, err
}

// Verified satu verified query.
type Verified struct {
	Name       string `json:"name"`
	Question   string `json:"question"`
	SQL        string `json:"sql"`
	VerifiedBy string `json:"verified_by"`
	VerifiedAt string `json:"verified_at"`
}

// VerifiedList hasil list_verified_queries.
type VerifiedList struct {
	Queries []Verified `json:"queries"`
}

func (t *tools) verified(ctx context.Context) (VerifiedList, error) {
	res, err := t.query(ctx, "SELECT name, question, sql, verified_by, verified_at::VARCHAR AS verified_at FROM karhutla_verified_queries() ORDER BY name")
	if err != nil {
		return VerifiedList{}, err
	}
	out := VerifiedList{Queries: []Verified{}}
	for _, r := range res.Rows {
		str := func(k string) string { s, _ := r[k].(string); return s }
		out.Queries = append(out.Queries, Verified{Name: str("name"), Question: str("question"), SQL: str("sql"),
			VerifiedBy: str("verified_by"), VerifiedAt: str("verified_at")})
	}
	return out, nil
}

func (t *tools) listVerified(ctx context.Context, req *sdk.CallToolRequest, _ empty) (*sdk.CallToolResult, VerifiedList, error) {
	out, err := t.verified(caller(ctx, req.Extra))
	return nil, out, err
}

// VerifiedIn argumen run_verified_query.
type VerifiedIn struct {
	Name string `json:"name"`
}

func (t *tools) runVerified(ctx context.Context, req *sdk.CallToolRequest, in VerifiedIn) (*sdk.CallToolResult, Result, error) {
	ctx = caller(ctx, req.Extra)
	res, err := t.query(ctx, "SELECT sql FROM karhutla_verified_queries() WHERE name = "+classification.Literal(in.Name))
	if err != nil {
		return nil, Result{}, err
	}
	if len(res.Rows) != 1 {
		return nil, Result{}, fmt.Errorf("verified query %q not found", in.Name)
	}
	sql, _ := res.Rows[0]["sql"].(string)
	res, err = t.query(ctx, sql)
	return nil, res, err
}

func (t *tools) semanticModel(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
	res, err := t.query(caller(ctx, req.Extra), "SELECT semantic_model FROM karhutla_semantic_model()")
	if err != nil {
		return nil, err
	}
	var text string
	if len(res.Rows) == 1 {
		text, _ = res.Rows[0]["semantic_model"].(string)
	}
	return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: req.Params.URI, MIMEType: "application/yaml", Text: text}}}, nil
}

func (t *tools) verifiedResource(ctx context.Context, req *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
	out, err := t.verified(caller(ctx, req.Extra))
	if err != nil {
		return nil, err
	}
	b, _ := json.Marshal(out) // struct string: tidak pernah error
	return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{URI: req.Params.URI, MIMEType: "application/json", Text: string(b)}}}, nil
}
