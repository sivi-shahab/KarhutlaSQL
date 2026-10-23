package flightsql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/bankmega/karhutlasql/internal/accesslog"
	"github.com/bankmega/karhutlasql/internal/audit"
	"github.com/bankmega/karhutlasql/internal/auth"
	"github.com/bankmega/karhutlasql/internal/classification"
	"github.com/bankmega/karhutlasql/internal/engine"
	"github.com/bankmega/karhutlasql/internal/report"
	"github.com/bankmega/karhutlasql/internal/sqlext"
)

// ReportRun hasil satu report run (baris karhutla_report_run()).
type ReportRun struct {
	Manifest                     report.Manifest
	Prefix                       string // reports/<name>/<report_id>/
	ResultSHA256, ManifestSHA256 string
}

// RunReport menjalankan report name sebagai identitas di ctx (G-10): SQL
// report & assert di-otorisasi seperti statement biasa (masking/RLS berlaku),
// lalu dieksekusi dalam satu transaksi engine: sidik tabel sumber, DQ gate
// (assert), hasil Parquet. Manifest ditandatangani Ed25519 dan semuanya
// disimpan di arsip. Error memakai awalan sqlext agar terpetakan ke kode gRPC.
func (s *Server) RunReport(ctx context.Context, name string) (ReportRun, error) {
	id, _ := auth.FromContext(ctx)
	start := time.Now().UTC()
	m := report.Manifest{ReportID: start.Format("20060102T150405Z") + "-" + uuid.NewString()[:8], Name: name, User: id.Subject,
		Source: id.Source, StartedAt: start, Tables: map[string]string{}, DQValidates: []string{}}
	ev := audit.Event{Type: audit.ReportRun, User: id.Subject, Source: id.Source, Object: name, Detail: m.ReportID}

	def, ok := s.opts.Reports[name]
	var err error
	switch {
	case !ok:
		err = fmt.Errorf("%w: unknown report %q", sqlext.ErrFailedPrecondition, name)
	case s.opts.Signer == nil || s.opts.Archive == nil:
		err = fmt.Errorf("%w: report run requires --report-signing-key-file and an archive", sqlext.ErrFailedPrecondition)
	case !slices.ContainsFunc(def.Roles, id.HasRole):
		err = fmt.Errorf("%w: report %s requires one of roles %v", sqlext.ErrPermissionDenied, name, def.Roles)
	}
	m.SQLSHA256 = report.SHA256([]byte(def.SQL))
	var run ReportRun
	if err == nil {
		run, err = s.runReport(ctx, def, &m)
	}
	if err != nil {
		ev.Status, ev.Error = "FAILED", err.Error()
		switch {
		case errors.Is(err, sqlext.ErrFailedPrecondition):
			ev.Status = "FailedPrecondition"
		case errors.Is(err, sqlext.ErrPermissionDenied):
			ev.Status = "PermissionDenied"
		}
		s.opts.Audit.Emit(ctx, ev)
		return ReportRun{}, err
	}
	ev.Status, ev.Rows, ev.Hash = "OK", m.Result.Rows, run.ManifestSHA256
	s.opts.Audit.Emit(ctx, ev)
	return run, nil
}

