// Package sqlext mendaftarkan fungsi SQL internal karhutla_*() sebagai table
// function DuckDB. Identitas & sesi pemanggil dibaca dari ctx query (duckdb-go
// meneruskan ctx QueryContext ke UDF); tanpa identitas → ditolak (fail-closed).
package sqlext

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	duckdb "github.com/duckdb/duckdb-go/v2"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"github.com/bankmega/karhutlasql/internal/audit"
	"github.com/bankmega/karhutlasql/internal/auth"
	"github.com/bankmega/karhutlasql/internal/session"
)

// ErrPermissionDenied awalan pesan error fungsi yang butuh role admin; lapisan
// Flight SQL memetakannya ke codes.PermissionDenied.
var ErrPermissionDenied = errors.New("permission denied")

// Deps sumber data fungsi.
type Deps struct {
	Sessions *session.Registry
	Gatherer prometheus.Gatherer
	Audit    *audit.Logger
}

// Register mendaftarkan karhutla_sessions(), karhutla_kill_session(id) dan
// karhutla_metrics() pada conn (berlaku untuk seluruh database).
func Register(_ context.Context, conn *sql.Conn, d Deps) error {
	fns := map[string]duckdb.RowTableFunction{
		"karhutla_sessions":     table(sessionCols, nil, d.sessions),
		"karhutla_kill_session": table(killCols, []duckdb.Type{duckdb.TYPE_VARCHAR}, d.kill),
		"karhutla_metrics":      table(metricCols, nil, d.metrics),
	}
	for name, f := range fns {
		if err := duckdb.RegisterTableUDF(conn, name, f); err != nil {
			return fmt.Errorf("sqlext.Register: %s: %w", name, err)
		}
	}
	return nil
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
	killCols   = []col{{"session_id", duckdb.TYPE_VARCHAR}, {"killed", duckdb.TYPE_BOOLEAN}}
	metricCols = []col{{"name", duckdb.TYPE_VARCHAR}, {"labels", duckdb.TYPE_VARCHAR}, {"type", duckdb.TYPE_VARCHAR}, {"value", duckdb.TYPE_DOUBLE}}
)

// table membuat table function yang barisnya dihitung saat eksekusi (FillRow
// pertama), bukan saat bind — prepare/describe tidak memicu efek samping.
func table(cols []col, args []duckdb.Type, rows func(ctx context.Context, args []any) ([][]any, error)) duckdb.RowTableFunction {
	var argInfo []duckdb.TypeInfo
	for _, t := range args {
		ti, _ := duckdb.NewTypeInfo(t) // tipe primitif: tidak pernah error
		argInfo = append(argInfo, ti)
	}
	return duckdb.RowTableFunction{
		Config: duckdb.TableFunctionConfig{Arguments: argInfo},
		BindArgumentsContext: func(ctx context.Context, _ map[string]any, args ...any) (duckdb.RowTableSource, error) {
			infos := make([]duckdb.ColumnInfo, len(cols))
			for i, c := range cols {
				ti, err := duckdb.NewTypeInfo(c.t)
				if err != nil {
					return nil, err
				}
				infos[i] = duckdb.ColumnInfo{Name: c.name, T: ti}
			}
			return &source{ctx: ctx, args: args, cols: infos, rows: rows}, nil
		},
	}
}

type source struct {
	ctx  context.Context //nolint:containedctx // ctx query, hidup selama eksekusi
	args []any
	cols []duckdb.ColumnInfo
	rows func(context.Context, []any) ([][]any, error)

	data [][]any
	done bool
}

func (s *source) ColumnInfos() []duckdb.ColumnInfo     { return s.cols }
func (s *source) Cardinality() *duckdb.CardinalityInfo { return nil }
func (s *source) Init()                                {}

func (s *source) FillRow(row duckdb.Row) (bool, error) {
	if !s.done {
		var err error
		if s.data, err = s.rows(s.ctx, s.args); err != nil {
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
func (d Deps) sessions(ctx context.Context, _ []any) ([][]any, error) {
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

func (d Deps) kill(ctx context.Context, args []any) ([][]any, error) {
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
	killed := d.Sessions.Kill(target)
	if killed {
		d.Audit.Emit(ctx, audit.Event{Type: audit.SessionKilled, User: id.Subject, Source: id.Source, Target: target, Via: "sql"})
	}
	return [][]any{{target, killed}}, nil
}

// metrics meratakan snapshot gatherer: histogram → _bucket{le}/_sum/_count,
// summary → {quantile}/_sum/_count.
func (d Deps) metrics(ctx context.Context, _ []any) ([][]any, error) {
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
