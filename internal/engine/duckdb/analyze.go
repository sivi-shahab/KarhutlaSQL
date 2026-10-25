//go:build duckdb_arrow

package duckdb

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	duckdbdrv "github.com/duckdb/duckdb-go/v2"

	"github.com/bankmega/karhutlasql/internal/engine"
)

var kinds = map[duckdbdrv.StmtType]string{
	duckdbdrv.STATEMENT_TYPE_SELECT: "SELECT", duckdbdrv.STATEMENT_TYPE_INSERT: "INSERT",
	duckdbdrv.STATEMENT_TYPE_UPDATE: "UPDATE", duckdbdrv.STATEMENT_TYPE_EXPLAIN: "EXPLAIN",
	duckdbdrv.STATEMENT_TYPE_DELETE: "DELETE", duckdbdrv.STATEMENT_TYPE_PREPARE: "PREPARE",
	duckdbdrv.STATEMENT_TYPE_CREATE: "CREATE", duckdbdrv.STATEMENT_TYPE_EXECUTE: "EXECUTE",
	duckdbdrv.STATEMENT_TYPE_ALTER: "ALTER", duckdbdrv.STATEMENT_TYPE_TRANSACTION: "TRANSACTION",
	duckdbdrv.STATEMENT_TYPE_COPY: "COPY", duckdbdrv.STATEMENT_TYPE_ANALYZE: "ANALYZE",
	duckdbdrv.STATEMENT_TYPE_VARIABLE_SET: "VARIABLE_SET", duckdbdrv.STATEMENT_TYPE_CREATE_FUNC: "CREATE_FUNC",
	duckdbdrv.STATEMENT_TYPE_DROP: "DROP", duckdbdrv.STATEMENT_TYPE_EXPORT: "EXPORT",
	duckdbdrv.STATEMENT_TYPE_PRAGMA: "PRAGMA", duckdbdrv.STATEMENT_TYPE_VACUUM: "VACUUM",
	duckdbdrv.STATEMENT_TYPE_CALL: "CALL", duckdbdrv.STATEMENT_TYPE_SET: "SET",
	duckdbdrv.STATEMENT_TYPE_LOAD: "LOAD", duckdbdrv.STATEMENT_TYPE_RELATION: "RELATION",
	duckdbdrv.STATEMENT_TYPE_EXTENSION: "EXTENSION", duckdbdrv.STATEMENT_TYPE_LOGICAL_PLAN: "LOGICAL_PLAN",
	duckdbdrv.STATEMENT_TYPE_ATTACH: "ATTACH", duckdbdrv.STATEMENT_TYPE_DETACH: "DETACH",
}

// explained tipe statement yang dianalisis lewat EXPLAIN; tipe lain hanya
// dilaporkan Kind-nya (pemanggil memperlakukannya sebagai operasi sistem).
var explained = []string{"SELECT", "INSERT", "UPDATE", "DELETE", "CREATE", "DROP", "ALTER", "CALL"}

// operators nama operator plan logis/fisik DuckDB yang bukan sumber data.
// Node lain tanpa "Table" dianggap table function (fail-closed: operator baru
// di versi DuckDB berikutnya terbaca sebagai fungsi, bukan diabaikan).
var operators = map[string]bool{}

func init() {
	for _, n := range strings.Fields(`PROJECTION FILTER AGGREGATE UNGROUPED_AGGREGATE HASH_GROUP_BY
		PERFECT_HASH_GROUP_BY PARTITIONED_AGGREGATE WINDOW STREAMING_WINDOW UNNEST LIMIT STREAMING_LIMIT
		LIMIT_PERCENT ORDER_BY TOP_N DISTINCT SAMPLE RESERVOIR_SAMPLE STREAMING_SAMPLE PIVOT
		DUMMY_SCAN EMPTY_RESULT COLUMN_DATA_SCAN CHUNK_GET CHUNK_SCAN EXPRESSION_GET EXPRESSION_SCAN DELIM_GET
		DELIM_SCAN CTE_REF CTE_SCAN CTE MATERIALIZED_CTE RECURSIVE_CTE RECURSIVE_CTE_SCAN
		RECURSIVE_RECURRING_CTE_SCAN JOIN COMPARISON_JOIN DELIM_JOIN LEFT_DELIM_JOIN RIGHT_DELIM_JOIN
		ANY_JOIN CROSS_PRODUCT POSITIONAL_JOIN POSITIONAL_SCAN ASOF_JOIN DEPENDENT_JOIN HASH_JOIN
		NESTED_LOOP_JOIN BLOCKWISE_NL_JOIN PIECEWISE_MERGE_JOIN IE_JOIN UNION EXCEPT INTERSECT
		INSERT BATCH_INSERT DELETE UPDATE MERGE_INTO CREATE_TABLE CREATE_TABLE_AS BATCH_CREATE_TABLE_AS
		CREATE_INDEX CREATE_SEQUENCE CREATE_VIEW CREATE_SCHEMA CREATE_TYPE ALTER DROP RESULT_COLLECTOR`) {
		operators[n] = true
	}
}

