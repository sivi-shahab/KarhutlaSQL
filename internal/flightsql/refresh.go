package flightsql

import (
	"context"
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ActionRefreshTable DoAction refresh tabel materialisasi (fase 9b):
// {"table":"catalog.schema.table","sql":"SELECT …"} → {"table","rows"}.
const ActionRefreshTable = "refresh_table"

var refreshIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// RefreshAction membangun ulang table dari satu SELECT secara atomik
// (CREATE OR REPLACE TABLE) sebagai identitas di ctx. Otorisasi, antrean,
// audit, dan access history memakai bentuk yang bisa di-parse parser authz,
// "CREATE TABLE <table> AS <sql>" (izin write pada table + read pada sumber);
// yang dieksekusi adalah statement setara dari komponen yang sama — nama
// tabel tervalidasi dan teks SELECT yang sama — sehingga pembaca tidak pernah
// melihat tabel hilang. Tabel ter-govern (mask/RLS) sebagai sumber ditolak
// seperti CREATE TABLE AS biasa.
func (s *Server) RefreshAction(ctx context.Context, body []byte) ([]byte, error) {
	var b struct {
		Table string `json:"table"`
		SQL   string `json:"sql"`
	}
	if err := json.Unmarshal(body, &b); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "action body must be JSON: %v", err)
	}
	parts := strings.Split(b.Table, ".")
	if len(parts) != 3 || !refreshIdent.MatchString(parts[0]) || !refreshIdent.MatchString(parts[1]) || !refreshIdent.MatchString(parts[2]) {
		return nil, status.Error(codes.InvalidArgument, "table must be catalog.schema.table (letters, digits, _)")
	}
	sel := strings.TrimSuffix(strings.TrimSpace(b.SQL), ";")
	if sel == "" {
		return nil, status.Error(codes.InvalidArgument, "sql is required")
	}
	create := "CREATE TABLE " + b.Table + " AS " + sel
	qctx, pl, end, err := s.begin(ctx, "update", create)
	if err != nil {
		return nil, err
	}
	if pl.final.Kind != "CREATE" || pl.sql != create {
		err = status.Error(codes.PermissionDenied, "refresh_table: sql must be a single SELECT over ungoverned tables")
		end(0, 0, err)
		return nil, err
	}
	if _, err := s.eng.Exec(qctx, "CREATE OR REPLACE TABLE "+b.Table+" AS "+sel); err != nil {
		err = toStatus(qctx, err)
		end(0, 0, err)
		return nil, err
	}
	// DuckDB tidak melaporkan jumlah baris CTAS; hitung tabel hasil.
	var n int64
	if v, err := scalar(qctx, s.eng, "SELECT count(*) FROM "+b.Table); err == nil {
		n, _ = strconv.ParseInt(v, 10, 64)
	}
	end(n, 0, nil)
	res, _ := json.Marshal(map[string]any{"table": b.Table, "rows": n}) // selalu bisa
	return res, nil
}
