package sqlrewrite

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// Codec AST JSON engine (DuckDB json_serialize_sql / json_deserialize_sql).
type Codec interface {
	SerializeSQL(ctx context.Context, sql string) (string, error)
	DeserializeSQL(ctx context.Context, ast string) (string, error)
}

// ErrRewrite query yang menyentuh tabel ter-govern tetapi tidak bisa
// di-rewrite; pemanggil menolak (fail-closed).
var ErrRewrite = errors.New("query cannot be rewritten for data governance")

// Names konteks resolusi nama tabel (aturan binder DuckDB).
type Names struct {
	DefaultCatalog string
	Catalogs       []string
}

// Resolve memetakan nama seperti ditulis ke "catalog.schema.table" (huruf
// kecil): "x.t" → catalog x (schema main) bila x ter-attach, selain itu schema
// x di catalog default; "t" → default.main.t.
func (n Names) Resolve(catalog, schema, table string) string {
	catalog, schema, table = strings.ToLower(catalog), strings.ToLower(schema), strings.ToLower(table)
	switch {
	case catalog != "":
	case schema != "" && slices.Contains(n.Catalogs, schema):
		catalog, schema = schema, "main"
	default:
		catalog = n.DefaultCatalog
	}
	if schema == "" {
		schema = "main"
	}
	return catalog + "." + schema + "." + table
}

// Substitute mengganti setiap referensi tabel yang ada di subs
// ("catalog.schema.table" → SQL subselect pengganti) dengan subquery beralias
// sama. Hanya SELECT (keterbatasan json_serialize_sql); sintaks yang tidak
// bisa diserialisasi → ErrRewrite. Mengembalikan SQL hasil dan jumlah
// substitusi. Kelengkapan substitusi diverifikasi pemanggil terhadap plan.
func Substitute(ctx context.Context, codec Codec, sql string, names Names, subs map[string]string) (string, int, error) {
	r := &rewriter{ctx: ctx, codec: codec, names: names, subs: subs, nodes: map[string]map[string]any{}}
	res, err := r.run(sql)
	return res, r.count, err
}

// run serialisasi sql → walk → deserialisasi.
func (r *rewriter) run(sql string) (string, error) {
	root, err := serialize(r.ctx, r.codec, sql)
	if err != nil {
		return "", err
	}
	stmts, _ := root["statements"].([]any)
	if len(stmts) != 1 {
		return "", fmt.Errorf("%w: %d statements", ErrRewrite, len(stmts))
	}
	out, err := r.walk(stmts[0], nil)
	if err != nil {
		return "", err
	}
	root["statements"] = []any{out}
	b, err := json.Marshal(root)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrRewrite, err)
	}
	res, err := r.codec.DeserializeSQL(r.ctx, string(b))
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrRewrite, err)
	}
	return res, nil
}

func serialize(ctx context.Context, codec Codec, sql string) (map[string]any, error) {
	js, err := codec.SerializeSQL(ctx, sql)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRewrite, err)
	}
	dec := json.NewDecoder(bytes.NewReader([]byte(js)))
	dec.UseNumber() // query_location bernilai uint64 max
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrRewrite, err)
	}
	if e, _ := root["error"].(bool); e {
		return nil, fmt.Errorf("%w: %v", ErrRewrite, root["error_message"])
	}
	return root, nil
}

type rewriter struct {
	ctx   context.Context
	codec Codec
	names Names
	subs  map[string]string
	nodes map[string]map[string]any // cache node SUBQUERY per tabel
	at    any                       // TimeTravel: at_clause untuk setiap tabel (tanpa substitusi)
	count int
}

// walk menelusuri AST generik; scope = nama CTE yang terlihat.
func (r *rewriter) walk(v any, scope []string) (any, error) {
	switch x := v.(type) {
	case []any:
		for i, e := range x {
			out, err := r.walk(e, scope)
			if err != nil {
				return nil, err
			}
			x[i] = out
		}
		return x, nil
	case map[string]any:
		if cm, ok := x["cte_map"].(map[string]any); ok {
			entries, _ := cm["map"].([]any)
			for _, e := range entries {
				if m, ok := e.(map[string]any); ok {
					if k, ok := m["key"].(string); ok {
						scope = append(slices.Clip(scope), strings.ToLower(k))
					}
				}
			}
		}
		if x["type"] == "BASE_TABLE" {
			return r.table(x, scope)
		}
		for k, e := range x {
			out, err := r.walk(e, scope)
			if err != nil {
				return nil, err
			}
			x[k] = out
		}
		return x, nil
	}
	return v, nil
}

