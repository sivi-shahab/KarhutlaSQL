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
var explained = []string{"SELECT", "INSERT", "UPDATE", "DELETE", "CREATE", "DROP", "ALTER"}

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
		return engine.Analysis{}, fmt.Errorf("duckdb.Analyze: %w", err)
	}
	defer func() { _ = c.Close() }()

	ds, err := c.Prepare(query) // count != 1 → error, tidak ada yang dieksekusi
	if err != nil {
		return engine.Analysis{}, fmt.Errorf("duckdb.Analyze: %w", err)
	}
	st := ds.(*duckdbdrv.Stmt)
	typ, terr := st.StatementType()
	nparams := st.NumInput()
	if err := errors.Join(terr, ds.Close()); err != nil {
		return engine.Analysis{}, fmt.Errorf("duckdb.Analyze: %w", err)
	}
	a := engine.Analysis{Kind: kinds[typ]}
	if a.Kind == "" {
		a.Kind = fmt.Sprintf("UNKNOWN(%d)", typ)
	}

	if a.DefaultCatalog, a.AllCatalogs, err = catalogs(ctx, c); err != nil {
		return engine.Analysis{}, fmt.Errorf("duckdb.Analyze: %w", err)
	}
	if !slices.Contains(explained, a.Kind) {
		return a, nil
	}
	// 'all' = plan logis pra-optimasi + teroptimasi + fisik: scan yang
	// dibuang optimizer (mis. parameter NULL → EMPTY_RESULT) tetap terlihat.
	if _, err := c.ExecContext(ctx, "SET explain_output = 'all'", nil); err != nil {
		return engine.Analysis{}, fmt.Errorf("duckdb.Analyze: %w", err)
	}
	plans, err := queryRows(ctx, c, "EXPLAIN (FORMAT json) "+query, make([]any, nparams))
	if err != nil {
		return engine.Analysis{}, fmt.Errorf("duckdb.Analyze: %w", err)
	}
	for _, p := range plans { // (explain_key, explain_value)
		var nodes []planNode
		if err := json.Unmarshal([]byte(p[1]), &nodes); err != nil {
			return engine.Analysis{}, fmt.Errorf("duckdb.Analyze: plan: %w", err)
		}
		for _, n := range nodes {
			walk(&a, n, nil, p[0] == "logical_plan")
		}
	}
	return a, nil
}

// walk mengumpulkan catalog/tabel/fungsi; scan di plan logis pra-optimasi
// dicatat beserta ekspresi FILTER induk langsungnya (verifikasi rewrite).
func walk(a *engine.Analysis, n planNode, parent *planNode, logical bool) {
	switch table, _ := n.ExtraInfo["Table"].(string); {
	case table != "":
		table = strings.ToLower(table)
		addUnique(&a.Tables, table)
		if logical {
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
	case !operators[n.Name]:
		addUnique(&a.Functions, n.Name)
	}
	for _, c := range n.Children {
		walk(a, c, &n, logical)
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
		return nil, fmt.Errorf("duckdb.Columns: %w", err)
	}
	defer func() { _ = c.Close() }()
	rows, err := queryRows(ctx, c, `SELECT column_name, data_type FROM information_schema.columns
		WHERE lower(table_catalog) = lower($1) AND lower(table_schema) = lower($2) AND lower(table_name) = lower($3)
		ORDER BY ordinal_position`, []any{catalog, schema, table})
	if err != nil {
		return nil, fmt.Errorf("duckdb.Columns: %w", err)
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
