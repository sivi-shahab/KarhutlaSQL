package flightsql

import (
	"context"
	"encoding/json"
	"slices"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bankmega/karhutlasql/internal/audit"
	"github.com/bankmega/karhutlasql/internal/auth"
	"github.com/bankmega/karhutlasql/internal/classification"
	"github.com/bankmega/karhutlasql/internal/dq"
)

// Tipe DoAction runner GX (Q-02/Q-03). Body & hasil JSON.
const (
	ActionDQSnapshot = "dq_snapshot" // {"table"} → {"table","snapshot_id"}
	ActionDQPublish  = "dq_publish"  // {"table","snapshot_id","result":<Validation Result GX>}
)

type dqBody struct {
	Table      string          `json:"table"`
	SnapshotID string          `json:"snapshot_id"`
	Result     json.RawMessage `json:"result"`
}

// DQAction menangani ActionDQSnapshot/ActionDQPublish untuk identitas di ctx
// (role dq_runner + izin baca tabel). Runner mengambil snapshot_id sebelum
// membaca data, lalu mem-publish hasil GX dengan snapshot_id itu; server
// menolak bila data sudah berubah sehingga hasil terikat ke data yang
// divalidasi. Error berupa status gRPC.
func (s *Server) DQAction(ctx context.Context, typ string, body []byte) ([]byte, error) {
	id, _ := auth.FromContext(ctx)
	if s.opts.DQ == nil {
		return nil, status.Error(codes.FailedPrecondition, "data quality store is not configured (--archive-dir / --archive-s3-*)")
	}
	if !id.HasRole(dq.RoleRunner) {
		return nil, status.Errorf(codes.PermissionDenied, "action %s requires role %s", typ, dq.RoleRunner)
	}
	var b dqBody
	if err := json.Unmarshal(body, &b); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "action body must be JSON: %v", err)
	}
	table, snap, err := s.dqSnapshot(ctx, b.Table)
	if err != nil {
		return nil, err
	}
	if typ == ActionDQSnapshot {
		res, _ := json.Marshal(map[string]string{"table": table, "snapshot_id": snap}) // selalu bisa
		return res, nil
	}

	ev := audit.Event{Type: audit.DQPublished, User: id.Subject, Source: id.Source, Object: table}
	v, err := s.dqPublish(ctx, b, table, snap)
	if err != nil {
		ev.Status, ev.Error = status.Code(err).String(), err.Error()
		s.opts.Audit.Emit(ctx, ev)
		return nil, err
	}
	ev.Status, ev.Detail = "OK", v.ID
	s.opts.Audit.Emit(ctx, ev)
	if s.opts.MetadataChanged != nil {
		s.opts.MetadataChanged() // status DQ ke Unity Catalog
	}
	res, _ := json.Marshal(map[string]any{"validation_id": v.ID, "table": v.Table, "snapshot_id": v.SnapshotID,
		"success": v.Success, "failed_critical": v.FailedCritical, "failed_warning": v.FailedWarning}) // selalu bisa
	return res, nil
}

func (s *Server) dqPublish(ctx context.Context, b dqBody, table, snap string) (dq.Validation, error) {
	if b.SnapshotID != snap {
		return dq.Validation{}, status.Errorf(codes.FailedPrecondition,
			"%s changed since snapshot %q (now %s): validate again", table, b.SnapshotID, snap)
	}
	tags := s.opts.Classification.Get().TableTags(table)
	v, err := dq.Sanitize(b.Result, func(c string) bool { _, ok := tags[strings.ToLower(c)]; return ok })
	if err != nil {
		return dq.Validation{}, status.Error(codes.InvalidArgument, err.Error())
	}
	id, _ := auth.FromContext(ctx)
	v.Table, v.SnapshotID, v.RunBy = table, snap, id.Source+":"+id.Subject
	if v, err = s.opts.DQ.Publish(ctx, v); err != nil {
		return dq.Validation{}, status.Error(codes.Unavailable, err.Error())
	}
	return v, nil
}

// dqSnapshot nama kanonis tabel (harus "catalog.schema.table" yang boleh
// dibaca pemanggil) dan snapshot_id isinya saat ini.
func (s *Server) dqSnapshot(ctx context.Context, table string) (string, string, error) {
	parts := strings.Split(table, ".")
	if len(parts) != 3 || slices.Contains(parts, "") {
		return "", "", status.Errorf(codes.InvalidArgument, "table must be catalog.schema.table, got %q", table)
	}
	for i := range parts {
		parts[i] = classification.Ident(parts[i])
	}
	// SELECT 1: cek izin baca tanpa kolom (tidak memicu audit mask_applied).
	pl, err := s.authorize(ctx, "SELECT 1 FROM "+strings.Join(parts, "."))
	if err != nil {
		return "", "", err
	}
	if len(pl.final.Tables) != 1 {
		return "", "", status.Errorf(codes.InvalidArgument, "%s is not a table", table)
	}
	table = pl.final.Tables[0]
	sn, err := s.eng.Snapshot(ctx)
	if err != nil {
		return "", "", toStatus(ctx, err)
	}
	defer func() { _ = sn.Close() }()
	ver, err := scalar(ctx, sn, "SELECT version()")
	if err != nil {
		return "", "", toStatus(ctx, err)
	}
	fp, err := fingerprint(ctx, sn, table, "")
	if err != nil {
		return "", "", toStatus(ctx, err)
	}
	return table, tableSnapshotID(ver, table, fp), nil
}
