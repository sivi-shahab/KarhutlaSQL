package catalogsync

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/bankmega/karhutlasql/internal/audit"
	"github.com/bankmega/karhutlasql/internal/dq"
	"github.com/bankmega/karhutlasql/internal/engine"
)

// Properties: catalog/schema milik KarhutlaSQL ditandai ManagedProp; hanya
// objek di bawah catalog bertanda yang dibuat ulang atau dihapus.
const (
	ManagedProp = "karhutla.managed"
	prefix      = "karhutla."
	// Placeholder storage_location yang tidak bisa dibaca (K-03, ADR 13).
	unreadable = "file:///karhutlasql-unreadable/"
)

// Table metadata satu tabel yang didorong ke UC.
type Table struct {
	Catalog, Schema, Name string
	Comment               string
	Columns               []Column
	Properties            map[string]string
}

// Column satu kolom (tipe DuckDB).
type Column struct {
	Name, Type, Comment string
	Nullable            bool
}

// Source sumber metadata (sumber kebenaran: engine + classification + DQ).
type Source struct {
	Engine engine.Engine
	// Tags kolom bertag milik tabel "catalog.schema.table" → tag (G-03).
	Tags func(table string) map[string]string
	// DQ validasi terbaru per tabel; nil = tanpa status DQ.
	DQ       *dq.Store
	Endpoint string // karhutla.endpoint
}

// Tables metadata semua tabel (bukan view, temp, atau katalog internal).
func (s Source) Tables(ctx context.Context) ([]Table, error) {
	rdr, err := s.Engine.Query(ctx, `SELECT c.database_name, c.schema_name, c.table_name, t.comment, c.column_name, c.data_type, c.is_nullable, c.comment
		FROM duckdb_columns() c JOIN duckdb_tables() t ON c.table_oid = t.table_oid
		WHERE NOT t.internal AND NOT t.temporary AND c.database_name NOT IN ('system', 'temp')
		ORDER BY 1, 2, 3, c.column_index`)
	if err != nil {
		return nil, fmt.Errorf("catalogsync.Tables: %w", err)
	}
	defer rdr.Release()
	latest := map[string]dq.Validation{}
	if s.DQ != nil {
		for _, v := range s.DQ.All() {
			latest[v.Table] = v
		}
	}
	var out []Table
	for rdr.Next() {
		rec := rdr.RecordBatch()
		for r := range int(rec.NumRows()) {
			v := func(i int) string {
				if rec.Column(i).IsNull(r) {
					return ""
				}
				return strings.Clone(rec.Column(i).ValueStr(r))
			}
			cat, sch, name := strings.ToLower(v(0)), strings.ToLower(v(1)), strings.ToLower(v(2))
			if n := len(out); n == 0 || out[n-1].Catalog != cat || out[n-1].Schema != sch || out[n-1].Name != name {
				out = append(out, s.table(cat, sch, name, v(3), latest))
			}
			t := &out[len(out)-1]
			t.Columns = append(t.Columns, Column{Name: strings.ToLower(v(4)), Type: v(5), Nullable: v(6) == "true", Comment: v(7)})
		}
	}
	if err := rdr.Err(); err != nil {
		return nil, fmt.Errorf("catalogsync.Tables: %w", err)
	}
	return out, nil
}

// table properties karhutla.* (K-02): cermin read-only, tidak pernah dibaca balik.
func (s Source) table(cat, sch, name, comment string, latest map[string]dq.Validation) Table {
	full := cat + "." + sch + "." + name
	props := map[string]string{ManagedProp: "true", prefix + "dq.status": "none"}
	if s.Endpoint != "" {
		props[prefix+"endpoint"] = s.Endpoint
	}
	if s.Tags != nil {
		for c, tag := range s.Tags(full) {
			props[prefix+"classification."+c] = tag
		}
	}
	if v, ok := latest[full]; ok {
		props[prefix+"dq.status"] = "failed"
		if v.FailedCritical == 0 {
			props[prefix+"dq.status"] = "passed"
		}
		props[prefix+"dq.validation_id"] = v.ID
		props[prefix+"dq.validated_at"] = v.ValidatedAt.UTC().Format(time.RFC3339)
	}
	return Table{Catalog: cat, Schema: sch, Name: name, Comment: comment, Properties: props}
}

// --- klien REST UC OSS (/api/2.1/unity-catalog) ---

// Client klien REST Unity Catalog OSS (stdlib net/http).
type Client struct {
	base  string
	token string
	hc    *http.Client
}