type planNode struct {
	Name      string         `json:"name"`
	Children  []planNode     `json:"children"`
	ExtraInfo map[string]any `json:"extra_info"`
}

// Analyze lihat engine.Engine. Satu statement dijamin oleh Conn.Prepare
// (extractor DuckDB, tanpa eksekusi) sebelum EXPLAIN dijalankan.
func (e *Engine) Analyze(ctx context.Context, query string) (engine.Analysis, error) {
	c, err := e.conn(ctx)
	if err != nil {
		return engine.Analysis{}, wrap("duckdb.Analyze", err)
	}
	defer func() { _ = c.Close() }()

	ds, err := c.Prepare(query) // count != 1 → error, tidak ada yang dieksekusi
	if err != nil {
		return engine.Analysis{}, wrap("duckdb.Analyze", err)
	}
	st := ds.(*duckdbdrv.Stmt)
	typ, terr := st.StatementType()
	nparams := st.NumInput()
	if err := errors.Join(terr, ds.Close()); err != nil {
		return engine.Analysis{}, wrap("duckdb.Analyze", err)
	}
	a := engine.Analysis{Kind: kinds[typ]}
	if a.Kind == "" {
		a.Kind = fmt.Sprintf("UNKNOWN(%d)", typ)
	}

	if a.DefaultCatalog, a.AllCatalogs, err = catalogs(ctx, c); err != nil {
		return engine.Analysis{}, wrap("duckdb.Analyze", err)
	}
	if !slices.Contains(explained, a.Kind) {
		return a, nil
	}
	// 'all' = plan logis pra-optimasi + teroptimasi + fisik: scan yang
	// dibuang optimizer (mis. parameter NULL → EMPTY_RESULT) tetap terlihat.
	if _, err := c.ExecContext(ctx, "SET explain_output = 'all'", nil); err != nil {
		return engine.Analysis{}, wrap("duckdb.Analyze", err)
	}
	plans, err := queryRows(ctx, c, "EXPLAIN (FORMAT json) "+query, make([]any, nparams))
	if err != nil {
		return engine.Analysis{}, wrap("duckdb.Analyze", err)
	}
	// DUCKLAKE_SCAN dan HNSW_INDEX_SCAN (ekstensi vss) menulis nama tabel
	// tanpa catalog/schema di EXPLAIN; nama lengkapnya diambil dari plan
	// terserialisasi (ADR 35, ADR 47).
	var lake shortNames
	for _, p := range plans {
		if lake.lake == nil && strings.Contains(p[1], `"DUCKLAKE_SCAN"`) {
			lake.lake = lakeTables(ctx, c, query)
		}
		if lake.index == nil && strings.Contains(p[1], `"HNSW_INDEX_SCAN"`) {
			lake.index = planTables(ctx, c, query)
		}
		if lake.sqlite == nil && strings.Contains(p[1], `"SQLITE_SCAN"`) {
			lake.sqlite = sqliteFiles(ctx, c, query)
		}
	}
	for _, p := range plans { // (explain_key, explain_value)
		var nodes []planNode
		if err := json.Unmarshal([]byte(p[1]), &nodes); err != nil {
			return engine.Analysis{}, fmt.Errorf("duckdb.Analyze: plan: %w", err)
		}
		for _, n := range nodes {
			w := walker{a: &a, lake: lake, logical: p[0] == "logical_plan"}
			w.walk(n, nil, false)
			if p[0] == "physical_plan" {
				columns(&a, n, lake)
			}
		}
	}
	for _, f := range a.Functions {
		if file, ok := strings.CutPrefix(f, sqliteUnattached); ok {
			return engine.Analysis{}, fmt.Errorf("duckdb.Analyze: %w: SQLite file %q is not an attached catalog (ATTACH it in --init-sql-file)", engine.ErrPermission, file)
		}
	}
	if a.Kind == "SELECT" && nparams == 0 {
		boundColumns(ctx, c, query, &a)
	}
	return a, nil
}

