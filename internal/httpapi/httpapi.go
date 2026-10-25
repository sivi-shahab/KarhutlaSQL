// Package httpapi endpoint HTTP Arrow IPC (backlog fase 9d): POST
// /api/v1/query → stream Arrow IPC, untuk aplikasi web dan notebook tanpa
// gRPC. Setiap request menjadi panggilan Flight SQL in-process dengan token
// pemanggil dan kanal "http", sehingga autentikasi, governance, antrean,
// kuota, audit, dan access history sama dengan klien Flight SQL. Package ini
// hanya menerjemahkan protokol; tidak ada keputusan akses di sini.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/flight"
	"github.com/apache/arrow-go/v18/arrow/flight/flightsql"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// Channel nilai header kanal (auth.ChannelHeader) untuk statement dari HTTP.
const Channel = "http"

// ContentType media type stream Arrow IPC.
const ContentType = "application/vnd.apache.arrow.stream"

// StatusTrailer trailer HTTP berisi "OK" bila stream lengkap, selain itu
// pesan error (statement gagal setelah baris pertama terkirim).
const StatusTrailer = "X-Karhutla-Status"

const maxBody = 1 << 20

// Config dependensi Handler.
type Config struct {
	// Login user + password lokal (Basic) → bearer token lewat handshake Flight.
	Login func(ctx context.Context, user, password string) (string, error)
	// Flight klien Flight SQL ke server ini (in-process).
	Flight *flightsql.Client
	// ChannelHeader nama header gRPC kanal (auth.ChannelHeader).
	ChannelHeader string
	// AllowedOrigins origin browser yang boleh memanggil (CORS); kosong = tanpa CORS.
	AllowedOrigins []string
	Log            *slog.Logger
}

type request struct {
	SQL    string `json:"sql"`
	Params []any  `json:"params"`
}

// Handler mux HTTP: POST /api/v1/query, POST /api/v1/login (Basic → token),
// GET /api/v1/tables (tabel yang boleh dibaca pemanggil).
func Handler(cfg Config) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/login", cfg.login)
	mux.HandleFunc("GET /api/v1/tables", cfg.tables)
	mux.HandleFunc("/api/v1/query", func(w http.ResponseWriter, r *http.Request) {
		if cfg.cors(w, r) {
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST, OPTIONS")
			writeError(w, http.StatusMethodNotAllowed, "use POST")
			return
		}
		cfg.query(w, r)
	})
	return mux
}

// cors menambah header CORS untuk origin yang diizinkan; true = preflight
// sudah dijawab.
func (cfg Config) cors(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || !slices.Contains(cfg.AllowedOrigins, origin) {
		return false
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Vary", "Origin")
	if r.Method == http.MethodOptions {
		h.Set("Access-Control-Allow-Methods", "POST")
		h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		h.Set("Access-Control-Max-Age", "600")
		w.WriteHeader(http.StatusNoContent)
		return true
	}
	return false
}

// callMD metadata panggilan Flight (token pemanggil & kanal http); false =
// 401 sudah ditulis.
func (cfg Config) callMD(w http.ResponseWriter, r *http.Request) ([]string, bool) {
	token, err := cfg.token(r.Context(), r)
	if err != nil {
		w.Header().Set("WWW-Authenticate", `Bearer, Basic realm="karhutlasql"`)
		writeError(w, http.StatusUnauthorized, err.Error())
		return nil, false
	}
	md := []string{"authorization", "Bearer " + token}
	if cfg.ChannelHeader != "" {
		md = append(md, cfg.ChannelHeader, Channel)
	}
	return md, true
}

// login: Basic → {"token"} agar aplikasi (mis. Web UI) tidak menyimpan
// password; token berumur --jwt-ttl dan bisa di-kill seperti sesi lain.
func (cfg Config) login(w http.ResponseWriter, r *http.Request) {
	user, pass, ok := r.BasicAuth()
	if !ok {
		w.Header().Set("WWW-Authenticate", `Basic realm="karhutlasql"`)
		writeError(w, http.StatusUnauthorized, "Basic credentials required")
		return
	}
	tok, err := cfg.Login(r.Context(), user, pass)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]string{"token": tok})
}

