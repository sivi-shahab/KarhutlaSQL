package sharing

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/flight/flightsql"
	"github.com/apache/arrow-go/v18/parquet"
	"github.com/apache/arrow-go/v18/parquet/compress"
	"github.com/apache/arrow-go/v18/parquet/pqarrow"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/bankmega/karhutlasql/internal/audit"
)

// Prefix awalan path HTTP endpoint Delta Sharing.
const Prefix = "/delta-sharing"

// Channel nilai header kanal untuk statement export.
const Channel = "sharing"

// Deps dependensi Handler.
type Deps struct {
	Config *Config
	// Flight klien Flight SQL in-process.
	Flight        *flightsql.Client
	ChannelHeader string
	// Token token internal identitas "share:<recipient>".
	Token func(recipient string) (string, error)
	// Dir direktori file Parquet hasil export (dibuat bila belum ada).
	Dir string
	// FileKey kunci HMAC URL file (acak per proses bila kosong).
	FileKey []byte
	// FileTTL umur URL & file export.
	FileTTL time.Duration
	// Endpoint URL publik endpoint sharing (mis. https://kq.bank.local:8444/delta-sharing);
	// kosong = dari Host request.
	Endpoint string
	Audit    *audit.Logger
	Log      *slog.Logger
}

type handler struct{ Deps }

// Handler endpoint Delta Sharing di bawah Prefix. Menjalankan pembersihan
// file kedaluwarsa sampai ctx selesai.
func Handler(ctx context.Context, d Deps) (http.Handler, error) {
	if err := os.MkdirAll(d.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("sharing.Handler: %w", err)
	}
	if len(d.FileKey) == 0 {
		d.FileKey = make([]byte, 32)
		_, _ = rand.Read(d.FileKey)
	}
	h := &handler{d}
	go h.cleanup(ctx)
	mux := http.NewServeMux()
	p := Prefix
	mux.HandleFunc("GET "+p+"/shares", h.auth(h.listShares))
	mux.HandleFunc("GET "+p+"/shares/{share}", h.auth(h.getShare))
	mux.HandleFunc("GET "+p+"/shares/{share}/schemas", h.auth(h.listSchemas))
	mux.HandleFunc("GET "+p+"/shares/{share}/schemas/{schema}/tables", h.auth(h.listTables))
	mux.HandleFunc("GET "+p+"/shares/{share}/all-tables", h.auth(h.listTables))
	mux.HandleFunc("GET "+p+"/shares/{share}/schemas/{schema}/tables/{table}/version", h.auth(h.version))
	mux.HandleFunc("HEAD "+p+"/shares/{share}/schemas/{schema}/tables/{table}", h.auth(h.version))
	mux.HandleFunc("GET "+p+"/shares/{share}/schemas/{schema}/tables/{table}/metadata", h.auth(h.metadata))
	mux.HandleFunc("POST "+p+"/shares/{share}/schemas/{schema}/tables/{table}/query", h.auth(h.query))
	mux.HandleFunc("GET "+p+"/files/{id}", h.file)
	return mux, nil
}

// auth: Bearer token → penerima.
func (h *handler) auth(next func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		recipient := ""
		if ok {
			recipient = h.Config.recipientFor(strings.TrimSpace(tok))
		}
		if recipient == "" {
			writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", "invalid or missing bearer token")
			return
		}
		next(w, r, recipient)
	}
}