// NewClient endpoint mis. http://uc:8080; token opsional (Bearer).
func NewClient(endpoint, token string) *Client {
	return &Client{base: strings.TrimRight(endpoint, "/") + "/api/2.1/unity-catalog", token: token, hc: &http.Client{Timeout: 30 * time.Second}}
}

var errNotFound = errors.New("not found")

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	// Host dari flag --uc-endpoint (operator); segmen path di-escape.
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd) //nolint:gosec // G704: host dari konfigurasi operator
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.hc.Do(req) //nolint:gosec // G704: lihat di atas
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return fmt.Errorf("%s %s: %w", method, path, errNotFound)
	case resp.StatusCode/100 != 2:
		return fmt.Errorf("%s %s: HTTP %d: %.300s", method, path, resp.StatusCode, b)
	case out != nil:
		return json.Unmarshal(b, out)
	}
	return nil
}

type ucObject struct {
	Name       string            `json:"name"`
	Properties map[string]string `json:"properties"`
}

type ucColumn struct {
	Name      string  `json:"name"`
	TypeText  string  `json:"type_text"`
	TypeName  string  `json:"type_name"`
	TypeJSON  string  `json:"type_json"`
	Precision *int    `json:"type_precision,omitempty"`
	Scale     *int    `json:"type_scale,omitempty"`
	Position  int     `json:"position"`
	Nullable  bool    `json:"nullable"`
	Comment   *string `json:"comment"`
}

type ucTable struct {
	Name            string            `json:"name"`
	CatalogName     string            `json:"catalog_name"`
	SchemaName      string            `json:"schema_name"`
	TableType       string            `json:"table_type"`
	Format          string            `json:"data_source_format"`
	StorageLocation string            `json:"storage_location"`
	Comment         *string           `json:"comment"`
	Columns         []ucColumn        `json:"columns"`
	Properties      map[string]string `json:"properties"`
}

// list GET berhalaman: key = nama array di respons.
func list[T any](ctx context.Context, c *Client, path, key string) ([]T, error) {
	var out []T
	token := ""
	for {
		p := path + "&max_results=1000"
		if token != "" {
			p += "&page_token=" + url.QueryEscape(token)
		}
		var page map[string]json.RawMessage
		if err := c.do(ctx, http.MethodGet, p, nil, &page); err != nil {
			return nil, err
		}
		var items []T
		if err := json.Unmarshal(page[key], &items); err != nil && page[key] != nil {
			return nil, fmt.Errorf("GET %s: %w", path, err)
		}
		out = append(out, items...)
		_ = json.Unmarshal(page["next_page_token"], &token)
		if token == "" || len(items) == 0 {
			return out, nil
		}
	}
}

// --- diff & push ---

// Syncer mendorong metadata satu arah ke UC (K-01) di goroutine sendiri.
type Syncer struct {
	c      *Client
	src    Source
	audit  *audit.Logger
	errors prometheus.Counter
	kick   chan struct{}
	mu     sync.Mutex // satu Sync pada satu waktu
}

// New Syncer; Errors() harus didaftarkan ke registry metrics.
func New(c *Client, src Source, a *audit.Logger) *Syncer {
	return &Syncer{c: c, src: src, audit: a, kick: make(chan struct{}, 1), errors: prometheus.NewCounter(prometheus.CounterOpts{
		Name: "karhutla_catalog_sync_errors_total", Help: "Operasi sinkron Unity Catalog yang gagal.",
	})}
}

// Errors counter karhutla_catalog_sync_errors_total.
func (s *Syncer) Errors() prometheus.Counter { return s.errors }

// Trigger meminta sinkron secepatnya (pasca-DDL/DQ); tidak pernah memblokir.
func (s *Syncer) Trigger() {
	if s == nil {
		return
	}
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// Run sinkron saat start, tiap every, dan saat Trigger; gagal → retry dengan
// backoff 1s..every. Berhenti saat ctx selesai.
func (s *Syncer) Run(ctx context.Context, every time.Duration, onErr func(error)) {
	backoff := time.Duration(0)
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-s.kick:
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
		}
		next := every
		if err := s.Sync(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			onErr(err)
			backoff = min(max(2*backoff, time.Second), every)
			next = backoff
		} else {
			backoff = 0
		}
		t.Reset(next)
	}
}

