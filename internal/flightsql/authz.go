package flightsql

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bankmega/karhutlasql/internal/accesslog"
	"github.com/bankmega/karhutlasql/internal/audit"
	"github.com/bankmega/karhutlasql/internal/auth"
	"github.com/bankmega/karhutlasql/internal/authz"
	"github.com/bankmega/karhutlasql/internal/classification"
	"github.com/bankmega/karhutlasql/internal/engine"
	"github.com/bankmega/karhutlasql/internal/sqlrewrite"
)

// safeFunctions table function yang tidak membaca data di luar catalog dan
// boleh dipakai tanpa izin admin. karhutla_* menegakkan izinnya sendiri.
var safeFunctions = []string{"RANGE", "GENERATE_SERIES", "UNNEST", "KARHUTLA_SESSIONS", "KARHUTLA_METRICS", "KARHUTLA_KILL_SESSION", "KARHUTLA_QUERY_HISTORY", "KARHUTLA_ACCESS_HISTORY", "KARHUTLA_ACCESS_HISTORY_RANGE", "KARHUTLA_REPORT_RUN", "KARHUTLA_DQ_RESULTS"}

// writeKinds statement yang target tulisnya ditentukan sqlrewrite.
var writeKinds = []string{"INSERT", "UPDATE", "DELETE", "CREATE", "DROP", "ALTER"}

// plan hasil otorisasi: SQL yang benar-benar dieksekusi dan batasnya.
type plan struct {
	sql    string // SQL asli, atau hasil rewrite masking/RLS/LIMIT
	nonce  string // nonce rewrite (ctx karhutla_mask_hmac); "" bila tidak ada
	limits authz.Limits
	// final analisis SQL yang benar-benar dieksekusi (kolom yang dibaca, G-05).
	final engine.Analysis
	// tags/masks: tabel → kolom → tag / mask yang diterapkan; filters: tabel →
	// row filter yang diterapkan; writes: objek target tulis.
	tags, masks map[string]map[string]string
	filters     map[string]string
	writes      []string
	// control statement admin yang hanya memakai fungsi karhutla_* tanpa
	// tabel: tidak antre di resource group (kill/monitoring tetap jalan saat
	// antrean penuh). Hanya admin: fungsi skalar berat tidak terlihat di
	// Analysis.Functions.
	control bool
}

func names(a engine.Analysis) sqlrewrite.Names {
	return sqlrewrite.Names{DefaultCatalog: a.DefaultCatalog, Catalogs: a.AllCatalogs}
}

// needs menurunkan izin yang dibutuhkan statement: read per tabel yang dibaca
// (binder DuckDB), write per objek target (parser), admin global untuk
// operasi sistem dan table function selain safeFunctions.
func needs(a engine.Analysis, sql string) ([]authz.Need, error) {
	var out []authz.Need
	global := authz.Need{Object: authz.AnyCatalog, Action: authz.Admin}
	for _, t := range a.Tables {
		out = append(out, authz.Need{Object: t, Action: authz.Read})
	}
	for _, f := range a.Functions {
		if !slices.Contains(safeFunctions, f) {
			out = append(out, global)
			break
		}
	}
	switch {
	case a.Kind == "SELECT", a.Kind == "TRANSACTION": // BEGIN/COMMIT: koneksi per statement, no-op
	case a.Kind == "CALL" && len(a.Functions) > 0: // fungsi non-safe sudah butuh admin di atas
	case slices.Contains(writeKinds, a.Kind):
		targets, err := sqlrewrite.WriteTargets(sql)
		if err != nil {
			return nil, err
		}
		for _, t := range targets {
			obj, err := t.Object(names(a))
			if err != nil {
				return nil, err
			}
			out = append(out, authz.Need{Object: obj, Action: authz.Write})
		}
	default: // EXPLAIN, COPY, ATTACH, SET, PRAGMA, CALL, LOAD, EXPORT, PREPARE, ...
		out = append(out, global)
	}
	return out, nil
}

func control(a engine.Analysis) bool {
	if (a.Kind != "SELECT" && a.Kind != "CALL") || len(a.Tables) > 0 || len(a.Functions) == 0 {
		return false
	}
	for _, f := range a.Functions {
		if !strings.HasPrefix(f, "KARHUTLA_") || f == "KARHUTLA_REPORT_RUN" { // report = beban berat: antre
			return false
		}
	}
	return true
}

// deny mencatat authz_deny dan mengembalikan PERMISSION_DENIED.
func (s *Server) deny(ctx context.Context, kind, sql string, err error) error {
	id, _ := auth.FromContext(ctx)
	ev := audit.Event{Type: audit.AuthzDeny, User: id.Subject, Source: id.Source, Kind: kind, Error: err.Error()}
	s.opts.Audit.SQL(&ev, sql)
	s.opts.Audit.Emit(ctx, ev)
	var de *authz.DenyError
	if errors.As(err, &de) {
		return status.Error(codes.PermissionDenied, de.Error())
	}
	return status.Errorf(codes.PermissionDenied, "permission denied: %v", err)
}

