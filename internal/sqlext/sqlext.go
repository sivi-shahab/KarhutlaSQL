// Package sqlext mendaftarkan fungsi SQL internal karhutla_*() sebagai table
// function DuckDB. Identitas & sesi pemanggil dibaca dari ctx query (duckdb-go
// meneruskan ctx QueryContext ke UDF); tanpa identitas → ditolak (fail-closed).
package sqlext

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/bankmega/karhutlasql/internal/accesslog"
	"github.com/bankmega/karhutlasql/internal/audit"
	"github.com/bankmega/karhutlasql/internal/auth"
	"github.com/bankmega/karhutlasql/internal/classification"
	"github.com/bankmega/karhutlasql/internal/dq"
	"github.com/bankmega/karhutlasql/internal/quota"
	"github.com/bankmega/karhutlasql/internal/session"
	"github.com/bankmega/karhutlasql/internal/verified"
)

// ErrPermissionDenied awalan pesan error fungsi yang butuh role admin; lapisan
// Flight SQL memetakannya ke codes.PermissionDenied.
var ErrPermissionDenied = errors.New("permission denied")

// ErrFailedPrecondition awalan pesan error yang dipetakan ke
// codes.FailedPrecondition (mis. DQ gate report).
var ErrFailedPrecondition = errors.New("failed precondition")

// Deps sumber data fungsi.
type Deps struct {
	Sessions *session.Registry
	Gatherer prometheus.Gatherer
	Audit    *audit.Logger
	History  *session.History
	// AccessLog sumber karhutla_access_history(); nil = fungsi menolak.
	AccessLog *accesslog.Log
	// DQ sumber karhutla_dq_results(); nil = fungsi menolak.
	DQ *dq.Store
	// RunReport menjalankan report sebagai identitas di ctx (G-10); asOf
	// kosong = data terkini, selain itu versi/waktu snapshot DuckLake. Hasil
	// satu baris kolom reportCols. nil = fungsi menolak.
	RunReport func(ctx context.Context, name, asOf string) ([]any, error)
	// Quota pemakaian per tim untuk karhutla_chargeback(); nil = kosong.
	Quota *quota.Tracker
	// Verified verified queries & semantic model (Fase 8c); nil = kosong.
	Verified *verified.Set
	// MaskKey kunci HMAC-SHA256 untuk mask hash_hmac (G-03); kosong = fungsi
	// mask menolak semua pemanggilan.
	MaskKey []byte
}

type nonceKey struct{}

// WithMaskNonce menandai ctx query dengan nonce rewrite; karhutla_mask_hmac
// hanya menghitung bila argumen nonce sama (bukan oracle hash bagi user).
func WithMaskNonce(ctx context.Context, nonce string) context.Context {
	return context.WithValue(ctx, nonceKey{}, nonce)
}

// Register mendaftarkan fungsi karhutla_*() pada conn (berlaku untuk seluruh database).
func Register(_ context.Context, conn *sql.Conn, d Deps) error {
	fns := map[string]duckdb.RowTableFunction{
		"karhutla_sessions":         table(sessionCols, nil, nil, d.sessions),
		"karhutla_kill_session":     table(killCols, []duckdb.Type{duckdb.TYPE_VARCHAR}, nil, d.kill),
		"karhutla_metrics":          table(metricCols, nil, nil, d.metrics),
		"karhutla_query_history":    table(historyCols, nil, nil, d.history),
		"karhutla_access_history":   table(accessCols, nil, nil, d.access),
		"karhutla_dq_results":       table(dqCols, nil, nil, d.dqResults),
		"karhutla_chargeback":       table(chargebackCols, nil, nil, d.chargeback),
		"karhutla_verified_queries": table(verifiedCols, nil, nil, d.verifiedQueries),
		"karhutla_semantic_model":   table([]col{{"semantic_model", duckdb.TYPE_VARCHAR}}, nil, nil, d.semanticModel),
		// Nama terpisah: duckdb-go gagal bila named argument tidak diberikan dan
		// DuckDB mengabaikan registrasi overload kedua.
		"karhutla_report_run":           table(reportCols, []duckdb.Type{duckdb.TYPE_VARCHAR}, nil, d.report),
		"karhutla_report_run_at":        table(reportCols, []duckdb.Type{duckdb.TYPE_VARCHAR, duckdb.TYPE_VARCHAR}, nil, d.report),
		"karhutla_access_history_range": table(accessCols, []duckdb.Type{duckdb.TYPE_DATE, duckdb.TYPE_DATE}, nil, d.access),
	}
	for name, f := range fns {
		if err := duckdb.RegisterTableUDF(conn, name, f); err != nil {
			return fmt.Errorf("sqlext.Register: %s: %w", name, err)
		}
	}
	if err := duckdb.RegisterScalarUDF(conn, classification.HMACFunc, hmacUDF{key: d.MaskKey}); err != nil {
		return fmt.Errorf("sqlext.Register: %s: %w", classification.HMACFunc, err)
	}
	return nil
}