func stableID(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	h := hex.EncodeToString(sum[:16])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

func (h *handler) listShares(w http.ResponseWriter, _ *http.Request, recipient string) {
	items := []map[string]string{}
	for _, name := range h.Config.sharesFor(recipient) {
		items = append(items, map[string]string{"name": name, "id": stableID(name)})
	}
	writeJSON(w, map[string]any{"items": items})
}

func (h *handler) share(w http.ResponseWriter, r *http.Request, recipient string) (string, Share, bool) {
	name := r.PathValue("share")
	s, ok := h.Config.shareFor(recipient, name)
	if !ok {
		writeError(w, http.StatusNotFound, "RESOURCE_DOES_NOT_EXIST", "share not found: "+name)
	}
	return name, s, ok
}

func (h *handler) getShare(w http.ResponseWriter, r *http.Request, recipient string) {
	name, _, ok := h.share(w, r, recipient)
	if ok {
		writeJSON(w, map[string]any{"share": map[string]string{"name": name, "id": stableID(name)}})
	}
}

func (h *handler) listSchemas(w http.ResponseWriter, r *http.Request, recipient string) {
	name, s, ok := h.share(w, r, recipient)
	if !ok {
		return
	}
	items := []map[string]string{}
	for _, schema := range slices.Sorted(maps.Keys(s.Schemas)) {
		items = append(items, map[string]string{"name": schema, "share": name})
	}
	writeJSON(w, map[string]any{"items": items})
}

func (h *handler) listTables(w http.ResponseWriter, r *http.Request, recipient string) {
	name, s, ok := h.share(w, r, recipient)
	if !ok {
		return
	}
	schemas := slices.Sorted(maps.Keys(s.Schemas))
	if one := r.PathValue("schema"); one != "" {
		if _, ok := s.Schemas[one]; !ok {
			writeError(w, http.StatusNotFound, "RESOURCE_DOES_NOT_EXIST", "schema not found: "+one)
			return
		}
		schemas = []string{one}
	}
	items := []map[string]string{}
	for _, schema := range schemas {
		for _, t := range slices.Sorted(maps.Keys(s.Schemas[schema])) {
			items = append(items, map[string]string{"name": t, "schema": schema, "share": name,
				"shareId": stableID(name), "id": stableID(name, schema, t)})
		}
	}
	writeJSON(w, map[string]any{"items": items})
}

// table sumber tabel share; menulis 404 bila tidak ada.
func (h *handler) table(w http.ResponseWriter, r *http.Request, recipient string) (shareName, source string, ok bool) {
	shareName, s, ok := h.share(w, r, recipient)
	if !ok {
		return "", "", false
	}
	source, ok = s.Schemas[r.PathValue("schema")][r.PathValue("table")]
	if !ok {
		writeError(w, http.StatusNotFound, "RESOURCE_DOES_NOT_EXIST", "table not found: "+r.PathValue("table"))
	}
	return shareName, source, ok
}

// version: tabel KarhutlaSQL tidak berversi Delta; versi = waktu export
// (detik Unix), naik monoton antar request.
func version() string { return strconv.FormatInt(time.Now().Unix(), 10) }

func (h *handler) version(w http.ResponseWriter, r *http.Request, recipient string) {
	if _, _, ok := h.table(w, r, recipient); !ok {
		return
	}
	if r.URL.Query().Get("startingTimestamp") != "" {
		writeError(w, http.StatusBadRequest, "INVALID_PARAMETER_VALUE", "startingTimestamp is not supported")
		return
	}
	w.Header().Set("Delta-Table-Version", version())
	w.WriteHeader(http.StatusOK)
}

// metadata: schema hasil (setelah governance) lewat query LIMIT 0.
func (h *handler) metadata(w http.ResponseWriter, r *http.Request, recipient string) {
	shareName, source, ok := h.table(w, r, recipient)
	if !ok {
		return
	}
	ex, err := h.export(r.Context(), recipient, source, 0, true)
	h.audit(r, recipient, shareName, "metadata", ex, err)
	if err != nil {
		writeStatus(w, err)
		return
	}
	w.Header().Set("Delta-Table-Version", version())
	h.writeHeaderLines(w, shareName, r.PathValue("schema"), r.PathValue("table"), ex)
}

// audit satu akses penerima (metadata atau query); Detail = jenis[:id file].
func (h *handler) audit(r *http.Request, recipient, shareName, kind string, ex exported, err error) {
	ev := audit.Event{Type: audit.ShareQuery, User: "share:" + recipient, Source: "local",
		Object: shareName + "." + r.PathValue("schema") + "." + r.PathValue("table"), Peer: r.RemoteAddr, Via: "http",
		Status: "OK", Rows: ex.rows, Detail: kind}
	if ex.id != "" {
		ev.Detail += ":" + ex.id
	}
	if err != nil {
		ev.Status, ev.Error = status.Code(err).String(), status.Convert(err).Message()
	}
	h.Audit.Emit(r.Context(), ev)
}

type queryBody struct {
	PredicateHints     []string `json:"predicateHints"`
	JSONPredicateHints string   `json:"jsonPredicateHints"`
	LimitHint          *int64   `json:"limitHint"`
	Version            *int64   `json:"version"`
	Timestamp          string   `json:"timestamp"`
	StartingVersion    *int64   `json:"startingVersion"`
}

func (h *handler) query(w http.ResponseWriter, r *http.Request, recipient string) {
	shareName, source, ok := h.table(w, r, recipient)
	if !ok {
		return
	}
	var b queryBody
	if r.ContentLength != 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&b); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, "MALFORMED_REQUEST", "body must be JSON")
			return
		}
	}
	if b.Version != nil || b.Timestamp != "" || b.StartingVersion != nil {
		writeError(w, http.StatusBadRequest, "INVALID_PARAMETER_VALUE", "time travel and change data feed are not supported")
		return
	}
	limit := int64(-1)
	if b.LimitHint != nil && *b.LimitHint >= 0 {
		limit = *b.LimitHint
	}
	ex, err := h.export(r.Context(), recipient, source, limit, false)
	h.audit(r, recipient, shareName, "query", ex, err)
	if err != nil {
		writeStatus(w, err)
		return
	}
	w.Header().Set("Delta-Table-Version", version())
	h.writeHeaderLines(w, shareName, r.PathValue("schema"), r.PathValue("table"), ex)
	exp := time.Now().Add(h.FileTTL)
	line, _ := json.Marshal(map[string]any{"file": map[string]any{ // selalu bisa
		"url": h.fileURL(r, ex.id, exp), "id": ex.id, "partitionValues": map[string]string{},
		"size": ex.size, "expirationTimestamp": exp.UnixMilli(),
		"stats": fmt.Sprintf(`{"numRecords":%d}`, ex.rows),
	}})
	_, _ = w.Write(append(line, '\n'))
}