// tables: GetTables Flight SQL (sudah difilter ke catalog yang boleh dibaca).
func (cfg Config) tables(w http.ResponseWriter, r *http.Request) {
	md, ok := cfg.callMD(w, r)
	if !ok {
		return
	}
	ctx := metadata.AppendToOutgoingContext(r.Context(), md...)
	info, err := cfg.Flight.GetTables(ctx, &flightsql.GetTablesOpts{})
	if err != nil {
		writeStatus(w, err)
		return
	}
	rdr, err := cfg.fetch(ctx, info.Endpoint, nil)
	if err != nil {
		writeStatus(w, err)
		return
	}
	defer rdr.Release()
	type table struct {
		Catalog string `json:"catalog"`
		Schema  string `json:"schema"`
		Name    string `json:"name"`
		Type    string `json:"type"`
	}
	out := []table{}
	for rdr.Next() {
		rec := rdr.RecordBatch()
		for i := range int(rec.NumRows()) {
			// Clone: buffer batch dilepas pada Next() berikutnya.
			out = append(out, table{Catalog: strings.Clone(rec.Column(0).ValueStr(i)), Schema: strings.Clone(rec.Column(1).ValueStr(i)),
				Name: strings.Clone(rec.Column(2).ValueStr(i)), Type: strings.Clone(rec.Column(3).ValueStr(i))})
		}
	}
	if err := rdr.Err(); err != nil {
		writeStatus(w, err)
		return
	}
	writeJSON(w, out)
}