func (r *rewriter) table(ref map[string]any, scope []string) (any, error) {
	str := func(k string) string { s, _ := ref[k].(string); return s }
	cat, sch, tbl := str("catalog_name"), str("schema_name"), str("table_name")
	if cat == "" && sch == "" && slices.Contains(scope, strings.ToLower(tbl)) {
		return ref, nil // referensi CTE
	}
	if r.at != nil {
		if ref["at_clause"] != nil {
			return nil, fmt.Errorf("%w: %s already has AT", ErrRewrite, tbl)
		}
		ref["at_clause"] = deepCopyAny(r.at)
		r.count++
		return ref, nil
	}
	fqn := r.names.Resolve(cat, sch, tbl)
	sub, ok := r.subs[fqn]
	if !ok {
		return ref, nil
	}
	node, err := r.subquery(fqn, sub)
	if err != nil {
		return nil, err
	}
	if at := ref["at_clause"]; at != nil { // time travel: pindah ke scan di dalam subquery
		n := 0
		visitTables(node, func(t map[string]any) {
			str := func(k string) string { s, _ := t[k].(string); return s }
			if r.names.Resolve(str("catalog_name"), str("schema_name"), str("table_name")) == fqn {
				t["at_clause"] = at
				n++
			}
		})
		if n == 0 {
			return nil, fmt.Errorf("%w: time travel (AT) on %s", ErrRewrite, fqn)
		}
	}
	alias := str("alias")
	if alias == "" {
		alias = tbl
	}
	node["alias"] = alias
	node["column_name_alias"] = ref["column_name_alias"]
	node["sample"] = ref["sample"]
	r.count++
	return node, nil
}

// subquery node SUBQUERY untuk sub, diambil dari serialisasi
// "SELECT * FROM (sub) AS x" (bentuk kanonik dari DuckDB sendiri).
func (r *rewriter) subquery(fqn, sub string) (map[string]any, error) {
	if n, ok := r.nodes[fqn]; ok {
		return deepCopy(n), nil
	}
	root, err := serialize(r.ctx, r.codec, "SELECT * FROM (\n"+sub+"\n) AS karhutla_sub")
	if err != nil {
		return nil, err
	}
	stmts, _ := root["statements"].([]any)
	st, _ := stmts[0].(map[string]any)
	node, _ := st["node"].(map[string]any)
	from, ok := node["from_table"].(map[string]any)
	if !ok || from["type"] != "SUBQUERY" {
		return nil, fmt.Errorf("%w: unexpected subquery AST", ErrRewrite)
	}
	r.nodes[fqn] = from
	return deepCopy(from), nil
}

// visitTables memanggil fn untuk setiap node BASE_TABLE di v.
func visitTables(v any, fn func(map[string]any)) {
	switch x := v.(type) {
	case []any:
		for _, e := range x {
			visitTables(e, fn)
		}
	case map[string]any:
		if x["type"] == "BASE_TABLE" {
			fn(x)
		}
		for _, e := range x {
			visitTables(e, fn)
		}
	}
}

// TimeTravel menambahkan "AT (at)" ke setiap referensi tabel di sql (selain
// CTE), mis. at = "VERSION => 5". at disusun pemanggil dari nilai bertipe,
// bukan input mentah. Referensi yang sudah ber-AT → ErrRewrite.
func TimeTravel(ctx context.Context, codec Codec, sql, at string) (string, error) {
	tmpl, err := serialize(ctx, codec, "SELECT 1 FROM t AT ("+at+")")
	if err != nil {
		return "", err
	}
	r := &rewriter{ctx: ctx, codec: codec}
	visitTables(tmpl["statements"], func(t map[string]any) { r.at = t["at_clause"] })
	if r.at == nil {
		return "", fmt.Errorf("%w: invalid AT clause", ErrRewrite)
	}
	return r.run(sql)
}

func deepCopyAny(v any) any {
	return deepCopy(map[string]any{"v": v})["v"]
}

func deepCopy(m map[string]any) map[string]any {
	b, _ := json.Marshal(m) // berasal dari json.Decode: selalu bisa di-marshal
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out map[string]any
	_ = dec.Decode(&out)
	return out
}