// boundColumns mengganti Columns dengan kolom yang dibaca tiap scan menurut
// plan logis teroptimasi (json_serialize_plan): termasuk kolom filter yang
// di-pushdown ke scan. Tabel yang dibuang optimizer (mis. EMPTY_RESULT dari
// statistik) memakai kolom yang direferensikan binder (plan tanpa optimasi).
// Gagal (sintaks yang tidak didukung) → Columns dari plan fisik tetap.
func boundColumns(ctx context.Context, c *duckdbdrv.Conn, query string, a *engine.Analysis) {
	opt, _, ok := planColumns(ctx, c, query, true)
	if !ok {
		return
	}
	var raw map[string][]string
	for _, t := range a.Tables {
		if _, seen := opt[t]; seen {
			continue
		}
		if raw == nil {
			if raw, _, ok = planColumns(ctx, c, query, false); !ok {
				return
			}
		}
		opt[t] = raw[t]
	}
	a.Columns = nil
	for t, l := range opt {
		if len(l) > 0 {
			if a.Columns == nil {
				a.Columns = map[string][]string{}
			}
			a.Columns[t] = l
		}
	}
}

// lakeTables nama tabel → nama lengkap "catalog.schema.table" setiap scan
// DuckLake di plan logis (dengan dan tanpa optimasi); nil bila plan tidak bisa
// diserialisasi (scan tetap tak terselesaikan → fail-closed).
func lakeTables(ctx context.Context, c *duckdbdrv.Conn, query string) map[string][]string {
	out := map[string][]string{}
	for _, optimize := range []bool{false, true} {
		_, lake, ok := planColumns(ctx, c, query, optimize)
		if !ok {
			return nil
		}
		for _, t := range lake {
			list := out[t[strings.LastIndexByte(t, '.')+1:]]
			addUnique(&list, t)
			out[t[strings.LastIndexByte(t, '.')+1:]] = list
		}
	}
	return out
}

// sqliteScans menyelesaikan SQLITE_SCAN (EXPLAIN: Table pendek + File;
// plan tidak bisa diserialisasi).
type sqliteScans struct {
	// files path file → catalog SQLite yang di-ATTACH.
	files map[string]string
	// direct: query memanggil sqlite_scan()/sqlite_query() langsung, atau AST
	// tidak tersedia (bukan SELECT). Node-nya identik dengan scan tabel
	// ter-attach, jadi scan dibiarkan tak teresolusi (hanya admin): rewrite
	// masking bekerja pada referensi tabel, bukan pemanggilan fungsi.
	direct bool
}

func sqliteFiles(ctx context.Context, c *duckdbdrv.Conn, query string) *sqliteScans {
	out := &sqliteScans{files: map[string]string{}, direct: true}
	if dbs, err := queryRows(ctx, c, "SELECT database_name, path FROM duckdb_databases() WHERE type = 'sqlite'", nil); err == nil {
		for _, d := range dbs {
			out.files[d[1]] = strings.ToLower(d[0])
		}
	}
	rows, err := queryRows(ctx, c, "SELECT json_serialize_sql($1::VARCHAR)::VARCHAR", []any{query})
	if err != nil || len(rows) != 1 {
		return out
	}
	var ast any
	if json.Unmarshal([]byte(rows[0][0]), &ast) != nil {
		return out
	}
	if m, ok := ast.(map[string]any); ok && m["error"] != true && !callsSQLiteFunction(ast) {
		out.direct = false
	}
	return out
}

// attached catalog untuk file (false bila bukan catalog SQLite ter-ATTACH).
func (s *sqliteScans) attached(file string) (string, bool) {
	if s == nil || file == "" {
		return "", false
	}
	cat, ok := s.files[file]
	return cat, ok
}

// sqliteUnattached penanda di Functions: SQLITE_SCAN atas file yang bukan
// catalog ter-ATTACH. Ekstensi sqlite membuka file lewat pustaka SQLite,
// tidak lewat filesystem DuckDB, sehingga allowed_directories (G-07) tidak
// berlaku — Analyze menolaknya untuk semua identitas.
const sqliteUnattached = "SQLITE_FILE_NOT_ATTACHED:"