// authorize menegakkan policy untuk sql sebelum prepare/eksekusi dan
// mengembalikan SQL yang harus dieksekusi (masking/RLS/LIMIT, G-02…G-06).
func (s *Server) authorize(ctx context.Context, sql string) (plan, error) {
	id, _ := auth.FromContext(ctx)
	a, err := s.eng.Analyze(ctx, sql)
	if err != nil {
		return plan{}, toStatus(ctx, err)
	}
	p := s.opts.Policy.Policy()
	pl := plan{sql: sql, limits: p.LimitsFor(id.Roles), control: id.HasRole(auth.RoleAdmin) && control(a)}
	if pl.limits.SelectOnly && a.Kind != "SELECT" {
		return plan{}, s.deny(ctx, a.Kind, sql, errors.New("role is SELECT-only"))
	}
	ns, err := needs(a, sql)
	if err == nil {
		err = p.Check(id.Roles, ns)
	}
	if err != nil {
		return plan{}, s.deny(ctx, a.Kind, sql, err)
	}
	for _, n := range ns {
		if n.Action == authz.Write {
			pl.writes = append(pl.writes, n.Object)
		}
	}
	pl.final = a
	if err := s.govern(ctx, &pl, a, p, id); err != nil {
		return plan{}, s.deny(ctx, a.Kind, sql, err)
	}
	if pl.limits.MaxRows > 0 && a.Kind == "SELECT" {
		wrapped := fmt.Sprintf("SELECT * FROM (\n%s\n) LIMIT %d", pl.sql, pl.limits.MaxRows)
		// Pastikan pembungkus tetap satu SELECT (mis. sql diakhiri ";").
		wa, err := s.eng.Analyze(ctx, wrapped)
		if err == nil && wa.Kind != "SELECT" {
			err = errors.New("not a single SELECT")
		}
		if err != nil {
			return plan{}, s.deny(ctx, a.Kind, sql, fmt.Errorf("cannot apply row limit: %w", err))
		}
		pl.sql, pl.final = wrapped, wa
	}
	return pl, nil
}

// govern mengganti setiap tabel yang ber-mask/RLS bagi identitas dengan
// subquery terlindung lalu memverifikasi lewat plan DuckDB bahwa tidak ada
// scan tabel itu di luar subquery (fail-closed: view, sintaks tak didukung,
// referensi terlewat → tolak).
func (s *Server) govern(ctx context.Context, pl *plan, a engine.Analysis, p *authz.Policy, id auth.Identity) error {
	cls := s.opts.Classification.Get()
	nonce := newNonce()
	guard := classification.Literal(nonce) + " = " + classification.Literal(nonce)
	subs := map[string]string{}
	pl.tags, pl.masks, pl.filters = map[string]map[string]string{}, map[string]map[string]string{}, map[string]string{}
	for _, t := range a.Tables {
		tags := cls.TableTags(t)
		pl.tags[t] = tags
		masked := map[string]classification.Mask{}
		for col, tag := range tags {
			if !p.Unmasked(id.Roles, tag) {
				masked[col] = cls.Tags[tag]
			}
		}
		pred, filtered, err := p.RowFilter(id.Roles, t, id.Attrs)
		if err != nil {
			return err
		}
		if len(masked) == 0 && !filtered {
			continue
		}
		sub, err := s.subselect(ctx, t, masked, pred, guard, nonce)
		if err != nil {
			return err
		}
		subs[t] = sub
		pl.masks[t] = map[string]string{}
		for col, m := range masked {
			pl.masks[t][col] = m.Mask
		}
		if filtered {
			pl.filters[t] = pred
		}
	}
	if len(subs) == 0 {
		return nil
	}
	if a.Kind != "SELECT" {
		return fmt.Errorf("%s reads masked or row-filtered data; only SELECT can be rewritten", a.Kind)
	}
	out, _, err := sqlrewrite.Substitute(ctx, s.eng, pl.sql, names(a), subs)
	if err != nil {
		return fmt.Errorf("%w (use standard SQL on governed tables)", err)
	}
	ra, err := s.eng.Analyze(ctx, out)
	if err != nil {
		return fmt.Errorf("%w: rewritten query invalid: %w", sqlrewrite.ErrRewrite, err)
	}
	for _, sc := range ra.Scans {
		if _, governed := subs[sc.Table]; governed && !strings.Contains(sc.ParentFilter, nonce) {
			return fmt.Errorf("%w: %s is referenced in a way that bypasses masking/row filter (e.g. through a view)", sqlrewrite.ErrRewrite, sc.Table)
		}
	}
	for _, t := range ra.Tables {
		if !slices.Contains(a.Tables, t) {
			return fmt.Errorf("%w: rewrite changed table set (%s)", sqlrewrite.ErrRewrite, t)
		}
	}
	pl.sql, pl.nonce, pl.final = out, nonce, ra
	s.auditGovernance(ctx, pl)
	return nil
}