// hmacUDF karhutla_mask_hmac(value VARCHAR, nonce VARCHAR) → hex HMAC-SHA256.
type hmacUDF struct{ key []byte }

func (hmacUDF) Config() duckdb.ScalarFuncConfig {
	v, _ := duckdb.NewTypeInfo(duckdb.TYPE_VARCHAR) // tipe primitif: tidak pernah error
	return duckdb.ScalarFuncConfig{InputTypeInfos: []duckdb.TypeInfo{v, v}, ResultTypeInfo: v}
}

func (h hmacUDF) Executor() duckdb.ScalarFuncExecutor {
	return duckdb.ScalarFuncExecutor{RowContextExecutor: func(ctx context.Context, values []driver.Value) (any, error) {
		want, _ := ctx.Value(nonceKey{}).(string)
		got, _ := values[1].(string)
		if len(h.key) == 0 || want == "" || subtle.ConstantTimeCompare([]byte(want), []byte(got)) != 1 {
			return nil, fmt.Errorf("%w: %s is internal", ErrPermissionDenied, classification.HMACFunc)
		}
		v, _ := values[0].(string)
		m := hmac.New(sha256.New, h.key)
		m.Write([]byte(v))
		return hex.EncodeToString(m.Sum(nil)), nil
	}}
}

type col struct {
	name string
	t    duckdb.Type
}

