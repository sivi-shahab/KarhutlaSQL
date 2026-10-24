//go:build duckdb_arrow

package duckdb

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Deterministic lihat engine.Engine: setiap fungsi di plan terikat tanpa
// optimasi (view & macro sudah diekspansi) ber-stability CONSISTENT di semua
// overload skalar/agregat-nya. Fungsi yang tidak dikenal atau plan yang tidak bisa
// diserialisasi → false.
func (e *Engine) Deterministic(ctx context.Context, query string) (bool, error) {
	c, err := e.conn(ctx)
	if err != nil {
		return false, wrap("duckdb.Deterministic", err)
	}
	defer func() { _ = c.Close() }()
	e.consistentOnce.Do(func() {
		var rows [][]string
		// Table function/macro senama (mis. repeat) tidak relevan: macro sudah
		// diekspansi binder, table function terlihat di Analysis.Functions.
		rows, e.consistentErr = queryRows(ctx, c, `SELECT lower(function_name) FROM duckdb_functions()
			WHERE function_type NOT IN ('table', 'table_macro', 'macro', 'pragma')
			GROUP BY 1 HAVING bool_and(coalesce(stability, '') = 'CONSISTENT')`, nil)
		e.consistent = map[string]bool{}
		for _, r := range rows {
			e.consistent[r[0]] = true
		}
	})
	if e.consistentErr != nil {
		return false, wrap("duckdb.Deterministic", e.consistentErr)
	}
	rows, err := queryRows(ctx, c, "SELECT json_serialize_plan($1::VARCHAR, optimize := false)::VARCHAR", []any{query})
	if err != nil || len(rows) != 1 {
		return false, nil //nolint:nilerr // tidak bisa dianalisis = tidak deterministik
	}
	var root struct {
		Error bool `json:"error"`
		Plans any  `json:"plans"`
	}
	if json.Unmarshal([]byte(rows[0][0]), &root) != nil || root.Error {
		return false, nil
	}
	ok := true
	var visit func(v any)
	visit = func(v any) {
		switch x := v.(type) {
		case []any:
			for _, el := range x {
				visit(el)
			}
		case map[string]any:
			if cls, _ := x["expression_class"].(string); cls == "BOUND_FUNCTION" || cls == "BOUND_AGGREGATE" || cls == "BOUND_WINDOW" {
				name, _ := x["name"].(string)
				if fn, isMap := x["function"].(map[string]any); name == "" && isMap {
					name, _ = fn["name"].(string)
				}
				ok = ok && e.consistent[strings.ToLower(name)]
			}
			for _, el := range x {
				visit(el)
			}
		}
	}
	visit(root.Plans)
	return ok, nil
}

// DataVersion lihat engine.Engine: "catalog=local" untuk catalog DuckDB
// (hanya berubah lewat proses ini) dan "catalog=<snapshot id>" untuk
// DuckLake; tipe lain (mis. postgres, sqlite) → ok false.
func (e *Engine) DataVersion(ctx context.Context, catalogs []string) (string, bool, error) {
	c, err := e.conn(ctx)
	if err != nil {
		return "", false, wrap("duckdb.DataVersion", err)
	}
	defer func() { _ = c.Close() }()
	rows, err := queryRows(ctx, c, "SELECT lower(database_name), type FROM duckdb_databases()", nil)
	if err != nil {
		return "", false, wrap("duckdb.DataVersion", err)
	}
	types := map[string]string{}
	for _, r := range rows {
		types[r[0]] = r[1]
	}
	cats := slices.Clone(catalogs)
	slices.Sort(cats)
	parts := make([]string, 0, len(cats))
	for _, cat := range slices.Compact(cats) {
		switch types[cat] {
		case "duckdb":
			parts = append(parts, cat+"=local")
		case "ducklake":
			v, err := queryRows(ctx, c, fmt.Sprintf("SELECT id::VARCHAR FROM %s.current_snapshot()", quoteIdent(cat)), nil)
			if err != nil || len(v) != 1 {
				return "", false, fmt.Errorf("duckdb.DataVersion: %s: %w", cat, err)
			}
			parts = append(parts, cat+"="+v[0][0])
		default:
			return "", false, nil
		}
	}
	return strings.Join(parts, ","), true, nil
}