func (s *Server) runReport(ctx context.Context, def report.Def, m *report.Manifest) (ReportRun, error) {
	id, _ := auth.FromContext(ctx)
	pl, err := s.authorize(ctx, def.SQL)
	if err != nil {
		return ReportRun{}, err
	}
	if pl.final.Kind != "SELECT" {
		return ReportRun{}, fmt.Errorf("%w: report sql must be a single SELECT", sqlext.ErrFailedPrecondition)
	}
	asserts := make([]plan, len(def.Asserts))
	tables := slices.Clone(pl.final.Tables)
	for i, a := range def.Asserts {
		if asserts[i], err = s.authorize(ctx, a.SQL); err != nil {
			return ReportRun{}, err
		}
		for _, t := range asserts[i].final.Tables {
			if !slices.Contains(tables, t) {
				tables = append(tables, t)
			}
		}
	}
	slices.Sort(tables)

	sn, err := s.eng.Snapshot(ctx)
	if err != nil {
		return ReportRun{}, err
	}
	defer func() { _ = sn.Close() }()

	// Sidik data sumber pada transaksi ini: jumlah baris + jumlah hash baris
	// (tidak bergantung urutan). Hanya digest; tidak ada nilai yang keluar.
	if m.Engine, err = scalar(ctx, sn, "SELECT version()"); err != nil {
		return ReportRun{}, err
	}
	h := sha256.New()
	h.Write([]byte("duckdb " + m.Engine))
	for _, t := range tables {
		parts := strings.Split(t, ".")
		for i := range parts {
			parts[i] = classification.Ident(parts[i])
		}
		fp, err := scalar(ctx, sn, fmt.Sprintf("SELECT 'rows:' || count(*) || ',hash:' || coalesce(sum(hash(x)), 0) FROM %s x", strings.Join(parts, ".")))
		if err != nil {
			return ReportRun{}, err
		}
		m.Tables[t] = fp
		h.Write([]byte("\n" + t + "=" + fp))
	}
	m.SnapshotID = hex.EncodeToString(h.Sum(nil))[:32]

	for i, a := range def.Asserts {
		v, err := scalar(sqlext.WithMaskNonce(ctx, asserts[i].nonce), sn, asserts[i].sql)
		passed := err == nil && v == "true"
		m.Asserts = append(m.Asserts, report.AssertResult{Name: a.Name, SQLSHA256: report.SHA256([]byte(a.SQL)), Passed: passed})
		if !passed {
			if err == nil {
				err = fmt.Errorf("got %q, want true", v)
			}
			return ReportRun{}, fmt.Errorf("%w: report gate failed: assert %s: %w", sqlext.ErrFailedPrecondition, a.Name, err)
		}
	}

	rdr, err := sn.Query(sqlext.WithMaskNonce(ctx, pl.nonce), pl.sql)
	if err != nil {
		return ReportRun{}, err
	}
	data, rows, err := report.WriteParquet(rdr)
	rdr.Release()
	if err != nil {
		return ReportRun{}, err
	}
	m.ExecSHA256 = report.SHA256([]byte(pl.sql))
	m.Result = report.Result{File: "result.parquet", Format: "parquet", SHA256: report.SHA256(data), Rows: rows, Bytes: int64(len(data))}
	m.FinishedAt = time.Now().UTC()
	manifest, sig, err := s.opts.Signer.Sign(*m)
	if err != nil {
		return ReportRun{}, err
	}
	run := ReportRun{Manifest: *m, Prefix: "reports/" + m.Name + "/" + m.ReportID + "/", ResultSHA256: m.Result.SHA256, ManifestSHA256: report.SHA256(manifest)}
	run.Manifest.KeyID = s.opts.Signer.KeyID
	for _, o := range []struct {
		key  string
		data []byte
	}{{"result.parquet", data}, {"manifest.json", manifest}, {"manifest.json.sig", sig}} {
		if err := s.opts.Archive.Put(ctx, run.Prefix+o.key, o.data); err != nil {
			return ReportRun{}, err
		}
	}
	if s.opts.AccessLog != nil {
		base := accesslog.Record{Time: m.StartedAt, RequestID: audit.RequestID(ctx), User: id.Subject, Source: id.Source,
			Kind: "report", Status: "OK", Rows: rows, SQLHash: m.SQLSHA256, RewrittenHash: m.ExecSHA256}
		s.opts.AccessLog.Append(pl.access(base)...)
	}
	return run, nil
}

// scalar nilai tunggal (kolom pertama baris pertama) sebagai teks.
func scalar(ctx context.Context, sn engine.Snapshot, q string) (string, error) {
	rdr, err := sn.Query(ctx, q)
	if err != nil {
		return "", err
	}
	defer rdr.Release()
	var out []string
	for rdr.Next() {
		rec := rdr.RecordBatch()
		for i := range int(rec.NumRows()) {
			c := rec.Column(0)
			if c.IsNull(i) {
				out = append(out, "NULL")
			} else {
				out = append(out, strings.Clone(c.ValueStr(i)))
			}
		}
	}
	if err := rdr.Err(); err != nil {
		return "", err
	}
	if len(out) != 1 || rdr.Schema().NumFields() != 1 {
		return "", fmt.Errorf("want exactly one value, got %d rows × %d columns", len(out), rdr.Schema().NumFields())
	}
	return out[0], nil
}

// ReportRow RunReport sebagai baris karhutla_report_run(): report_id, name,
// snapshot_id, rows, location (prefix key di arsip), result_sha256,
// manifest_sha256.
func (s *Server) ReportRow(ctx context.Context, name string) ([]any, error) {
	r, err := s.RunReport(ctx, name)
	if err != nil {
		return nil, err
	}
	m := r.Manifest
	return []any{m.ReportID, m.Name, m.SnapshotID, m.Result.Rows, r.Prefix, r.ResultSHA256, r.ManifestSHA256}, nil
}