var (
	sessionCols = []col{
		{"session_id", duckdb.TYPE_VARCHAR}, {"user", duckdb.TYPE_VARCHAR}, {"source", duckdb.TYPE_VARCHAR},
		{"peer", duckdb.TYPE_VARCHAR}, {"started_at", duckdb.TYPE_TIMESTAMP_TZ}, {"last_active_at", duckdb.TYPE_TIMESTAMP_TZ},
		{"active_statements", duckdb.TYPE_INTEGER}, {"statement", duckdb.TYPE_VARCHAR},
		{"statement_started_at", duckdb.TYPE_TIMESTAMP_TZ}, {"rows_sent", duckdb.TYPE_BIGINT},
		{"bytes_sent", duckdb.TYPE_BIGINT}, {"current", duckdb.TYPE_BOOLEAN},
	}
	historyCols = []col{
		{"request_id", duckdb.TYPE_VARCHAR}, {"session_id", duckdb.TYPE_VARCHAR}, {"user", duckdb.TYPE_VARCHAR},
		{"kind", duckdb.TYPE_VARCHAR}, {"resource_group", duckdb.TYPE_VARCHAR}, {"statement", duckdb.TYPE_VARCHAR},
		{"status", duckdb.TYPE_VARCHAR}, {"error", duckdb.TYPE_VARCHAR}, {"started_at", duckdb.TYPE_TIMESTAMP_TZ},
		{"queue_ms", duckdb.TYPE_BIGINT}, {"duration_ms", duckdb.TYPE_BIGINT}, {"rows", duckdb.TYPE_BIGINT},
		{"bytes", duckdb.TYPE_BIGINT}, {"peak_memory_bytes", duckdb.TYPE_BIGINT}, {"plan_hash", duckdb.TYPE_VARCHAR},
	}
	accessCols = []col{
		{"ts", duckdb.TYPE_TIMESTAMP_TZ}, {"request_id", duckdb.TYPE_VARCHAR}, {"session_id", duckdb.TYPE_VARCHAR},
		{"user", duckdb.TYPE_VARCHAR}, {"source", duckdb.TYPE_VARCHAR}, {"kind", duckdb.TYPE_VARCHAR},
		{"action", duckdb.TYPE_VARCHAR}, {"status", duckdb.TYPE_VARCHAR}, {"rows", duckdb.TYPE_BIGINT},
		{"table_name", duckdb.TYPE_VARCHAR}, {"column_name", duckdb.TYPE_VARCHAR}, {"tag", duckdb.TYPE_VARCHAR},
		{"policy", duckdb.TYPE_VARCHAR}, {"row_filter", duckdb.TYPE_VARCHAR}, {"sql_sha256", duckdb.TYPE_VARCHAR},
		{"rewritten_sql_sha256", duckdb.TYPE_VARCHAR}, {"sql", duckdb.TYPE_VARCHAR},
	}
	// ReportCols kolom karhutla_report_run(name).
	reportCols = []col{
		{"report_id", duckdb.TYPE_VARCHAR}, {"name", duckdb.TYPE_VARCHAR}, {"snapshot_id", duckdb.TYPE_VARCHAR},
		{"rows", duckdb.TYPE_BIGINT}, {"location", duckdb.TYPE_VARCHAR}, {"result_sha256", duckdb.TYPE_VARCHAR},
		{"manifest_sha256", duckdb.TYPE_VARCHAR},
	}
	dqCols = []col{
		{"table_name", duckdb.TYPE_VARCHAR}, {"suite", duckdb.TYPE_VARCHAR}, {"run_id", duckdb.TYPE_VARCHAR},
		{"validation_id", duckdb.TYPE_VARCHAR}, {"snapshot_id", duckdb.TYPE_VARCHAR}, {"success", duckdb.TYPE_BOOLEAN},
		{"evaluated", duckdb.TYPE_BIGINT}, {"failed_critical", duckdb.TYPE_BIGINT}, {"failed_warning", duckdb.TYPE_BIGINT},
		{"validated_at", duckdb.TYPE_TIMESTAMP_TZ}, {"run_by", duckdb.TYPE_VARCHAR}, {"results", duckdb.TYPE_VARCHAR},
	}
	killCols = []col{{"session_id", duckdb.TYPE_VARCHAR}, {"killed", duckdb.TYPE_BOOLEAN}}
	// verifiedCols karhutla_verified_queries() (Fase 8c).
	verifiedCols = []col{{"name", duckdb.TYPE_VARCHAR}, {"question", duckdb.TYPE_VARCHAR}, {"sql", duckdb.TYPE_VARCHAR},
		{"verified_by", duckdb.TYPE_VARCHAR}, {"verified_at", duckdb.TYPE_TIMESTAMP_TZ}}
	// chargebackCols karhutla_chargeback(): jendela kuota berjalan + total sejak start.
	chargebackCols = []col{{"team", duckdb.TYPE_VARCHAR}, {"window_start", duckdb.TYPE_TIMESTAMP_TZ},
		{"queries", duckdb.TYPE_BIGINT}, {"exec_seconds", duckdb.TYPE_DOUBLE}, {"rows", duckdb.TYPE_BIGINT}, {"bytes", duckdb.TYPE_BIGINT},
		{"total_queries", duckdb.TYPE_BIGINT}, {"total_exec_seconds", duckdb.TYPE_DOUBLE}, {"total_rows", duckdb.TYPE_BIGINT}, {"total_bytes", duckdb.TYPE_BIGINT}}
	metricCols = []col{{"name", duckdb.TYPE_VARCHAR}, {"labels", duckdb.TYPE_VARCHAR}, {"type", duckdb.TYPE_VARCHAR}, {"value", duckdb.TYPE_DOUBLE}}
)

// rowsFunc menghasilkan baris dari argumen posisi & bernama.
type rowsFunc func(ctx context.Context, args []any, named map[string]any) ([][]any, error)

