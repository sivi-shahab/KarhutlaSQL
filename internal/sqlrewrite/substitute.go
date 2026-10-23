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
	root, err := serialize(ctx, codec, sql)
	if err != nil {
		return "", 0, err
	}
	stmts, _ := root["statements"].([]any)
	if len(stmts) != 1 {
		return "", 0, fmt.Errorf("%w: %d statements", ErrRewrite, len(stmts))
	}
	r := &rewriter{ctx: ctx, codec: codec, names: names, subs: subs, nodes: map[string]map[string]any{}}
	out, err := r.walk(stmts[0], nil)
	if err != nil {
		return "", 0, err
	}
	root["statements"] = []any{out}
	b, err := json.Marshal(root)
	if err != nil {
		return "", 0, fmt.Errorf("%w: %w", ErrRewrite, err)
	}
	res, err := codec.DeserializeSQL(ctx, string(b))
	if err != nil {
		return "", 0, fmt.Errorf("%w: %w", ErrRewrite, err)
	}
	return res, r.count, nil
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
	fqn := r.names.Resolve(cat, sch, tbl)
	sub, ok := r.subs[fqn]
	if !ok {
		return ref, nil
	}
	if ref["at_clause"] != nil {
		return nil, fmt.Errorf("%w: time travel (AT) on %s", ErrRewrite, fqn)
	}
	node, err := r.subquery(fqn, sub)
	if err != nil {
		return nil, err
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

func deepCopy(m map[string]any) map[string]any {
	b, _ := json.Marshal(m) // berasal dari json.Decode: selalu bisa di-marshal
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out map[string]any
	_ = dec.Decode(&out)
	return out
}
