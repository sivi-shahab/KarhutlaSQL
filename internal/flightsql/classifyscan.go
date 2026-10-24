package flightsql

import (
	"context"
	"fmt"
	"strings"

	"github.com/bankmega/karhutlasql/internal/audit"
	"github.com/bankmega/karhutlasql/internal/auth"
	"github.com/bankmega/karhutlasql/internal/classification"
	"github.com/bankmega/karhutlasql/internal/classify"
	"github.com/bankmega/karhutlasql/internal/engine"
	"github.com/bankmega/karhutlasql/internal/sqlext"
)

const (
	// classifySampleRows sampel reservoir per tabel. ponytail: reservoir
	// memindai seluruh tabel; ganti ke sampling system bila tabel sangat besar.
	classifySampleRows = 1000
	// classifyMinRatio porsi nilai sampel yang harus cocok pola.
	classifyMinRatio = 0.8
)

// ClassifyScan karhutla_classify_scan() (admin): sampel kolom teks/angka di
// semua tabel dasar → usulan tag PII (classify.Rules) per kolom, dengan tag
// yang sudah berlaku. Hanya rasio cocok yang keluar, bukan nilai sampel.
// Tabel yang gagal dibaca (mis. catalog eksternal mati) dilewati dan dihitung
// di audit.
func (s *Server) ClassifyScan(ctx context.Context) ([][]any, error) {
	id, _ := auth.FromContext(ctx)
	if !id.HasRole(auth.RoleAdmin) {
		return nil, fmt.Errorf("%w: karhutla_classify_scan requires role %s", sqlext.ErrPermissionDenied, auth.RoleAdmin)
	}
	tables, err := s.baseTables(ctx)
	if err != nil {
		return nil, fmt.Errorf("karhutla_classify_scan: %w", err)
	}
	cls := s.opts.Classification.Get()
	var out [][]any
	skipped := 0
	for _, t := range tables {
		rows, err := s.classifyTable(ctx, t, cls)
		if err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("karhutla_classify_scan: %w", ctx.Err())
			}
			skipped++
			continue
		}
		out = append(out, rows...)
	}
	s.opts.Audit.Emit(ctx, audit.Event{Type: audit.ClassificationScan, User: id.Subject, Source: id.Source,
		Rows: int64(len(out)), Detail: fmt.Sprintf("tables=%d skipped=%d", len(tables), skipped)})
	return out, nil
}

// baseTables tabel dasar milik user ([catalog, schema, table]).
func (s *Server) baseTables(ctx context.Context) ([][3]string, error) {
	rdr, err := s.eng.Tables(ctx, engine.TableFilter{TableTypes: []string{"BASE TABLE"}})
	if err != nil {
		return nil, err
	}
	defer rdr.Release()
	var out [][3]string
	for rdr.Next() {
		rec := rdr.RecordBatch()
		for i := range int(rec.NumRows()) {
			t := [3]string{strings.Clone(rec.Column(0).ValueStr(i)), strings.Clone(rec.Column(1).ValueStr(i)), strings.Clone(rec.Column(2).ValueStr(i))}
			if t[0] == "system" || t[0] == "temp" {
				continue
			}
			out = append(out, t)
		}
	}
	return out, rdr.Err()
}

// scannable: tipe yang bisa memuat PII teks/angka (NIK sering BIGINT).
func scannable(typ string) bool {
	switch {
	case strings.HasPrefix(typ, "VARCHAR"), strings.HasPrefix(typ, "DECIMAL"):
		return true
	}
	switch typ {
	case "BIGINT", "UBIGINT", "HUGEINT", "UHUGEINT":
		return true
	}
	return false
}

func (s *Server) classifyTable(ctx context.Context, t [3]string, cls *classification.Classification) ([][]any, error) {
	cols, err := s.eng.Columns(ctx, t[0], t[1], t[2])
	if err != nil {
		return nil, err
	}
	var scan []engine.Column
	sel := make([]string, 0, len(cols))
	for _, c := range cols {
		if scannable(c.Type) {
			scan = append(scan, c)
			sel = append(sel, "CAST("+classification.Ident(c.Name)+" AS VARCHAR)")
		}
	}
	values := make([][]string, len(scan))
	if len(scan) > 0 {
		q := fmt.Sprintf("SELECT %s FROM %s.%s.%s USING SAMPLE %d ROWS", strings.Join(sel, ", "),
			classification.Ident(t[0]), classification.Ident(t[1]), classification.Ident(t[2]), classifySampleRows)
		rdr, err := s.eng.Query(ctx, q)
		if err != nil {
			return nil, err
		}
		for rdr.Next() {
			rec := rdr.RecordBatch()
			for j := range scan {
				c := rec.Column(j)
				for i := range int(rec.NumRows()) {
					if !c.IsNull(i) {
						if v := c.ValueStr(i); v != "" {
							values[j] = append(values[j], strings.Clone(v))
						}
					}
				}
			}
		}
		err = rdr.Err()
		rdr.Release()
		if err != nil {
			return nil, err
		}
	}
	table := strings.ToLower(t[0] + "." + t[1] + "." + t[2])
	tags := cls.TableTags(table)
	var out [][]any
	for _, c := range cols {
		var vals []string
		for j, sc := range scan {
			if sc.Name == c.Name {
				vals = values[j]
			}
		}
		sg, ok := classify.Suggest(c.Name, vals, classifyMinRatio)
		if !ok {
			continue
		}
		var current any
		if tag, ok := tags[strings.ToLower(c.Name)]; ok {
			current = tag
		}
		out = append(out, []any{table, c.Name, c.Type, sg.Tag, sg.Ratio, int64(sg.Sampled), sg.Reason, current})
	}
	return out, nil
}