// table membuat table function yang barisnya dihitung saat eksekusi (FillRow
// pertama), bukan saat bind — prepare/describe tidak memicu efek samping.
func table(cols []col, args []duckdb.Type, named map[string]duckdb.Type, rows rowsFunc) duckdb.RowTableFunction {
	var argInfo []duckdb.TypeInfo
	for _, t := range args {
		ti, _ := duckdb.NewTypeInfo(t) // tipe primitif: tidak pernah error
		argInfo = append(argInfo, ti)
	}
	namedInfo := map[string]duckdb.TypeInfo{}
	for n, t := range named {
		namedInfo[n], _ = duckdb.NewTypeInfo(t) // tipe primitif: tidak pernah error
	}
	return duckdb.RowTableFunction{
		Config: duckdb.TableFunctionConfig{Arguments: argInfo, NamedArguments: namedInfo},
		BindArgumentsContext: func(ctx context.Context, named map[string]any, args ...any) (duckdb.RowTableSource, error) {
			infos := make([]duckdb.ColumnInfo, len(cols))
			for i, c := range cols {
				ti, err := duckdb.NewTypeInfo(c.t)
				if err != nil {
					return nil, err
				}
				infos[i] = duckdb.ColumnInfo{Name: c.name, T: ti}
			}
			return &source{ctx: ctx, args: args, named: named, cols: infos, rows: rows}, nil
		},
	}
}

type source struct {
	ctx   context.Context //nolint:containedctx // ctx query, hidup selama eksekusi
	args  []any
	named map[string]any
	cols  []duckdb.ColumnInfo
	rows  rowsFunc

	data [][]any
	done bool
}

func (s *source) ColumnInfos() []duckdb.ColumnInfo     { return s.cols }
func (s *source) Cardinality() *duckdb.CardinalityInfo { return nil }
func (s *source) Init()                                {}

func (s *source) FillRow(row duckdb.Row) (bool, error) {
	if !s.done {
		var err error
		if s.data, err = s.rows(s.ctx, s.args, s.named); err != nil {
			return false, err
		}
		s.done = true
	}
	if len(s.data) == 0 {
		return false, nil
	}
	for i, v := range s.data[0] {
		if !row.IsProjected(i) {
			continue
		}
		if err := row.SetRowValue(i, v); err != nil {
			return false, err
		}
	}
	s.data = s.data[1:]
	return true, nil
}

func identity(ctx context.Context) (auth.Identity, error) {
	id, ok := auth.FromContext(ctx)
	if !ok {
		return id, fmt.Errorf("%w: no identity", ErrPermissionDenied)
	}
	return id, nil
}