// callsSQLiteFunction: AST memuat table function sqlite_* (sqlite_scan,
// sqlite_query, ...).
func callsSQLiteFunction(v any) bool {
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			if callsSQLiteFunction(e) {
				return true
			}
		}
	case map[string]any:
		if x["type"] == "TABLE_FUNCTION" {
			if fn, ok := x["function"].(map[string]any); ok {
				if name, _ := fn["function_name"].(string); strings.HasPrefix(strings.ToLower(name), "sqlite") {
					return true
				}
			}
		}
		for _, e := range x {
			if callsSQLiteFunction(e) {
				return true
			}
		}
	}
	return false
}

// planTables nama tabel → nama lengkap semua LOGICAL_GET di plan logis
// pra-optimasi. Scan indeks (HNSW) membaca salah satu tabel ini: optimizer
// tidak menambah tabel. (Plan teroptimasi vss gagal diserialisasi DuckDB.)
func planTables(ctx context.Context, c *duckdbdrv.Conn, query string) map[string][]string {
	cols, _, ok := planColumns(ctx, c, query, false)
	if !ok {
		return nil
	}
	out := map[string][]string{}
	for t := range cols {
		short := t[strings.LastIndexByte(t, '.')+1:]
		list := out[short]
		addUnique(&list, t)
		out[short] = list
	}
	return out
}

// planColumns tabel → kolom setiap LOGICAL_GET di json_serialize_plan, plus
// daftar tabel yang dibaca lewat ducklake_scan.
func planColumns(ctx context.Context, c *duckdbdrv.Conn, query string, optimize bool) (map[string][]string, []string, bool) {
	rows, err := queryRows(ctx, c, "SELECT json_serialize_plan($1::VARCHAR, optimize := $2::BOOLEAN)::VARCHAR", []any{query, optimize})
	if err != nil || len(rows) != 1 {
		return nil, nil, false
	}
	var root struct {
		Error bool `json:"error"`
		Plans any  `json:"plans"`
	}
	if json.Unmarshal([]byte(rows[0][0]), &root) != nil || root.Error {
		return nil, nil, false
	}
	cols := map[string][]string{}
	var lake []string
	var visit func(v any)
	visit = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, e := range x {
				visit(e)
			}
		case map[string]any:
			if x["type"] == "LOGICAL_GET" {
				fd, _ := x["function_data"].(map[string]any)
				key := func(k string) string { v, _ := fd[k].(string); return v }
				cat, sch, tbl := key("catalog"), key("schema"), key("table")
				isLake := x["name"] == "ducklake_scan"
				if isLake {
					cat, sch, tbl = key("catalog_name"), key("schema_name"), key("table_name")
				}
				names, _ := x["names"].([]any)
				idx, _ := x["column_indexes"].([]any)
				if cat != "" && sch != "" && tbl != "" {
					t := strings.ToLower(cat + "." + sch + "." + tbl)
					if isLake {
						addUnique(&lake, t)
					}
					list := cols[t]
					for _, ci := range idx {
						m, _ := ci.(map[string]any)
						// row id = 2^64-1: bandingkan sebelum konversi agar tidak meluap.
						if i, ok := m["index"].(float64); ok && i >= 0 && i < float64(len(names)) {
							if n, ok := names[int(i)].(string); ok {
								addUnique(&list, strings.ToLower(n))
							}
						}
					}
					cols[t] = list
				}
			}
			for _, e := range x {
				visit(e)
			}
		}
	}
	visit(root.Plans)
	return cols, lake, true
}

// walker mengumpulkan catalog/tabel/fungsi dari satu plan EXPLAIN.
type walker struct {
	a       *engine.Analysis
	lake    shortNames
	logical bool
}

// lakeWrites operator tulis DuckLake. COPY_TO_FILE di bawahnya adalah
// penulisan file data ke DATA_PATH catalog (dibatasi allowed_directories),
// bukan COPY milik pengguna (statement COPY tidak di-EXPLAIN).
var lakeWrites = map[string]bool{"DUCKLAKE_INSERT": true, "DUCKLAKE_UPDATE": true, "DUCKLAKE_DELETE": true,
	"DUCKLAKE_CREATE_TABLE_AS": true, "DUCKLAKE_INLINE_DATA": true}