func (h *handler) writeHeaderLines(w http.ResponseWriter, share, schema, table string, ex exported) {
	w.Header().Set("Content-Type", "application/x-ndjson; charset=utf-8")
	proto, _ := json.Marshal(map[string]any{"protocol": map[string]int{"minReaderVersion": 1}}) // selalu bisa
	meta, _ := json.Marshal(map[string]any{"metaData": map[string]any{                          // selalu bisa
		"id": stableID(share, schema, table), "name": table, "format": map[string]string{"provider": "parquet"},
		"schemaString": ex.schema, "partitionColumns": []string{}, "configuration": map[string]string{},
	}})
	_, _ = w.Write(append(append(proto, '\n'), append(meta, '\n')...))
}

// exported hasil export satu tabel.
type exported struct {
	id     string
	schema string // schemaString Spark
	rows   int64
	size   int64
}

// export menjalankan SELECT * FROM source sebagai "share:<recipient>" lewat
// Flight SQL in-process (governance & audit berlaku) dan menulis Parquet.
// schemaOnly: hanya schema (LIMIT 0, tanpa file).
func (h *handler) export(ctx context.Context, recipient, source string, limit int64, schemaOnly bool) (exported, error) {
	tok, err := h.Token(recipient)
	if err != nil {
		return exported{}, status.Error(codes.Internal, err.Error())
	}
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+tok, h.ChannelHeader, Channel)
	q := "SELECT * FROM " + source // komponen tervalidasi saat Load
	switch {
	case schemaOnly:
		q += " LIMIT 0"
	case limit >= 0:
		q += " LIMIT " + strconv.FormatInt(limit, 10)
	}
	info, err := h.Flight.Execute(ctx, q)
	if err != nil {
		return exported{}, err
	}
	if len(info.Endpoint) != 1 {
		return exported{}, status.Errorf(codes.Internal, "want 1 endpoint, got %d", len(info.Endpoint))
	}
	rdr, err := h.Flight.DoGet(ctx, info.Endpoint[0].Ticket)
	if err != nil {
		return exported{}, err
	}
	defer rdr.Release()
	ss, err := schemaString(rdr.Schema())
	if err != nil {
		return exported{}, status.Error(codes.FailedPrecondition, err.Error())
	}
	ex := exported{schema: ss}
	if schemaOnly {
		for rdr.Next() { // kosongkan stream
		}
		return ex, rdr.Err()
	}
	ex.id = newID()
	path := filepath.Join(h.Dir, ex.id+".parquet")
	if err := writeParquet(path, rdr.Schema(), rdr, &ex.rows); err != nil {
		_ = os.Remove(path)
		return exported{}, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return exported{}, status.Error(codes.Internal, err.Error())
	}
	ex.size = fi.Size()
	return ex, nil
}

type batchReader interface {
	Next() bool
	RecordBatch() arrow.RecordBatch
	Err() error
}

