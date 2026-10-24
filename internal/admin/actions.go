package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bankmega/karhutlasql/internal/approval"
	"github.com/bankmega/karhutlasql/internal/audit"
	"github.com/bankmega/karhutlasql/internal/auth"
)

// Tipe Flight DoAction admin (G-12). Body & hasil JSON.
const (
	ActionHealth        = "health"         // semua user terautentikasi
	ActionListSessions  = "list_sessions"  // admin
	ActionKillSession   = "kill_session"   // admin; {"session_id"}
	ActionPolicyStatus  = "policy_status"  // admin
	ActionReloadPolicy  = "reload_policy"  // admin; {"document"} → pending + persetujuan pertama
	ActionApprovePolicy = "approve_policy" // admin; {"document","hash"}
)

// Actions tipe DoAction yang ditangani paket ini.
var Actions = []string{ActionHealth, ActionListSessions, ActionKillSession, ActionPolicyStatus, ActionReloadPolicy, ActionApprovePolicy}

type actionBody struct {
	SessionID string `json:"session_id"`
	Document  string `json:"document"`
	Hash      string `json:"hash"`
}

// Approver kunci identitas untuk four-eyes: source:subject.
func Approver(id auth.Identity) string { return id.Source + ":" + id.Subject }

// Do menjalankan action admin untuk identitas di ctx (sudah diautentikasi
// interceptor). Error berupa status gRPC.
func (o Options) Do(ctx context.Context, typ string, body []byte) ([]byte, error) {
	id, ok := auth.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "must authenticate first")
	}
	var b actionBody
	if len(body) > 0 {
		if err := json.Unmarshal(body, &b); err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "action body must be JSON: %v", err)
		}
	}
	if typ == ActionHealth {
		hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := o.Health(hctx); err != nil {
			return nil, status.Error(codes.Unavailable, "unhealthy")
		}
		return []byte(`{"status":"SERVING"}`), nil
	}
	if !id.HasRole(auth.RoleAdmin) {
		return nil, status.Errorf(codes.PermissionDenied, "action %s requires role %s", typ, auth.RoleAdmin)
	}
	var out any
	switch typ {
	case ActionListSessions:
		infos := o.Sessions.Snapshot()
		for i := range infos {
			if infos[i].SQL != "" {
				infos[i].SQL = o.Audit.Redact(infos[i].SQL)
			}
		}
		out = infos
	case ActionKillSession:
		if b.SessionID == id.TokenID {
			return nil, status.Error(codes.InvalidArgument, "cannot kill the current session")
		}
		killed, err := o.Sessions.Kill(ctx, b.SessionID)
		if !killed {
			return nil, status.Error(codes.NotFound, "session not found")
		}
		if err != nil {
			o.Audit.Emit(ctx, audit.Event{Type: audit.SessionKilled, User: id.Subject, Source: id.Source, Target: b.SessionID, Via: "action", Status: "PARTIAL", Error: err.Error()})
			return nil, status.Error(codes.Unavailable, err.Error())
		}
		o.Audit.Emit(ctx, audit.Event{Type: audit.SessionKilled, User: id.Subject, Source: id.Source, Target: b.SessionID, Via: "action"})
		out = map[string]any{"session_id": b.SessionID, "killed": true}
	case ActionPolicyStatus, ActionReloadPolicy, ActionApprovePolicy:
		st, err := o.policy(typ, Approver(id), b)
		if err != nil {
			return nil, err
		}
		out = st
	default:
		return nil, status.Errorf(codes.Unimplemented, "unknown action %q", typ)
	}
	res, _ := json.Marshal(out) // tipe hasil di atas selalu bisa di-marshal
	return res, nil
}

func (o Options) policy(typ, by string, b actionBody) (any, error) {
	if o.Gate == nil {
		return nil, status.Error(codes.FailedPrecondition, "no policy file configured")
	}
	var (
		st  approval.Status
		err error
	)
	switch typ {
	case ActionPolicyStatus:
		return o.Gate.Status(), nil
	case ActionReloadPolicy:
		st, err = o.Gate.Propose(b.Document, by)
	default:
		st, err = o.Gate.Approve(b.Document, by, b.Hash)
	}
	switch {
	case errors.Is(err, approval.ErrUnknown):
		return nil, status.Error(codes.NotFound, err.Error())
	case errors.Is(err, approval.ErrNoPending), errors.Is(err, approval.ErrSameApprover):
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	case err != nil:
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return st, nil
}

// httpAction menjalankan action lewat HTTP: GET /admin/policy,
// POST /admin/policy/{document}/reload, POST /admin/policy/{document}/approve?hash=.
func (o Options) httpAction(typ string) func(http.ResponseWriter, *http.Request, auth.Identity) {
	return func(w http.ResponseWriter, r *http.Request, id auth.Identity) {
		body, _ := json.Marshal(actionBody{Document: r.PathValue("document"), Hash: r.URL.Query().Get("hash")})
		res, err := o.Do(auth.WithIdentity(r.Context(), id), typ, body)
		if err != nil {
			s := status.Convert(err)
			code := map[codes.Code]int{codes.NotFound: http.StatusNotFound, codes.FailedPrecondition: http.StatusConflict,
				codes.InvalidArgument: http.StatusBadRequest, codes.PermissionDenied: http.StatusForbidden}[s.Code()]
			if code == 0 {
				code = http.StatusInternalServerError
			}
			http.Error(w, s.Message(), code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(res)
	}
}
