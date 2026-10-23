// Package sqlrewrite mem-parse SQL untuk keputusan authz (dan, di Fase 5b,
// rewrite masking/RLS). Fail-closed: statement yang tidak bisa di-parse atau
// tipe yang tidak dikenali → error; tidak ada fallback regex.
package sqlrewrite

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	pg "github.com/pganalyze/pg_query_go/v6"
)

// Target objek yang ditulis statement: nama 1–3 bagian seperti ditulis
// (huruf kecil, DuckDB case-insensitive). Schema = objeknya schema itu
// sendiri (CREATE/DROP SCHEMA), bukan relasi.
type Target struct {
	Parts  []string
	Schema bool
	Temp   bool
}

// ErrUnsupported statement tulis yang targetnya tidak bisa ditentukan.
var ErrUnsupported = errors.New("cannot determine write target")

// WriteTargets mengembalikan target tulis statement DML/DDL (parser dialek
// Postgres; sintaks khusus DuckDB pada statement tulis → error).
func WriteTargets(sql string) ([]Target, error) {
	res, err := pg.Parse(sql)
	if err != nil {
		return nil, fmt.Errorf("sqlrewrite.WriteTargets: %w: %w", ErrUnsupported, err)
	}
	if len(res.GetStmts()) != 1 {
		return nil, fmt.Errorf("sqlrewrite.WriteTargets: %w: %d statements", ErrUnsupported, len(res.GetStmts()))
	}
	n := res.GetStmts()[0].GetStmt()
	rel := func(rv *pg.RangeVar) ([]Target, error) {
		if rv == nil {
			return nil, fmt.Errorf("sqlrewrite.WriteTargets: %w", ErrUnsupported)
		}
		return []Target{rangeVar(rv)}, nil
	}
	switch {
	case n.GetInsertStmt() != nil:
		return rel(n.GetInsertStmt().GetRelation())
	case n.GetUpdateStmt() != nil:
		return rel(n.GetUpdateStmt().GetRelation())
	case n.GetDeleteStmt() != nil:
		return rel(n.GetDeleteStmt().GetRelation())
	case n.GetMergeStmt() != nil:
		return rel(n.GetMergeStmt().GetRelation())
	case n.GetCreateStmt() != nil:
		return rel(n.GetCreateStmt().GetRelation())
	case n.GetCreateTableAsStmt() != nil:
		return rel(n.GetCreateTableAsStmt().GetInto().GetRel())
	case n.GetViewStmt() != nil:
		return rel(n.GetViewStmt().GetView())
	case n.GetCreateSeqStmt() != nil:
		return rel(n.GetCreateSeqStmt().GetSequence())
	case n.GetIndexStmt() != nil:
		return rel(n.GetIndexStmt().GetRelation())
	case n.GetAlterTableStmt() != nil:
		return rel(n.GetAlterTableStmt().GetRelation())
	case n.GetRenameStmt() != nil && n.GetRenameStmt().GetRelation() != nil:
		return rel(n.GetRenameStmt().GetRelation())
	case n.GetTruncateStmt() != nil:
		var out []Target
		for _, r := range n.GetTruncateStmt().GetRelations() {
			if r.GetRangeVar() == nil {
				return nil, fmt.Errorf("sqlrewrite.WriteTargets: %w", ErrUnsupported)
			}
			out = append(out, rangeVar(r.GetRangeVar()))
		}
		return out, nil
	case n.GetCreateSchemaStmt() != nil:
		return []Target{{Parts: []string{strings.ToLower(n.GetCreateSchemaStmt().GetSchemaname())}, Schema: true}}, nil
	case n.GetDropStmt() != nil:
		return dropTargets(n.GetDropStmt())
	}
	return nil, fmt.Errorf("sqlrewrite.WriteTargets: %w: unsupported statement", ErrUnsupported)
}

func rangeVar(rv *pg.RangeVar) Target {
	var parts []string
	for _, p := range []string{rv.GetCatalogname(), rv.GetSchemaname(), rv.GetRelname()} {
		if p != "" {
			parts = append(parts, strings.ToLower(p))
		}
	}
	return Target{Parts: parts, Temp: rv.GetRelpersistence() == "t"}
}

func dropTargets(d *pg.DropStmt) ([]Target, error) {
	schema := d.GetRemoveType() == pg.ObjectType_OBJECT_SCHEMA
	switch d.GetRemoveType() {
	case pg.ObjectType_OBJECT_TABLE, pg.ObjectType_OBJECT_VIEW, pg.ObjectType_OBJECT_SEQUENCE,
		pg.ObjectType_OBJECT_INDEX, pg.ObjectType_OBJECT_SCHEMA:
	default:
		return nil, fmt.Errorf("sqlrewrite.WriteTargets: %w: DROP %s", ErrUnsupported, d.GetRemoveType())
	}
	var out []Target
	for _, o := range d.GetObjects() {
		var parts []string
		switch {
		case o.GetString_() != nil:
			parts = []string{o.GetString_().GetSval()}
		case o.GetList() != nil:
			for _, it := range o.GetList().GetItems() {
				if it.GetString_() == nil {
					return nil, fmt.Errorf("sqlrewrite.WriteTargets: %w", ErrUnsupported)
				}
				parts = append(parts, it.GetString_().GetSval())
			}
		default:
			return nil, fmt.Errorf("sqlrewrite.WriteTargets: %w", ErrUnsupported)
		}
		for i := range parts {
			parts[i] = strings.ToLower(parts[i])
		}
		out = append(out, Target{Parts: parts, Schema: schema})
	}
	return out, nil
}

// Catalogs memetakan target ke catalog yang ditulis, mengikuti binder DuckDB
// (BindSchemaOrCatalog): nama 2 bagian "x.obj" → catalog x bila x ter-attach,
// selain itu schema x di catalog default; x yang sekaligus schema di catalog
// default ditolak DuckDB sebagai ambigu (dikunci TestBinderCatalogOrSchema).
func (t Target) Catalogs(defaultCatalog string, all []string) ([]string, error) {
	n := len(t.Parts)
	switch {
	case t.Temp:
		return []string{"temp"}, nil
	case t.Schema && n == 1, !t.Schema && n == 1:
		return []string{defaultCatalog}, nil
	case t.Schema && n == 2, !t.Schema && n == 3:
		return []string{t.Parts[0]}, nil
	case !t.Schema && n == 2:
		if slices.Contains(all, t.Parts[0]) {
			return []string{t.Parts[0]}, nil
		}
		return []string{defaultCatalog}, nil
	}
	return nil, fmt.Errorf("sqlrewrite.Catalogs: %w: name %q", ErrUnsupported, strings.Join(t.Parts, "."))
}