func ts(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// sessions: admin melihat semua sesi, user lain hanya sesinya sendiri. Teks
// SQL mengikuti --audit-log-sql (default hash).
func (d Deps) sessions(ctx context.Context, _ []any, _ map[string]any) ([][]any, error) {
	id, err := identity(ctx)
	if err != nil {
		return nil, err
	}
	var out [][]any
	for _, s := range d.Sessions.Snapshot() {
		if !id.HasRole(auth.RoleAdmin) && s.User != id.Subject {
			continue
		}
		var stmt any
		if s.SQL != "" {
			stmt = d.Audit.Redact(s.SQL)
		}
		out = append(out, []any{s.ID, s.User, s.Source, s.Peer, ts(s.StartedAt), ts(s.LastActive),
			int32(min(s.ActiveStatements, math.MaxInt32)), stmt, ts(s.SQLStartedAt), s.Rows, s.Bytes, s.ID == id.TokenID}) //nolint:gosec // dibatasi min
	}
	return out, nil
}

// history: statement terakhir (G-09), tertua dulu; admin melihat semua,
// user lain hanya miliknya. Teks SQL sudah disamarkan saat dicatat.
func (d Deps) history(ctx context.Context, _ []any, _ map[string]any) ([][]any, error) {
	id, err := identity(ctx)
	if err != nil {
		return nil, err
	}
	var out [][]any
	for _, q := range d.History.Snapshot() {
		if !id.HasRole(auth.RoleAdmin) && q.User != id.Subject {
			continue
		}
		var e any
		if q.Error != "" {
			e = q.Error
		}
		var plan any
		if q.PlanHash != "" {
			plan = q.PlanHash
		}
		out = append(out, []any{q.RequestID, q.SessionID, q.User, q.Kind, q.Group, q.SQL, q.Status, e, ts(q.StartedAt),
			q.QueueWait.Milliseconds(), q.Duration.Milliseconds(), q.Rows, q.Bytes, q.PeakMemory, plan})
	}
	return out, nil
}

// access: access-history (G-05) [since, until] (DATE UTC inklusif; tanpa
// argumen: 7 hari terakhir s.d. sekarang); admin melihat semua, user lain
// hanya miliknya.
func (d Deps) access(ctx context.Context, args []any, _ map[string]any) ([][]any, error) {
	id, err := identity(ctx)
	if err != nil {
		return nil, err
	}
	if d.AccessLog == nil {
		return nil, errors.New("karhutla_access_history: access history is not configured (--archive-dir / --archive-s3-*)")
	}
	now := time.Now()
	since, until := now.AddDate(0, 0, -7), now.Add(time.Minute)
	if len(args) == 2 {
		s, ok1 := args[0].(time.Time)
		u, ok2 := args[1].(time.Time)
		if !ok1 || !ok2 {
			return nil, errors.New("karhutla_access_history_range(since, until): both dates are required")
		}
		since, until = s, u.AddDate(0, 0, 1) // inklusif
	}
	recs, err := d.AccessLog.Query(ctx, since, until)
	if err != nil {
		return nil, fmt.Errorf("karhutla_access_history: %w", err)
	}
	var out [][]any
	for _, r := range recs {
		if !id.HasRole(auth.RoleAdmin) && (r.User != id.Subject || r.Source != id.Source) {
			continue
		}
		out = append(out, r.Values())
	}
	return out, nil
}

// dqResults: karhutla_dq_results() — semua hasil validasi GX tersanitasi
// (tanpa nilai baris, Q-04); terbuka untuk semua user terautentikasi.
func (d Deps) dqResults(ctx context.Context, _ []any, _ map[string]any) ([][]any, error) {
	if _, err := identity(ctx); err != nil {
		return nil, err
	}
	if d.DQ == nil {
		return nil, errors.New("karhutla_dq_results: data quality store is not configured (--archive-dir / --archive-s3-*)")
	}
	if err := d.DQ.Refresh(ctx); err != nil { // publish dari instance lain
		return nil, fmt.Errorf("karhutla_dq_results: %w", err)
	}
	var out [][]any
	for _, v := range d.DQ.All() {
		out = append(out, []any{v.Table, v.Suite, v.RunID, v.ID, v.SnapshotID, v.Success, v.Evaluated, v.FailedCritical,
			v.FailedWarning, v.ValidatedAt, v.RunBy, v.Results})
	}
	return out, nil
}

// report: karhutla_report_run(name) / karhutla_report_run_at(name, as_of) —
// dijalankan saat eksekusi (FillRow), bukan bind; izin & DQ gate ditegakkan
// RunReport.
func (d Deps) report(ctx context.Context, args []any, _ map[string]any) ([][]any, error) {
	if _, err := identity(ctx); err != nil {
		return nil, err
	}
	if d.RunReport == nil {
		return nil, fmt.Errorf("%w: report run is not configured", ErrFailedPrecondition)
	}
	name, _ := args[0].(string)
	var asOf string
	if len(args) > 1 {
		if asOf, _ = args[1].(string); asOf == "" {
			return nil, fmt.Errorf("%w: karhutla_report_run_at: as_of is required", ErrFailedPrecondition)
		}
	}
	row, err := d.RunReport(ctx, name, asOf)
	if err != nil {
		return nil, fmt.Errorf("karhutla_report_run: %w", err)
	}
	return [][]any{row}, nil
}

func (d Deps) kill(ctx context.Context, args []any, _ map[string]any) ([][]any, error) {
	id, err := identity(ctx)
	if err != nil {
		return nil, err
	}
	if !id.HasRole(auth.RoleAdmin) {
		return nil, fmt.Errorf("%w: karhutla_kill_session requires role %s", ErrPermissionDenied, auth.RoleAdmin)
	}
	target, _ := args[0].(string)
	if target == id.TokenID {
		return nil, errors.New("karhutla_kill_session: cannot kill the current session")
	}
	killed, err := d.Sessions.Kill(ctx, target)
	if err != nil {
		d.Audit.Emit(ctx, audit.Event{Type: audit.SessionKilled, User: id.Subject, Source: id.Source, Target: target, Via: "sql", Status: "PARTIAL", Error: err.Error()})
		return nil, fmt.Errorf("karhutla_kill_session: %w", err)
	}
	if killed {
		d.Audit.Emit(ctx, audit.Event{Type: audit.SessionKilled, User: id.Subject, Source: id.Source, Target: target, Via: "sql"})
	}
	return [][]any{{target, killed}}, nil
}

// metrics meratakan snapshot gatherer: histogram → _bucket{le}/_sum/_count,
// summary → {quantile}/_sum/_count.
// verifiedQueries: verified queries (semua user; SQL dieksekusi sebagai pemanggil).
func (d Deps) verifiedQueries(ctx context.Context, _ []any, _ map[string]any) ([][]any, error) {
	if _, err := identity(ctx); err != nil {
		return nil, err
	}
	var out [][]any
	if d.Verified != nil {
		for _, q := range d.Verified.List() {
			out = append(out, []any{q.Name, q.Question, q.SQL, q.VerifiedBy, q.VerifiedAt})
		}
	}
	return out, nil
}

// semanticModel: satu baris teks semantic model (kosong bila tidak ada).
func (d Deps) semanticModel(ctx context.Context, _ []any, _ map[string]any) ([][]any, error) {
	if _, err := identity(ctx); err != nil {
		return nil, err
	}
	if d.Verified == nil {
		return [][]any{{""}}, nil
	}
	return [][]any{{d.Verified.SemanticModel}}, nil
}

// chargeback: pemakaian per tim; admin melihat semua tim, user lain hanya timnya.
func (d Deps) chargeback(ctx context.Context, _ []any, _ map[string]any) ([][]any, error) {
	id, err := identity(ctx)
	if err != nil {
		return nil, err
	}
	if d.Quota == nil {
		return nil, nil
	}
	team := d.Quota.Team(id)
	var out [][]any
	for _, r := range d.Quota.Rows() {
		if !id.HasRole(auth.RoleAdmin) && r.Team != team {
			continue
		}
		out = append(out, []any{r.Team, ts(r.WindowStart), r.Window.Queries, r.Window.ExecSeconds, r.Window.Rows, r.Window.Bytes,
			r.Total.Queries, r.Total.ExecSeconds, r.Total.Rows, r.Total.Bytes})
	}
	return out, nil
}

func (d Deps) metrics(ctx context.Context, _ []any, _ map[string]any) ([][]any, error) {
	if _, err := identity(ctx); err != nil {
		return nil, err
	}
	mfs, err := d.Gatherer.Gather()
	if err != nil {
		return nil, fmt.Errorf("karhutla_metrics: %w", err)
	}
	var out [][]any
	for _, mf := range mfs {
		name, typ := mf.GetName(), mf.GetType().String()
		for _, m := range mf.GetMetric() {
			add := func(suffix string, v float64, extra ...string) {
				labels := map[string]string{}
				for _, lp := range m.GetLabel() {
					labels[lp.GetName()] = lp.GetValue()
				}
				for i := 0; i+1 < len(extra); i += 2 {
					labels[extra[i]] = extra[i+1]
				}
				b, _ := json.Marshal(labels) // map[string]string selalu bisa di-marshal
				out = append(out, []any{name + suffix, string(b), typ, v})
			}
			switch mf.GetType() {
			case dto.MetricType_COUNTER:
				add("", m.GetCounter().GetValue())
			case dto.MetricType_GAUGE:
				add("", m.GetGauge().GetValue())
			case dto.MetricType_UNTYPED:
				add("", m.GetUntyped().GetValue())
			case dto.MetricType_HISTOGRAM, dto.MetricType_GAUGE_HISTOGRAM:
				h := m.GetHistogram()
				for _, b := range h.GetBucket() {
					add("_bucket", float64(b.GetCumulativeCount()), "le", fmt.Sprint(b.GetUpperBound()))
				}
				add("_sum", h.GetSampleSum())
				add("_count", float64(h.GetSampleCount()))
			case dto.MetricType_SUMMARY:
				sm := m.GetSummary()
				for _, q := range sm.GetQuantile() {
					add("", q.GetValue(), "quantile", fmt.Sprint(q.GetQuantile()))
				}
				add("_sum", sm.GetSampleSum())
				add("_count", float64(sm.GetSampleCount()))
			}
		}
	}
	return out, nil
}