// Sync satu putaran penuh: create yang belum ada, delete+create yang berbeda
// (UC OSS tidak punya PATCH tabel), delete yang sudah tidak ada — hanya di
// catalog bertanda ManagedProp. Error per objek dicatat dan tidak
// menghentikan objek lain.
func (s *Syncer) Sync(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tables, err := s.src.Tables(ctx)
	if err != nil {
		return s.fail(ctx, "*", "read", err)
	}
	want := map[string]map[string]map[string]Table{} // catalog → schema → table
	for _, t := range tables {
		if want[t.Catalog] == nil {
			want[t.Catalog] = map[string]map[string]Table{}
		}
		if want[t.Catalog][t.Schema] == nil {
			want[t.Catalog][t.Schema] = map[string]Table{}
		}
		want[t.Catalog][t.Schema][t.Name] = t
	}
	cats, err := list[ucObject](ctx, s.c, "/catalogs?", "catalogs")
	if err != nil {
		return s.fail(ctx, "*", "list", err)
	}
	have := map[string]bool{} // nama → dikelola KarhutlaSQL
	for _, c := range cats {
		have[c.Name] = c.Properties[ManagedProp] == "true"
	}
	var errs []error
	for _, cat := range slices.Sorted(maps.Keys(want)) {
		managed, exists := have[cat]
		switch {
		case !exists:
			if err := s.apply(ctx, cat, "create", http.MethodPost, "/catalogs", ucObject{Name: cat, Properties: map[string]string{ManagedProp: "true"}}); err != nil {
				errs = append(errs, err)
				continue
			}
		case !managed:
			errs = append(errs, s.fail(ctx, cat, "create", errors.New("catalog exists in Unity Catalog and is not managed by karhutlasql")))
			continue
		}
		errs = append(errs, s.syncCatalog(ctx, cat, want[cat]))
	}
	for _, c := range cats {
		if _, ok := want[c.Name]; !ok && have[c.Name] {
			errs = append(errs, s.apply(ctx, c.Name, "delete", http.MethodDelete, "/catalogs/"+url.PathEscape(c.Name)+"?force=true", nil))
		}
	}
	return errors.Join(errs...)
}

