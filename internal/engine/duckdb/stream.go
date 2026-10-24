//go:build duckdb_arrow

package duckdb

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	bindings "github.com/duckdb/duckdb-go-bindings"
	duckdbdrv "github.com/duckdb/duckdb-go/v2"
	"github.com/duckdb/duckdb-go/v2/arrowmapping"
	"github.com/duckdb/duckdb-go/v2/mapping"
)

// Streaming hasil (ADR 34). duckdb-go selalu memakai duckdb_pending_prepared
// (hasil dimaterialisasi penuh sebelum baris pertama) dan tidak mengekspos
// handle C koneksinya. Jalur ini mengeksekusi SELECT tanpa parameter lewat
// C API (duckdb_pending_prepared_streaming + duckdb_fetch_chunk) di atas
// koneksi duckdb-go yang sama, sehingga UDF, secret, dan guardrail tetap
// berlaku; context query ditaruh di store context duckdb-go agar UDF
// (identitas, nonce masking) melihatnya seperti jalur biasa.
//
// Handle C dan store context diambil dari field tak-terekspor *duckdbdrv.Conn
// lewat reflect (nama & tipe diperiksa, ID koneksi dicocokkan ke DuckDB saat
// Open). Bila struktur duckdb-go berubah, probe gagal dan engine kembali ke
// jalur materialisasi — tidak pernah menebak.

// rawConn handle C dan store context duckdb-go milik satu koneksi.
type rawConn struct {
	conn mapping.Connection
	id   uint64
	ctxs *sync.Map // contextStore.m: ID koneksi → context.Context
}

var errLayout = errors.New("unsupported duckdb-go Conn layout")

// rawOf membaca handle dari field "conn", "id", "ctxStore.m" *duckdbdrv.Conn.
func rawOf(c *duckdbdrv.Conn) (rawConn, error) {
	v := reflect.ValueOf(c).Elem()
	conn, id, store := v.FieldByName("conn"), v.FieldByName("id"), v.FieldByName("ctxStore")
	if conn.Type() != reflect.TypeFor[mapping.Connection]() || id.Kind() != reflect.Uint64 ||
		store.Kind() != reflect.Pointer || store.IsNil() || store.Elem().Kind() != reflect.Struct {
		return rawConn{}, errLayout
	}
	m := store.Elem().FieldByName("m")
	if !m.IsValid() || m.Type() != reflect.TypeFor[sync.Map]() {
		return rawConn{}, errLayout
	}
	return rawConn{
		conn: *(*mapping.Connection)(unsafe.Pointer(conn.UnsafeAddr())), //nolint:gosec // G103: tipe diperiksa di atas
		id:   id.Uint(),
		ctxs: (*sync.Map)(unsafe.Pointer(m.UnsafeAddr())), //nolint:gosec // G103: tipe diperiksa di atas
	}, nil
}

// probeStreaming memastikan rawOf cocok dengan koneksi DuckDB sebenarnya.
func probeStreaming(c *duckdbdrv.Conn) error {
	rc, err := rawOf(c)
	if err != nil {
		return err
	}
	var cc mapping.ClientContext
	mapping.ConnectionGetClientContext(rc.conn, &cc)
	defer mapping.DestroyClientContext(&cc)
	if got := uint64(mapping.ClientContextGetConnectionId(cc)); got != rc.id {
		return fmt.Errorf("%w: connection id %d != %d", errLayout, got, rc.id)
	}
	return nil
}

// stream mengeksekusi query (tanpa parameter) secara streaming di c. Error
// prepare dikembalikan sebagai errPrepare agar pemanggil bisa mengulang di
// jalur duckdb-go (yang memberi tipe error lengkap).
func stream(ctx context.Context, c *duckdbdrv.Conn, query string) (array.RecordReader, error) {
	rc, err := rawOf(c)
	if err != nil {
		return nil, err
	}
	r := &streamReader{rc: rc, ctx: ctx, done: make(chan struct{}), exited: make(chan struct{})}
	r.refs.Store(1)
	// UDF di statement ini melihat ctx query; table UDF duckdb-go membacanya
	// saat bind (di dalam Prepare), jadi disimpan sebelum Prepare.
	rc.ctxs.Store(rc.id, ctx)
	if mapping.Prepare(rc.conn, query, &r.stmt) == mapping.StateError || mapping.NParams(r.stmt) > 0 {
		mapping.DestroyPrepare(&r.stmt)
		rc.ctxs.Delete(rc.id)
		return nil, errPrepare
	}
	// Pembatalan: ctx selesai → interrupt eksekusi/fetch. Release menunggu
	// goroutine ini keluar sebelum membebaskan handle (tanpa itu Interrupt bisa
	// mengenai koneksi yang sudah ditutup → korupsi heap, ditemukan
	// TestStreaming_Stress).
	go func() {
		defer close(r.exited)
		select {
		case <-ctx.Done():
			mapping.Interrupt(rc.conn)
		case <-r.done:
		}
	}()
	//nolint:staticcheck // SA1019: satu-satunya API streaming di C API DuckDB 1.5 (ADR 34)
	if bindings.PendingPreparedStreaming(r.stmt, &r.pending) == mapping.StateError {
		err := &duckdbdrv.Error{Type: duckdbdrv.ErrorTypeInvalid, Msg: mapping.PendingError(r.pending)}
		r.Release()
		return nil, errors.Join(ctx.Err(), err)
	}
	state := mapping.ExecutePending(r.pending, &r.res)
	r.hasRes = true // juga saat error: hasil membawa pesan & harus dibebaskan
	if state == mapping.StateError {
		err := r.resultErr()
		r.Release()
		return nil, errors.Join(ctx.Err(), err)
	}
	r.opts = arrowmapping.ResultGetArrowOptions(&r.res)
	n := mapping.ColumnCount(&r.res)
	names, types := make([]string, n), make([]mapping.LogicalType, n)
	for i := range n {
		names[i], types[i] = mapping.ColumnName(&r.res, i), mapping.ColumnLogicalType(&r.res, i)
	}
	schema, ed := arrowmapping.NewArrowSchema(r.opts, types, names)
	for i := range types {
		mapping.DestroyLogicalType(&types[i])
	}
	if err := errData(ed); err != nil {
		r.Release()
		return nil, fmt.Errorf("arrow schema: %w", err)
	}
	r.schema = schema
	return r, nil
}