// shortNames kandidat nama lengkap untuk scan yang hanya menulis nama tabel
// pendek di EXPLAIN: lake (DUCKLAKE_SCAN, lihat lakeTables), index
// (HNSW_INDEX_SCAN, lihat planTables), dan sqlite (SQLITE_SCAN: file →
// catalog, lihat sqliteFiles).
type shortNames struct {
	lake, index map[string][]string
	sqlite      *sqliteScans
}

// tableName nama "catalog.schema.table" dari extra_info Table; scan DuckLake
// dan indeks HNSW diselesaikan bila tepat satu kandidat (ambigu → tetap
// pendek → ditolak di walk).
func tableName(n planNode, table string, names shortNames) string {
	table = normTable(table)
	if strings.Contains(table, ".") {
		return table
	}
	var m map[string][]string
	switch n.Name {
	case "DUCKLAKE_SCAN":
		m = names.lake
	case "HNSW_INDEX_SCAN":
		m = names.index
	case "SQLITE_SCAN": // SQLite hanya punya schema main
		file, _ := n.ExtraInfo["File"].(string)
		if names.sqlite != nil && !names.sqlite.direct {
			if cat, ok := names.sqlite.files[file]; ok && file != "" {
				return cat + ".main." + table
			}
		}
	}
	if l := m[table]; len(l) == 1 {
		return l[0]
	}
	return table
}

// walk scan di plan logis pra-optimasi dicatat beserta ekspresi FILTER induk
// langsungnya (verifikasi rewrite).
func (w walker) walk(n planNode, parent *planNode, inLakeWrite bool) {
	a := w.a
	if n.Name == "SQLITE_SCAN" {
		file, _ := n.ExtraInfo["File"].(string)
		if _, ok := w.lake.sqlite.attached(file); !ok {
			addUnique(&a.Functions, sqliteUnattached+file)
		}
	}
	switch table, _ := n.ExtraInfo["Table"].(string); {
	case table != "":
		table = tableName(n, table, w.lake)
		addUnique(&a.Tables, table)
		if w.logical {
			sc := engine.Scan{Table: table}
			if parent != nil && parent.Name == "FILTER" {
				sc.ParentFilter = exprText(parent.ExtraInfo["Expressions"])
			}
			a.Scans = append(a.Scans, sc)
		}
		// "catalog.schema.table" tanpa kutip: cocokkan prefix dengan catalog
		// yang ada (nama catalog bisa mengandung titik); ambigu → semua.
		matched := false
		for _, cat := range a.AllCatalogs {
			if strings.HasPrefix(strings.ToLower(table), cat+".") {
				addUnique(&a.Catalogs, cat)
				matched = true
			}
		}
		if !matched { // tidak terduga: laporkan sebagai fungsi agar ditolak
			addUnique(&a.Functions, "UNRESOLVED_TABLE:"+table)
		}
	case lakeWrites[n.Name]:
		inLakeWrite = true
	case n.Name == "COPY_TO_FILE" && inLakeWrite:
	case !operators[n.Name]:
		addUnique(&a.Functions, n.Name)
	}
	for _, c := range n.Children {
		w.walk(c, &n, inLakeWrite)
	}
}