// auditGovernance mencatat mask_applied & row_filter_applied per tabel.
func (s *Server) auditGovernance(ctx context.Context, pl *plan) {
	id, _ := auth.FromContext(ctx)
	for _, t := range slices.Sorted(maps.Keys(pl.masks)) {
		if len(pl.masks[t]) > 0 {
			var parts []string
			for _, col := range slices.Sorted(maps.Keys(pl.masks[t])) {
				parts = append(parts, col+":"+pl.masks[t][col])
			}
			s.opts.Audit.Emit(ctx, audit.Event{Type: audit.MaskApplied, User: id.Subject, Source: id.Source, Object: t, Detail: strings.Join(parts, ",")})
		}
		if f, ok := pl.filters[t]; ok {
			s.opts.Audit.Emit(ctx, audit.Event{Type: audit.RowFilterApplied, User: id.Subject, Source: id.Source, Object: t, Detail: f})
		}
	}
}

// access record access-history (G-05) statement yang dieksekusi: satu per
// kolom yang dibaca (tabel tanpa kolom → satu record tanpa kolom) dan satu
// per objek target tulis.
func (pl plan) access(base accesslog.Record) []accesslog.Record {
	var out []accesslog.Record
	for _, t := range pl.final.Tables {
		r := base
		r.Action, r.Table, r.RowFilter = "read", t, pl.filters[t]
		cols := pl.final.Columns[t]
		if len(cols) == 0 {
			out = append(out, r)
		}
		for _, c := range cols {
			r.Column, r.Tag, r.Policy = c, pl.tags[t][c], pl.masks[t][c]
			if r.Tag != "" && r.Policy == "" {
				r.Policy = "unmasked"
			}
			out = append(out, r)
		}
	}
	for _, w := range pl.writes {
		r := base
		r.Action, r.Table = "write", w
		out = append(out, r)
	}
	return out
}

// subselect SELECT pengganti tabel: semua kolom (yang bertag di-mask), row
// filter, dan guard nonce di FILTER tepat di atas scan.
func (s *Server) subselect(ctx context.Context, table string, masked map[string]classification.Mask, pred, guard, nonce string) (string, error) {
	parts := strings.Split(table, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("%w: table name %q", sqlrewrite.ErrRewrite, table)
	}
	cols, err := s.eng.Columns(ctx, parts[0], parts[1], parts[2])
	if err == nil && len(cols) == 0 {
		err = errors.New("no columns")
	}
	if err != nil {
		return "", fmt.Errorf("%w: columns of %s: %w", sqlrewrite.ErrRewrite, table, err)
	}
	exprs := make([]string, len(cols))
	for i, c := range cols {
		ident := classification.Ident(c.Name)
		if m, ok := masked[strings.ToLower(c.Name)]; ok {
			exprs[i] = m.Expr(ident, c.Type, nonce) + " AS " + ident
		} else {
			exprs[i] = ident
		}
	}
	if pred == "" {
		pred = "TRUE"
	}
	return fmt.Sprintf("SELECT %s FROM %s.%s.%s WHERE (%s) AND (%s)", strings.Join(exprs, ", "),
		classification.Ident(parts[0]), classification.Ident(parts[1]), classification.Ident(parts[2]), pred, guard), nil
}

func newNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b) // crypto/rand.Read tidak pernah gagal (Go ≥ 1.24)
	return "kg" + hex.EncodeToString(b)
}

// visible melaporkan apakah objek metadata boleh tampil bagi pemanggil.
func (s *Server) visible(ctx context.Context, object string) bool {
	id, _ := auth.FromContext(ctx)
	return s.opts.Policy.Policy().Visible(id.Roles, strings.ToLower(object))
}

// filterObjects membuang baris metadata (semua kolom string) yang objeknya
// — gabungan depth kolom pertama: catalog / catalog.schema /
// catalog.schema.table — tidak boleh dilihat pemanggil.
func (s *Server) filterObjects(ctx context.Context, recs []arrow.RecordBatch, depth int) []arrow.RecordBatch {
	out := make([]arrow.RecordBatch, 0, len(recs))
	for _, rec := range recs {
		cols := make([]*array.String, rec.NumCols())
		for i := range cols {
			cols[i], _ = rec.Column(i).(*array.String)
		}
		builders := make([]*array.StringBuilder, rec.NumCols())
		for i := range builders {
			builders[i] = array.NewStringBuilder(s.Alloc)
		}
		for r := range int(rec.NumRows()) {
			key := make([]string, 0, depth)
			for i := 0; i < depth && i < len(cols); i++ {
				if cols[i] == nil || cols[i].IsNull(r) {
					break
				}
				key = append(key, cols[i].Value(r))
			}
			if len(key) != depth || !s.visible(ctx, strings.Join(key, ".")) {
				continue // kontrak engine dilanggar atau tidak boleh dilihat → buang
			}
			for c, b := range builders {
				if cols[c] == nil || cols[c].IsNull(r) {
					b.AppendNull()
				} else {
					b.Append(cols[c].Value(r))
				}
			}
		}
		arrs := make([]arrow.Array, len(builders))
		for i, b := range builders {
			arrs[i] = b.NewArray()
			b.Release()
		}
		out = append(out, array.NewRecordBatch(rec.Schema(), arrs, int64(arrs[0].Len())))
		for _, a := range arrs {
			a.Release()
		}
	}
	return out
}