// errPrepare: ulangi lewat jalur duckdb-go (error bertipe / parameter).
var errPrepare = errors.New("prepare failed")

func errData(ed mapping.ErrorData) error {
	defer mapping.DestroyErrorData(&ed)
	if !mapping.ErrorDataHasError(ed) {
		return nil
	}
	return &duckdbdrv.Error{Type: duckdbdrv.ErrorType(mapping.ErrorDataErrorType(ed)), Msg: mapping.ErrorDataMessage(ed)}
}

// streamReader array.RecordReader di atas hasil streaming DuckDB: tiap Next
// mengambil satu chunk (eksekusi berjalan seiring pembacaan, dibatasi
// streaming_buffer_size).
type streamReader struct {
	rc      rawConn
	ctx     context.Context
	done    chan struct{} // Release → goroutine interrupt berhenti
	exited  chan struct{} // goroutine interrupt sudah keluar
	stmt    mapping.PreparedStatement
	pending mapping.PendingResult
	res     mapping.Result
	opts    arrowmapping.ArrowOptions
	schema  *arrow.Schema
	cur     arrow.RecordBatch
	err     error
	hasRes  bool
	eof     bool // hasil habis dibaca: tidak perlu interrupt
	refs    atomic.Int64
	once    sync.Once
}

func (r *streamReader) resultErr() error {
	return &duckdbdrv.Error{Type: duckdbdrv.ErrorType(mapping.ResultErrorType(&r.res)), Msg: mapping.ResultError(&r.res)}
}

func (r *streamReader) Schema() *arrow.Schema { return r.schema }

func (r *streamReader) Next() bool {
	if r.cur != nil {
		r.cur.Release()
		r.cur = nil
	}
	if r.err != nil || r.eof { // Next setelah habis: tetap false (FetchChunk lagi = error DuckDB)
		return false
	}
	if err := r.ctx.Err(); err != nil {
		r.err = err
		return false
	}
	ch := mapping.FetchChunk(r.res)
	if ch.Ptr == nil {
		r.eof = true
		if msg := mapping.ResultError(&r.res); msg != "" {
			r.err = errors.Join(r.ctx.Err(), r.resultErr())
		}
		return false
	}
	defer mapping.DestroyDataChunk(&ch)
	rec, ed := arrowmapping.DataChunkToArrowArray(r.opts, r.schema, ch)
	if err := errData(ed); err != nil {
		r.err = fmt.Errorf("arrow array: %w", err)
		return false
	}
	r.cur = rec
	return true
}

func (r *streamReader) RecordBatch() arrow.RecordBatch { return r.cur }

//nolint:staticcheck // Record wajib ada di array.RecordReader (deprecated alias)
func (r *streamReader) Record() arrow.RecordBatch { return r.cur }

func (r *streamReader) Err() error { return r.err }

func (r *streamReader) Retain() { r.refs.Add(1) }

// Release terakhir menghentikan eksekusi yang belum selesai dan membebaskan
// handle C; koneksi ditutup pemanggil (connReader) sesudahnya.
func (r *streamReader) Release() {
	if r.refs.Add(-1) > 0 {
		return
	}
	r.once.Do(func() {
		close(r.done)
		<-r.exited
		if r.cur != nil {
			r.cur.Release()
		}
		if r.hasRes {
			if !r.eof {
				mapping.Interrupt(r.rc.conn) // klien berhenti lebih awal: hentikan sisa pipeline
			}
			mapping.DestroyResult(&r.res)
		}
		if r.pending.Ptr != nil {
			mapping.DestroyPending(&r.pending)
		}
		if r.opts.Ptr != nil {
			arrowmapping.DestroyArrowOptions(&r.opts)
		}
		mapping.DestroyPrepare(&r.stmt)
		r.rc.ctxs.Delete(r.rc.id)
	})
}