// normTable "catalog.schema.table" dari EXPLAIN (identifier yang perlu
// kutip, mis. keyword seperti "secret", ditulis ber-kutip) → bagian tanpa
// kutip, huruf kecil.
func normTable(s string) string {
	var parts []string
	var cur strings.Builder
	quoted := false
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"' && quoted && i+1 < len(s) && s[i+1] == '"':
			cur.WriteByte('"')
			i++
		case c == '"':
			quoted = !quoted
		case c == '.' && !quoted:
			parts = append(parts, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	parts = append(parts, cur.String())
	if quoted || len(parts) != 3 || slices.ContainsFunc(parts, func(p string) bool { return strings.Contains(p, ".") }) {
		return strings.ToLower(s) // titik di nama: ambigu, tidak didukung (fail-closed di walk)
	}
	return strings.ToLower(strings.Join(parts, "."))
}

// columns mencatat Projections setiap scan tabel di plan fisik.
func columns(a *engine.Analysis, n planNode, lake shortNames) {
	if table, _ := n.ExtraInfo["Table"].(string); table != "" {
		var cols []string
		switch p := n.ExtraInfo["Projections"].(type) {
		case string:
			if p != "" {
				cols = []string{p}
			}
		case []any:
			for _, c := range p {
				if s, ok := c.(string); ok {
					cols = append(cols, s)
				}
			}
		}
		table = tableName(n, table, lake)
		for _, c := range cols {
			if a.Columns == nil {
				a.Columns = map[string][]string{}
			}
			list := a.Columns[table]
			addUnique(&list, strings.ToLower(c))
			a.Columns[table] = list
		}
	}
	for _, c := range n.Children {
		columns(a, c, lake)
	}
}

func exprText(v any) string {
	switch e := v.(type) {
	case string:
		return e
	case []any:
		parts := make([]string, 0, len(e))
		for _, x := range e {
			s, _ := x.(string)
			parts = append(parts, s)
		}
		return strings.Join(parts, " AND ")
	}
	return ""
}

func addUnique(list *[]string, v string) {
	if !slices.Contains(*list, v) {
		*list = append(*list, v)
	}
}

// catalogs mengembalikan catalog default dan semua catalog (huruf kecil).
func catalogs(ctx context.Context, c *duckdbdrv.Conn) (string, []string, error) {
	def, err := queryRows(ctx, c, "SELECT lower(current_database())", nil)
	if err != nil || len(def) != 1 {
		return "", nil, errors.Join(errors.New("current_database"), err)
	}
	rows, err := queryRows(ctx, c, "SELECT lower(database_name) FROM duckdb_databases()", nil)
	all := make([]string, len(rows))
	for i, r := range rows {
		all[i] = r[0]
	}
	return def[0][0], all, err
}

// queryRows menjalankan satu statement (Prepare menolak multi-statement) dan
// mengembalikan setiap baris sebagai string (kolom non-string → "").
func queryRows(ctx context.Context, c *duckdbdrv.Conn, query string, args []any) ([][]string, error) {
	ds, err := c.Prepare(query)
	if err != nil {
		return nil, err
	}
	defer func() { _ = ds.Close() }()
	rows, err := ds.(driver.StmtQueryContext).QueryContext(ctx, namedArgs(args))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	dest := make([]driver.Value, len(rows.Columns()))
	var out [][]string
	for {
		if err := rows.Next(dest); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, err
		}
		row := make([]string, len(dest))
		for i, v := range dest {
			row[i], _ = v.(string)
		}
		out = append(out, row)
	}
}

// SerializeSQL lihat engine.Engine (AST JSON DuckDB; SELECT saja).
func (e *Engine) SerializeSQL(ctx context.Context, query string) (string, error) {
	return e.scalarString(ctx, "SELECT json_serialize_sql($1::VARCHAR)::VARCHAR", query)
}

// DeserializeSQL lihat engine.Engine.
func (e *Engine) DeserializeSQL(ctx context.Context, ast string) (string, error) {
	return e.scalarString(ctx, "SELECT json_deserialize_sql($1::JSON)", ast)
}

// Columns lihat engine.Engine.
func (e *Engine) Columns(ctx context.Context, catalog, schema, table string) ([]engine.Column, error) {
	c, err := e.conn(ctx)
	if err != nil {
		return nil, wrap("duckdb.Columns", err)
	}
	defer func() { _ = c.Close() }()
	rows, err := queryRows(ctx, c, `SELECT column_name, data_type FROM information_schema.columns
		WHERE lower(table_catalog) = lower($1) AND lower(table_schema) = lower($2) AND lower(table_name) = lower($3)
		ORDER BY ordinal_position`, []any{catalog, schema, table})
	if err != nil {
		return nil, wrap("duckdb.Columns", err)
	}
	cols := make([]engine.Column, len(rows))
	for i, r := range rows {
		cols[i] = engine.Column{Name: r[0], Type: r[1]}
	}
	return cols, nil
}

func (e *Engine) scalarString(ctx context.Context, query string, arg string) (string, error) {
	c, err := e.conn(ctx)
	if err != nil {
		return "", fmt.Errorf("duckdb: %w", err)
	}
	defer func() { _ = c.Close() }()
	rows, err := queryRows(ctx, c, query, []any{arg})
	if err != nil {
		return "", fmt.Errorf("duckdb: %w", err)
	}
	if len(rows) != 1 {
		return "", fmt.Errorf("duckdb: %d rows", len(rows))
	}
	return rows[0][0], nil
}