func writeParquet(path string, sch *arrow.Schema, rdr batchReader, rows *int64) error {
	f, err := os.OpenFile(filepath.Clean(path), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	pw, err := pqarrow.NewFileWriter(sch, f, parquet.NewWriterProperties(parquet.WithCompression(compress.Codecs.Zstd)),
		pqarrow.NewArrowWriterProperties(pqarrow.WithStoreSchema()))
	if err != nil {
		_ = f.Close()
		return status.Error(codes.Internal, err.Error())
	}
	for rdr.Next() {
		rec := rdr.RecordBatch()
		if err := pw.WriteBuffered(rec); err != nil {
			_ = pw.Close()
			return status.Error(codes.Internal, err.Error())
		}
		*rows += rec.NumRows()
	}
	if err := rdr.Err(); err != nil {
		_ = pw.Close()
		return err
	}
	if err := pw.Close(); err != nil { // menutup f juga
		return status.Error(codes.Internal, err.Error())
	}
	return nil
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (h *handler) sign(id string, exp int64) string {
	m := hmac.New(sha256.New, h.FileKey)
	_, _ = fmt.Fprintf(m, "%s|%d", id, exp) // hash.Hash tidak pernah error
	return hex.EncodeToString(m.Sum(nil))
}

func (h *handler) fileURL(r *http.Request, id string, exp time.Time) string {
	base := h.Endpoint
	if base == "" {
		scheme := "http"
		if r.TLS != nil {
			scheme = "https"
		}
		base = scheme + "://" + r.Host + Prefix
	}
	e := exp.Unix()
	return fmt.Sprintf("%s/files/%s?exp=%d&sig=%s", strings.TrimSuffix(base, "/"), id, e, h.sign(id, e))
}

// file unduhan Parquet dengan URL bertanda tangan (pengganti presigned URL
// object storage); mendukung Range (pembaca Parquet membaca footer dulu).
func (h *handler) file(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	exp, err := strconv.ParseInt(r.URL.Query().Get("exp"), 10, 64)
	if _, herr := hex.DecodeString(id); err != nil || herr != nil || len(id) != 32 || !constantEqual(h.sign(id, exp), r.URL.Query().Get("sig")) {
		writeError(w, http.StatusForbidden, "PERMISSION_DENIED", "invalid file signature")
		return
	}
	if time.Now().Unix() > exp {
		writeError(w, http.StatusForbidden, "PERMISSION_DENIED", "file URL expired")
		return
	}
	f, err := os.OpenInRoot(h.Dir, id+".parquet") // id hex 32 karakter; tidak bisa keluar dari Dir
	if err != nil {
		writeError(w, http.StatusNotFound, "RESOURCE_DOES_NOT_EXIST", "file not found")
		return
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", "file unreadable")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	http.ServeContent(w, r, id+".parquet", fi.ModTime(), f)
}

// cleanup menghapus file export yang lebih tua dari FileTTL.
func (h *handler) cleanup(ctx context.Context) {
	t := time.NewTicker(max(h.FileTTL/4, time.Minute))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		entries, _ := os.ReadDir(h.Dir)
		for _, e := range entries {
			if fi, err := e.Info(); err == nil && strings.HasSuffix(e.Name(), ".parquet") && time.Since(fi.ModTime()) > h.FileTTL {
				_ = os.Remove(filepath.Join(h.Dir, e.Name()))
			}
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

// writeError format error Delta Sharing {"errorCode","message"}.
func writeError(w http.ResponseWriter, code int, errorCode, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"errorCode": errorCode, "message": msg})
}

func writeStatus(w http.ResponseWriter, err error) {
	st, _ := status.FromError(err)
	switch st.Code() {
	case codes.PermissionDenied:
		writeError(w, http.StatusForbidden, "PERMISSION_DENIED", st.Message())
	case codes.InvalidArgument, codes.FailedPrecondition:
		writeError(w, http.StatusBadRequest, "INVALID_PARAMETER_VALUE", st.Message())
	case codes.ResourceExhausted:
		writeError(w, http.StatusTooManyRequests, "RESOURCE_EXHAUSTED", st.Message())
	case codes.Unauthenticated:
		writeError(w, http.StatusUnauthorized, "UNAUTHENTICATED", st.Message())
	default:
		writeError(w, http.StatusInternalServerError, "INTERNAL_ERROR", st.Message())
	}
}