func (s *Syncer) syncCatalog(ctx context.Context, cat string, want map[string]map[string]Table) error {
	schemas, err := list[ucObject](ctx, s.c, "/schemas?catalog_name="+url.QueryEscape(cat), "schemas")
	if err != nil {
		return s.fail(ctx, cat, "list", err)
	}
	var errs []error
	for _, sc := range schemas {
		if _, ok := want[sc.Name]; !ok {
			errs = append(errs, s.apply(ctx, cat+"."+sc.Name, "delete", http.MethodDelete, "/schemas/"+url.PathEscape(cat+"."+sc.Name)+"?force=true", nil))
		}
	}
	for _, sch := range slices.Sorted(maps.Keys(want)) {
		if !slices.ContainsFunc(schemas, func(o ucObject) bool { return o.Name == sch }) {
			if err := s.apply(ctx, cat+"."+sch, "create", http.MethodPost, "/schemas",
				map[string]any{"name": sch, "catalog_name": cat, "properties": map[string]string{ManagedProp: "true"}}); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		errs = append(errs, s.syncSchema(ctx, cat, sch, want[sch]))
	}
	return errors.Join(errs...)
}

func (s *Syncer) syncSchema(ctx context.Context, cat, sch string, want map[string]Table) error {
	have, err := list[ucTable](ctx, s.c, "/tables?catalog_name="+url.QueryEscape(cat)+"&schema_name="+url.QueryEscape(sch), "tables")
	if err != nil {
		return s.fail(ctx, cat+"."+sch, "list", err)
	}
	var errs []error
	got := map[string]ucTable{}
	for _, t := range have {
		got[t.Name] = t
		if _, ok := want[t.Name]; !ok {
			errs = append(errs, s.deleteTable(ctx, cat, sch, t.Name, "delete"))
		}
	}
	for _, name := range slices.Sorted(maps.Keys(want)) {
		body := toUC(want[name])
		full := cat + "." + sch + "." + name
		action := "create"
		if g, ok := got[name]; ok {
			if same(body, g) {
				continue
			}
			action = "update"
			if err := s.deleteTable(ctx, cat, sch, name, action); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		errs = append(errs, s.apply(ctx, full, action, http.MethodPost, "/tables", body))
	}
	return errors.Join(errs...)
}

func (s *Syncer) deleteTable(ctx context.Context, cat, sch, name, action string) error {
	full := cat + "." + sch + "." + name
	err := s.c.do(ctx, http.MethodDelete, "/tables/"+url.PathEscape(full), nil, nil)
	if err != nil && !errors.Is(err, errNotFound) {
		return s.fail(ctx, full, action, err)
	}
	if action == "delete" {
		s.emit(ctx, full, action, nil)
	}
	return nil
}

// apply satu mutasi UC + audit catalog_sync.
func (s *Syncer) apply(ctx context.Context, object, action, method, path string, body any) error {
	if err := s.c.do(ctx, method, path, body, nil); err != nil && (method != http.MethodDelete || !errors.Is(err, errNotFound)) {
		return s.fail(ctx, object, action, err)
	}
	s.emit(ctx, object, action, nil)
	return nil
}

func (s *Syncer) fail(ctx context.Context, object, action string, err error) error {
	s.errors.Inc()
	s.emit(ctx, object, action, err)
	return fmt.Errorf("catalogsync %s %s: %w", action, object, err)
}

func (s *Syncer) emit(ctx context.Context, object, action string, err error) {
	ev := audit.Event{Type: audit.CatalogSync, User: "karhutlasql", Source: "system", Object: object, Detail: action, Status: "OK"}
	if err != nil {
		ev.Status, ev.Error = "FAILED", err.Error()
	}
	s.audit.Emit(ctx, ev)
}

// toUC tabel sebagai body POST /tables: EXTERNAL dengan storage_location
// placeholder yang tidak bisa dibaca (ADR 13).
func toUC(t Table) ucTable {
	u := ucTable{Name: t.Name, CatalogName: t.Catalog, SchemaName: t.Schema, TableType: "EXTERNAL", Format: "PARQUET",
		StorageLocation: unreadable + t.Catalog + "/" + t.Schema + "/" + t.Name, Properties: t.Properties}
	if t.Comment != "" {
		u.Comment = &t.Comment
	}
	for i, c := range t.Columns {
		u.Columns = append(u.Columns, column(i, c))
	}
	return u
}

var decimalType = regexp.MustCompile(`^DECIMAL\((\d+),(\d+)\)$`)

// column tipe DuckDB → tipe Spark (yang diminta type_json UC OSS). Tipe tanpa
// padanan (LIST, STRUCT, UUID, ...) dikirim sebagai string; type_text tetap
// tipe DuckDB.
func column(pos int, c Column) ucColumn {
	spark, name := "string", "STRING"
	u := ucColumn{Name: c.Name, Position: pos, Nullable: c.Nullable}
	switch typ := strings.ToUpper(c.Type); typ {
	case "BOOLEAN":
		spark, name = "boolean", "BOOLEAN"
	case "TINYINT":
		spark, name = "byte", "BYTE"
	case "SMALLINT":
		spark, name = "short", "SHORT"
	case "INTEGER":
		spark, name = "integer", "INT"
	case "BIGINT":
		spark, name = "long", "LONG"
	case "FLOAT":
		spark, name = "float", "FLOAT"
	case "DOUBLE":
		spark, name = "double", "DOUBLE"
	case "DATE":
		spark, name = "date", "DATE"
	case "TIMESTAMP":
		spark, name = "timestamp_ntz", "TIMESTAMP_NTZ"
	case "TIMESTAMP WITH TIME ZONE":
		spark, name = "timestamp", "TIMESTAMP"
	case "BLOB":
		spark, name = "binary", "BINARY"
	default:
		if m := decimalType.FindStringSubmatch(typ); m != nil {
			p, _ := strconv.Atoi(m[1])
			sc, _ := strconv.Atoi(m[2])
			spark, name, u.Precision, u.Scale = "decimal("+m[1]+","+m[2]+")", "DECIMAL", &p, &sc
		}
	}
	u.TypeText, u.TypeName = strings.ToLower(c.Type), name
	meta := map[string]string{}
	if c.Comment != "" {
		u.Comment = &c.Comment
		meta["comment"] = c.Comment
	}
	tj, _ := json.Marshal(map[string]any{"name": c.Name, "type": spark, "nullable": c.Nullable, "metadata": meta}) // selalu bisa
	u.TypeJSON = string(tj)
	return u
}

// same tabel di UC sama dengan yang diinginkan (properties, kolom, lokasi):
// perubahan manual di UC (mis. property karhutla.classification.*) ditimpa
// putaran berikutnya.
func same(want, got ucTable) bool {
	str := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	if got.TableType != want.TableType || got.StorageLocation != want.StorageLocation || str(got.Comment) != str(want.Comment) ||
		!maps.Equal(got.Properties, want.Properties) || len(got.Columns) != len(want.Columns) {
		return false
	}
	for i, w := range want.Columns {
		g := got.Columns[i]
		if g.Name != w.Name || g.TypeText != w.TypeText || g.TypeName != w.TypeName || g.Nullable != w.Nullable ||
			g.Position != w.Position || str(g.Comment) != str(w.Comment) {
			return false
		}
	}
	return true
}
