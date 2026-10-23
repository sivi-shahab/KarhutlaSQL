package flightsql

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bankmega/karhutlasql/internal/audit"
	"github.com/bankmega/karhutlasql/internal/auth"
	"github.com/bankmega/karhutlasql/internal/authz"
	"github.com/bankmega/karhutlasql/internal/engine"
	"github.com/bankmega/karhutlasql/internal/sqlrewrite"
)

// safeFunctions table function yang tidak membaca data di luar catalog dan
// boleh dipakai tanpa izin admin. karhutla_* menegakkan izinnya sendiri.
var safeFunctions = []string{"RANGE", "GENERATE_SERIES", "UNNEST", "KARHUTLA_SESSIONS", "KARHUTLA_METRICS", "KARHUTLA_KILL_SESSION"}

// writeKinds statement yang target tulisnya ditentukan sqlrewrite.
var writeKinds = []string{"INSERT", "UPDATE", "DELETE", "CREATE", "DROP", "ALTER"}

// needs menurunkan izin yang dibutuhkan statement: read per catalog yang
// dibaca (binder DuckDB), write per catalog target (parser), admin global
// untuk operasi sistem dan table function selain safeFunctions.
func needs(a engine.Analysis, sql string) ([]authz.Need, error) {
	var out []authz.Need
	global := authz.Need{Catalog: authz.AnyCatalog, Action: authz.Admin}
	for _, c := range a.Catalogs {
		out = append(out, authz.Need{Catalog: c, Action: authz.Read})
	}
	for _, f := range a.Functions {
		if !slices.Contains(safeFunctions, f) {
			out = append(out, global)
			break
		}
	}
	switch {
	case a.Kind == "SELECT", a.Kind == "TRANSACTION": // BEGIN/COMMIT: koneksi per statement, no-op
	case slices.Contains(writeKinds, a.Kind):
		targets, err := sqlrewrite.WriteTargets(sql)
		if err != nil {
			return nil, err
		}
		for _, t := range targets {
			cats, err := t.Catalogs(a.DefaultCatalog, a.AllCatalogs)
			if err != nil {
				return nil, err
			}
			for _, c := range cats {
				out = append(out, authz.Need{Catalog: c, Action: authz.Write})
			}
		}
	default: // EXPLAIN, COPY, ATTACH, SET, PRAGMA, CALL, LOAD, EXPORT, PREPARE, ...
		out = append(out, global)
	}
	return out, nil
}

// authorize menegakkan policy untuk sql sebelum prepare/eksekusi.
func (s *Server) authorize(ctx context.Context, sql string) error {
	id, _ := auth.FromContext(ctx)
	a, err := s.eng.Analyze(ctx, sql)
	if err != nil {
		return toStatus(ctx, err)
	}
	ns, err := needs(a, sql)
	if err == nil {
		err = s.opts.Policy.Policy().Check(id.Roles, ns)
	}
	if err == nil {
		return nil
	}
	ev := audit.Event{Type: audit.AuthzDeny, User: id.Subject, Source: id.Source, Kind: a.Kind, Error: err.Error()}
	s.opts.Audit.SQL(&ev, sql)
	s.opts.Audit.Emit(ctx, ev)
	var de *authz.DenyError
	if errors.As(err, &de) {
		return status.Error(codes.PermissionDenied, de.Error())
	}
	return status.Errorf(codes.PermissionDenied, "permission denied: %v", err)
}

// readable melaporkan apakah pemanggil boleh membaca catalog (filter metadata).
func (s *Server) readable(ctx context.Context, catalog string) bool {
	id, _ := auth.FromContext(ctx)
	return s.opts.Policy.Policy().Check(id.Roles, []authz.Need{{Catalog: strings.ToLower(catalog), Action: authz.Read}}) == nil
}

// filterCatalogs membuang baris metadata (kolom 0 = catalog_name, semua kolom
// string) yang catalog-nya tidak boleh dibaca pemanggil.
func (s *Server) filterCatalogs(ctx context.Context, recs []arrow.RecordBatch) []arrow.RecordBatch {
	out := make([]arrow.RecordBatch, 0, len(recs))
	for _, rec := range recs {
		cat, ok := rec.Column(0).(*array.String)
		if !ok {
			continue // kontrak engine dilanggar → buang, jangan bocorkan
		}
		builders := make([]*array.StringBuilder, rec.NumCols())
		for i := range builders {
			builders[i] = array.NewStringBuilder(s.Alloc)
		}
		for r := range int(rec.NumRows()) {
			if cat.IsNull(r) || !s.readable(ctx, cat.Value(r)) {
				continue
			}
			for c, b := range builders {
				col, _ := rec.Column(c).(*array.String)
				if col == nil || col.IsNull(r) {
					b.AppendNull()
				} else {
					b.Append(col.Value(r))
				}
			}
		}
		cols := make([]arrow.Array, len(builders))
		for i, b := range builders {
			cols[i] = b.NewArray()
			b.Release()
		}
		out = append(out, array.NewRecordBatch(rec.Schema(), cols, int64(cols[0].Len())))
		for _, c := range cols {
			c.Release()
		}
	}
	return out
}
