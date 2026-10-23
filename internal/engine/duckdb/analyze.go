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
	plans, err := queryStrings(ctx, c, "EXPLAIN (FORMAT json) "+query, make([]any, nparams), 1)
	if err != nil {
		return engine.Analysis{}, fmt.Errorf("duckdb.Analyze: %w", err)
	}
	for _, p := range plans {
		var nodes []planNode
		if err := json.Unmarshal([]byte(p), &nodes); err != nil {
			return engine.Analysis{}, fmt.Errorf("duckdb.Analyze: plan: %w", err)
		}
		for _, n := range nodes {
			walk(&a, n)
		}
	}
	return a, nil
}

func walk(a *engine.Analysis, n planNode) {
	switch table, _ := n.ExtraInfo["Table"].(string); {
	case table != "":
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
		walk(a, c)
	}
}

func addUnique(list *[]string, v string) {
	if !slices.Contains(*list, v) {
		*list = append(*list, v)
	}
}

// catalogs mengembalikan catalog default dan semua catalog (huruf kecil).
func catalogs(ctx context.Context, c *duckdbdrv.Conn) (string, []string, error) {
	def, err := queryStrings(ctx, c, "SELECT lower(current_database())", nil, 0)
	if err != nil || len(def) != 1 {
		return "", nil, errors.Join(errors.New("current_database"), err)
	}
	all, err := queryStrings(ctx, c, "SELECT lower(database_name) FROM duckdb_databases()", nil, 0)
	return def[0], all, err
}

// queryStrings menjalankan satu statement (Prepare menolak multi-statement)
// dan mengembalikan kolom col setiap baris sebagai string.
func queryStrings(ctx context.Context, c *duckdbdrv.Conn, query string, args []any, col int) ([]string, error) {
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
	var out []string
	for {
		if err := rows.Next(dest); err != nil {
			if errors.Is(err, io.EOF) {
				return out, nil
			}
			return nil, err
		}
		v, _ := dest[col].(string)
		out = append(out, v)
	}
}