func (cfg Config) query(w http.ResponseWriter, r *http.Request) {
	md, ok := cfg.callMD(w, r)
	if !ok {
		return
	}
	ctx := metadata.AppendToOutgoingContext(r.Context(), md...)
	var req request
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.UseNumber()
	if err := dec.Decode(&req); err != nil || strings.TrimSpace(req.SQL) == "" {
		writeError(w, http.StatusBadRequest, `body must be JSON {"sql": "...", "params": [...]}`)
		return
	}
	rdr, err := cfg.execute(ctx, req)
	if err != nil {
		writeStatus(w, err)
		return
	}
	defer rdr.Release()
	w.Header().Set("Content-Type", ContentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Trailer", StatusTrailer)
	iw := ipc.NewWriter(w, ipc.WithSchema(rdr.Schema()))
	rc := http.NewResponseController(w)
	for rdr.Next() {
		if err := iw.Write(rdr.RecordBatch()); err != nil {
			return // klien putus; ctx request membatalkan statement
		}
		_ = rc.Flush()
	}
	if err := rdr.Err(); err != nil {
		// Status 200 sudah terkirim: laporkan di trailer dan putuskan koneksi
		// tanpa chunk penutup (HTTP/1.1), agar hasil terpotong tidak terbaca
		// sebagai lengkap.
		cfg.Log.Warn("http query stream failed", "err", err)
		w.Header().Set(StatusTrailer, status.Convert(err).Message())
		if conn, _, herr := rc.Hijack(); herr == nil {
			_ = conn.Close()
		}
		return
	}
	_ = iw.Close()
	w.Header().Set(StatusTrailer, "OK")
}

// token dari Authorization: Bearer (diteruskan apa adanya) atau Basic
// (login lokal lewat handshake).
func (cfg Config) token(ctx context.Context, r *http.Request) (string, error) {
	h := r.Header.Get("Authorization")
	if tok, ok := strings.CutPrefix(h, "Bearer "); ok && strings.TrimSpace(tok) != "" {
		return strings.TrimSpace(tok), nil
	}
	if user, pass, ok := r.BasicAuth(); ok {
		tok, err := cfg.Login(ctx, user, pass)
		if err != nil {
			return "", errors.New("invalid credentials")
		}
		return tok, nil
	}
	return "", errors.New("authorization required (Bearer token or Basic)")
}

// execute: tanpa parameter → statement biasa; dengan parameter → prepared
// statement Flight SQL (nilai JSON dikirim bertipe, engine meng-cast).
func (cfg Config) execute(ctx context.Context, req request) (*flightReader, error) {
	if len(req.Params) == 0 {
		info, err := cfg.Flight.Execute(ctx, req.SQL)
		if err != nil {
			return nil, err
		}
		return cfg.fetch(ctx, info.Endpoint, nil)
	}
	prep, err := cfg.Flight.Prepare(ctx, req.SQL)
	if err != nil {
		return nil, err
	}
	closePrep := func() { _ = prep.Close(context.WithoutCancel(ctx)) }
	rec, err := paramRecord(req.Params)
	if err != nil {
		closePrep()
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	prep.SetParameters(rec)
	rec.Release()
	info, err := prep.Execute(ctx)
	if err != nil {
		closePrep()
		return nil, err
	}
	return cfg.fetch(ctx, info.Endpoint, closePrep)
}

// flightReader stream hasil + pelepasan prepared statement.
type flightReader struct {
	*flight.Reader
	release func()
}

func (f *flightReader) Release() {
	f.Reader.Release()
	if f.release != nil {
		f.release()
	}
}

func (cfg Config) fetch(ctx context.Context, eps []*flight.FlightEndpoint, release func()) (*flightReader, error) {
	if len(eps) != 1 {
		if release != nil {
			release()
		}
		return nil, status.Errorf(codes.Internal, "want 1 endpoint, got %d", len(eps))
	}
	rdr, err := cfg.Flight.DoGet(ctx, eps[0].Ticket)
	if err != nil {
		if release != nil {
			release()
		}
		return nil, err
	}
	return &flightReader{Reader: rdr, release: release}, nil
}

// paramRecord satu baris parameter dari nilai JSON (string, angka, bool, null).
func paramRecord(params []any) (arrow.RecordBatch, error) {
	mem := memory.DefaultAllocator
	fields := make([]arrow.Field, len(params))
	cols := make([]arrow.Array, len(params))
	defer func() {
		for _, a := range cols {
			if a != nil {
				a.Release()
			}
		}
	}()
	for i, v := range params {
		var b array.Builder
		switch x := v.(type) {
		case nil:
			sb := array.NewStringBuilder(mem)
			sb.AppendNull()
			b = sb
		case string:
			sb := array.NewStringBuilder(mem)
			sb.Append(x)
			b = sb
		case bool:
			bb := array.NewBooleanBuilder(mem)
			bb.Append(x)
			b = bb
		case json.Number:
			if n, err := x.Int64(); err == nil {
				ib := array.NewInt64Builder(mem)
				ib.Append(n)
				b = ib
			} else if f, err := x.Float64(); err == nil {
				fb := array.NewFloat64Builder(mem)
				fb.Append(f)
				b = fb
			} else {
				return nil, fmt.Errorf("params[%d]: invalid number %q", i, x)
			}
		default:
			return nil, fmt.Errorf("params[%d]: want string, number, boolean or null", i)
		}
		cols[i] = b.NewArray()
		b.Release()
		fields[i] = arrow.Field{Name: fmt.Sprintf("$%d", i+1), Type: cols[i].DataType(), Nullable: true}
	}
	return array.NewRecordBatch(arrow.NewSchema(fields, nil), cols, 1), nil
}

func writeStatus(w http.ResponseWriter, err error) {
	st, _ := status.FromError(err)
	code := map[codes.Code]int{
		codes.Unauthenticated:    http.StatusUnauthorized,
		codes.PermissionDenied:   http.StatusForbidden,
		codes.InvalidArgument:    http.StatusBadRequest,
		codes.NotFound:           http.StatusNotFound,
		codes.ResourceExhausted:  http.StatusTooManyRequests,
		codes.DeadlineExceeded:   http.StatusGatewayTimeout,
		codes.Canceled:           499,
		codes.FailedPrecondition: http.StatusPreconditionFailed,
		codes.Unimplemented:      http.StatusNotImplemented,
		codes.Unavailable:        http.StatusServiceUnavailable,
	}[st.Code()]
	if code == 0 {
		code = http.StatusInternalServerError
	}
	writeError(w, code, st.Message())
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
