package flightsql

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
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
	"github.com/bankmega/karhutlasql/internal/sqlrewrite"
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
// disimpan di arsip. asOf (versi atau waktu RFC 3339) menjalankan report pada
// snapshot DuckLake lampau (time travel); kosong = data terkini. Error memakai
// awalan sqlext agar terpetakan ke kode gRPC.
func (s *Server) RunReport(ctx context.Context, name, asOf string) (ReportRun, error) {
	id, _ := auth.FromContext(ctx)
	start := time.Now().UTC()
	m := report.Manifest{ReportID: start.Format("20060102T150405Z") + "-" + uuid.NewString()[:8], Name: name, User: id.Subject,
		Source: id.Source, StartedAt: start, AsOf: asOf, Tables: map[string]string{}, DQValidates: []string{}}
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
	at, err := asOfClause(m.AsOf)
	if err != nil {
		return ReportRun{}, err
	}
	// Time travel: AT pada setiap tabel report & assert sebelum otorisasi,
	// sehingga governance berlaku pada SQL yang benar-benar dieksekusi.
	travel := func(q string) (string, error) {
		if at == "" {
			return q, nil
		}
		out, err := sqlrewrite.TimeTravel(ctx, s.eng, q, at)
		if err != nil {
			return "", fmt.Errorf("%w: as_of: %w", sqlext.ErrFailedPrecondition, err)
		}
		return out, nil
	}
	q, err := travel(def.SQL)
	if err != nil {
		return ReportRun{}, err
	}
	pl, err := s.authorize(ctx, q)
	if err != nil {
		return ReportRun{}, err
	}
	if pl.final.Kind != "SELECT" {
		return ReportRun{}, fmt.Errorf("%w: report sql must be a single SELECT", sqlext.ErrFailedPrecondition)
	}
	asserts := make([]plan, len(def.Asserts))
	tables := slices.Clone(pl.final.Tables)
	for i, a := range def.Asserts {
		if q, err = travel(a.SQL); err != nil {
			return ReportRun{}, err
		}
		if asserts[i], err = s.authorize(ctx, q); err != nil {
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

	// Sidik data sumber pada transaksi ini (lihat fingerprint).
	if m.Engine, err = scalar(ctx, sn, "SELECT version()"); err != nil {
		return ReportRun{}, err
	}
	h := sha256.New()
	h.Write([]byte("duckdb " + m.Engine))
	snaps := map[string]string{}
	for _, t := range tables {
		fp, err := fingerprint(ctx, sn, t, at)
		if err != nil {
			return ReportRun{}, err
		}
		m.Tables[t] = fp
		snaps[t] = tableSnapshotID(m.Engine, t, fp)
		h.Write([]byte("\n" + t + "=" + fp))
	}
	m.SnapshotID = hex.EncodeToString(h.Sum(nil))[:32]
	if m.LakeSnapshots, err = lakeSnapshots(ctx, sn, tables, at); err != nil {
		return ReportRun{}, err
	}

	// DQ gate (Q-05): hasil GX terbaru tiap tabel sumber pada data yang sama.
	if s.opts.DQ != nil && s.opts.DQMaxAge > 0 {
		if err := s.opts.DQ.Refresh(ctx); err != nil { // hasil dq_publish instance lain (ADR 35)
			return ReportRun{}, err
		}
		if m.DQValidates, err = s.opts.DQ.Gate(snaps, s.opts.DQMaxAge, id.Source+":"+id.Subject); err != nil {
			return ReportRun{}, fmt.Errorf("%w: report gate failed: %w", sqlext.ErrFailedPrecondition, err)
		}
	}

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
	manifest, sig, err := s.opts.Signer.Sign(ctx, *m)
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

// asOfClause klausa AT DuckLake dari as_of: bilangan bulat = versi
// snapshot, RFC 3339 = waktu. Kosong = tanpa time travel.
func asOfClause(asOf string) (string, error) {
	if asOf == "" {
		return "", nil
	}
	if v, err := strconv.ParseUint(asOf, 10, 63); err == nil {
		return fmt.Sprintf("VERSION => %d", v), nil
	}
	t, err := time.Parse(time.RFC3339Nano, asOf)
	if err != nil {
		return "", fmt.Errorf("%w: as_of %q: want a snapshot version or an RFC 3339 time", sqlext.ErrFailedPrecondition, asOf)
	}
	return "TIMESTAMP => TIMESTAMPTZ " + classification.Literal(t.UTC().Format("2006-01-02 15:04:05.999999")+"+00"), nil
}

// lakeSnapshots ID snapshot DuckLake yang dibaca report per catalog sumber
// bertipe ducklake: snapshot transaksi (tanpa at), versi at (harus ada), atau
// snapshot terakhir pada/sebelum waktu at.
func lakeSnapshots(ctx context.Context, sn engine.Snapshot, tables []string, at string) (map[string]int64, error) {
	cond := "TRUE"
	if v, ok := strings.CutPrefix(at, "VERSION => "); ok {
		cond = "snapshot_id = " + v
	} else if ts, ok := strings.CutPrefix(at, "TIMESTAMP => "); ok {
		cond = "snapshot_time <= " + ts
	}
	var out map[string]int64
	for _, t := range tables {
		cat, _, _ := strings.Cut(t, ".")
		if _, done := out[cat]; done {
			continue
		}
		n, err := scalar(ctx, sn, "SELECT count(*) FROM duckdb_databases() WHERE type = 'ducklake' AND lower(database_name) = "+classification.Literal(cat))
		if err != nil {
			return nil, err
		}
		if n != "1" {
			continue
		}
		v, err := scalar(ctx, sn, fmt.Sprintf("SELECT max(snapshot_id) FROM %s.snapshots() WHERE %s", classification.Ident(cat), cond))
		if err != nil {
			return nil, err
		}
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: catalog %s has no snapshot at %s", sqlext.ErrFailedPrecondition, cat, at)
		}
		if out == nil {
			out = map[string]int64{}
		}
		out[cat] = id
	}
	return out, nil
}

// fingerprint sidik isi tabel pada transaksi sn (dan snapshot at): jumlah
// baris + jumlah hash baris (tidak bergantung urutan). Hanya digest; tidak
// ada nilai yang keluar.
// ponytail: scan penuh juga untuk tabel DuckLake; pakai statistik snapshot
// DuckLake bila tabel besar membuat report lambat.
func fingerprint(ctx context.Context, sn engine.Snapshot, table, at string) (string, error) {
	parts := strings.Split(table, ".")
	for i := range parts {
		parts[i] = classification.Ident(parts[i])
	}
	if at != "" {
		at = " AT (" + at + ")"
	}
	return scalar(ctx, sn, fmt.Sprintf("SELECT 'rows:' || count(*) || ',hash:' || coalesce(sum(hash(x)), 0) FROM %s AS x%s", strings.Join(parts, "."), at))
}

// tableSnapshotID ID snapshot satu tabel (pengikat hasil DQ ke data, Q-03).
func tableSnapshotID(engineVersion, table, fp string) string {
	return report.SHA256([]byte("duckdb " + engineVersion + "\n" + table + "=" + fp))[:32]
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
func (s *Server) ReportRow(ctx context.Context, name, asOf string) ([]any, error) {
	r, err := s.RunReport(ctx, name, asOf)
	if err != nil {
		return nil, err
	}
	m := r.Manifest
	return []any{m.ReportID, m.Name, m.SnapshotID, m.Result.Rows, r.Prefix, r.ResultSHA256, r.ManifestSHA256}, nil
}
