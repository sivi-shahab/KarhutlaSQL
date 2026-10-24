package flightsql

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bankmega/karhutlasql/internal/audit"
	"github.com/bankmega/karhutlasql/internal/auth"
	"github.com/bankmega/karhutlasql/internal/classification"
	"github.com/bankmega/karhutlasql/internal/report"
)

const (
	// ActionPIIAccess DoAction hak akses subjek data (UU PDP):
	// {"subject_id","from","to"} (tanggal YYYY-MM-DD, to inklusif) → daftar
	// statement yang mungkin membaca kolom PII milik subjek.
	ActionPIIAccess = "audit.pii_access"
	// RolePDPO role petugas pelindungan data pribadi; admin juga boleh.
	RolePDPO = "dpo"
)

// PIIAccess satu statement yang mungkin membaca data pribadi subjek.
type PIIAccess struct {
	Time      time.Time `json:"ts"`
	RequestID string    `json:"request_id"`
	User      string    `json:"user"`
	Source    string    `json:"source"`
	Kind      string    `json:"kind"`
	Table     string    `json:"table"`
	// Columns kolom bertag yang dibaca → mask yang diterapkan ("unmasked" =
	// nilai mentah terlihat).
	Columns   map[string]string `json:"columns"`
	RowFilter string            `json:"row_filter,omitempty"`
	// Match dasar pencantuman: unfiltered (tabel dibaca tanpa row filter),
	// row_filter_matches (baris subjek lolos row filter), subject_not_found
	// (subjek tidak ada di data saat ini: dicantumkan konservatif),
	// filter_unevaluable (predikat gagal dievaluasi: dicantumkan).
	Match string `json:"match"`
}

type piiBody struct {
	SubjectID string `json:"subject_id"`
	From      string `json:"from"`
	To        string `json:"to"`
}

// PIIAccessAction menjawab "siapa yang mungkin mengakses data subjek X" dari
// access history (G-05): record baca kolom bertag pada tabel yang punya
// kolom pengenal subjek (classification subjects), dikurangi akses ber-row
// filter yang tidak mencakup baris subjek. WHERE milik query pengguna tidak
// diperhitungkan, jadi hasilnya superset (konservatif). subject_id tidak
// pernah dicatat mentah (audit hanya hash).
func (s *Server) PIIAccessAction(ctx context.Context, body []byte) ([]byte, error) {
	id, _ := auth.FromContext(ctx)
	if !id.HasRole(auth.RoleAdmin) && !id.HasRole(RolePDPO) {
		return nil, status.Errorf(codes.PermissionDenied, "action %s requires role %s or %s", ActionPIIAccess, RolePDPO, auth.RoleAdmin)
	}
	if s.opts.AccessLog == nil {
		return nil, status.Error(codes.FailedPrecondition, "access history is not configured (--archive-dir / --archive-s3-*)")
	}
	var b piiBody
	if err := json.Unmarshal(body, &b); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "action body must be JSON: %v", err)
	}
	from, err1 := time.Parse(time.DateOnly, b.From)
	to, err2 := time.Parse(time.DateOnly, b.To)
	if b.SubjectID == "" || err1 != nil || err2 != nil || to.Before(from) {
		return nil, status.Error(codes.InvalidArgument, `body needs "subject_id" and "from"/"to" dates (YYYY-MM-DD, from <= to)`)
	}
	ev := audit.Event{Type: audit.PIIAccessQuery, User: id.Subject, Source: id.Source,
		Object: report.SHA256([]byte(b.SubjectID))[:16], Detail: b.From + ".." + b.To}
	out, err := s.piiAccess(ctx, b.SubjectID, from, to.AddDate(0, 0, 1))
	if err != nil {
		ev.Status, ev.Error = status.Code(err).String(), err.Error()
		s.opts.Audit.Emit(ctx, ev)
		return nil, err
	}
	ev.Status, ev.Rows = "OK", int64(len(out))
	s.opts.Audit.Emit(ctx, ev)
	res, _ := json.Marshal(out) // struct string/map: tidak pernah error
	return res, nil
}

func (s *Server) piiAccess(ctx context.Context, subject string, since, until time.Time) ([]PIIAccess, error) {
	recs, err := s.opts.AccessLog.Query(ctx, since, until)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "access history: %v", err)
	}
	subjects := s.opts.Classification.Get().Subjects
	byKey := map[string]*PIIAccess{} // request_id + tabel
	var order []string
	for _, r := range recs {
		if r.Action != "read" || r.Tag == "" || subjects[r.Table] == "" {
			continue
		}
		k := r.RequestID + "\x00" + r.Table
		a := byKey[k]
		if a == nil {
			a = &PIIAccess{Time: r.Time, RequestID: r.RequestID, User: r.User, Source: r.Source, Kind: r.Kind, Table: r.Table,
				Columns: map[string]string{}, RowFilter: r.RowFilter}
			byKey[k] = a
			order = append(order, k)
		}
		a.Columns[r.Column] = r.Policy
	}
	match := map[string]string{} // tabel + filter → hasil evaluasi
	var out []PIIAccess
	for _, k := range order {
		a := byKey[k]
		mk := a.Table + "\x00" + a.RowFilter
		if _, ok := match[mk]; !ok {
			match[mk] = s.subjectMatch(ctx, a.Table, subjects[a.Table], a.RowFilter, subject)
		}
		if a.Match = match[mk]; a.Match != "" {
			out = append(out, *a)
		}
	}
	slices.SortStableFunc(out, func(x, y PIIAccess) int { return x.Time.Compare(y.Time) })
	return out, nil
}

// subjectMatch dasar pencantuman (lihat PIIAccess.Match); "" = baris subjek
// tidak tercakup row filter. Predikat berasal dari policy (tercatat server di
// arsip WORM), dievaluasi pada data saat ini.
func (s *Server) subjectMatch(ctx context.Context, table, col, filter, subject string) string {
	parts := strings.Split(table, ".")
	for i := range parts {
		parts[i] = classification.Ident(parts[i])
	}
	q := fmt.Sprintf("SELECT count(*) FILTER (WHERE TRUE)::VARCHAR, count(*) FILTER (WHERE (%s))::VARCHAR FROM %s WHERE %s = $1",
		cmp.Or(filter, "TRUE"), strings.Join(parts, "."), classification.Ident(col))
	rdr, err := s.eng.Query(ctx, q, subject)
	if err != nil {
		return "filter_unevaluable"
	}
	defer rdr.Release()
	var total, kept string
	for rdr.Next() {
		rec := rdr.RecordBatch()
		// Clone: ValueStr menunjuk buffer Arrow yang dibebaskan Next berikutnya.
		total, kept = strings.Clone(rec.Column(0).ValueStr(0)), strings.Clone(rec.Column(1).ValueStr(0))
	}
	switch {
	case rdr.Err() != nil:
		return "filter_unevaluable"
	case total == "0":
		return "subject_not_found"
	case filter == "":
		return "unfiltered"
	case kept != "0":
		return "row_filter_matches"
	}
	return ""
}
