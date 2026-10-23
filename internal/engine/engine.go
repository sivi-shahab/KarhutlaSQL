package engine

import (
	"context"
	"errors"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
)

// Engine mengeksekusi SQL dan mengembalikan hasil sebagai Arrow. Semua method
// menghormati cancellation ctx (timeout & kill session bergantung padanya).
//
// Method metadata mengembalikan RecordReader dengan urutan kolom tetap
// (didokumentasikan per method) agar lapisan Flight SQL cukup memberi label
// schema protokol tanpa tahu SQL engine.
type Engine interface {
	// Query mengeksekusi statement yang menghasilkan baris. Pemanggil wajib
	// Release() reader; koneksi engine dilepas saat itu.
	Query(ctx context.Context, sql string, args ...any) (array.RecordReader, error)
	// Exec mengeksekusi statement tanpa hasil baris dan mengembalikan jumlah baris terdampak.
	Exec(ctx context.Context, sql string, args ...any) (int64, error)
	// Describe menyiapkan statement tanpa mengeksekusinya.
	Describe(ctx context.Context, sql string) (StatementInfo, error)
	// Analyze mem-bind tepat satu statement tanpa mengeksekusinya dan
	// melaporkan apa yang disentuhnya (input keputusan authz). Lebih dari satu
	// statement → error.
	Analyze(ctx context.Context, sql string) (Analysis, error)
	// SerializeSQL / DeserializeSQL: AST JSON engine (untuk rewrite SELECT).
	SerializeSQL(ctx context.Context, sql string) (string, error)
	DeserializeSQL(ctx context.Context, ast string) (string, error)
	// Columns kolom tabel (nama, tipe SQL engine) urut posisi.
	Columns(ctx context.Context, catalog, schema, table string) ([]Column, error)
	// Snapshot membuka satu transaksi: semua Query di dalamnya melihat data
	// yang sama (report run, G-10). Pemanggil wajib Close.
	Snapshot(ctx context.Context) (Snapshot, error)

	// Catalogs: (catalog_name).
	Catalogs(ctx context.Context) (array.RecordReader, error)
	// Schemas: (catalog_name, db_schema_name). Pattern memakai sintaks LIKE; nil = semua.
	Schemas(ctx context.Context, catalog, schemaPattern *string) (array.RecordReader, error)
	// Tables: (catalog_name, db_schema_name, table_name, table_type).
	Tables(ctx context.Context, f TableFilter) (array.RecordReader, error)
	// TableTypes: (table_type).
	TableTypes(ctx context.Context) (array.RecordReader, error)
	// TableSchema mengembalikan schema Arrow sebuah tabel/view.
	TableSchema(ctx context.Context, catalog, schema, table string) (*arrow.Schema, error)

	Close() error
}

// Snapshot transaksi baca. Query berurutan (reader sebelumnya harus dilepas).
type Snapshot interface {
	Query(ctx context.Context, sql string, args ...any) (array.RecordReader, error)
	// Close mengakhiri transaksi (rollback: snapshot tidak menulis).
	Close() error
}

// StatementInfo hasil Describe.
type StatementInfo struct {
	NumParams int
	// ReturnsRows true untuk SELECT/EXPLAIN/PRAGMA/CALL dll.
	ReturnsRows bool
	// Schema hasil bila bisa ditentukan tanpa eksekusi; nil bila tidak diketahui.
	Schema *arrow.Schema
}

// Analysis hasil Engine.Analyze.
type Analysis struct {
	// Kind tipe statement menurut parser engine (SELECT, INSERT, UPDATE,
	// DELETE, CREATE, DROP, ALTER, COPY, ATTACH, ...).
	Kind string
	// Catalogs yang tabelnya dibaca, hasil resolusi binder (termasuk tabel
	// di balik view dan scan yang dibuang optimizer).
	Catalogs []string
	// Functions table function / scan non-tabel yang dipakai (nama huruf
	// besar, mis. RANGE, READ_CSV).
	Functions []string
	// DefaultCatalog catalog untuk nama tak berkualifikasi; AllCatalogs semua
	// catalog ter-attach (resolusi nama target tulis).
	DefaultCatalog string
	AllCatalogs    []string
	// Tables tabel yang dibaca, "catalog.schema.table" huruf kecil.
	Tables []string
	// Scans setiap scan tabel di plan logis pra-optimasi beserta ekspresi
	// FILTER induk langsungnya ("" bila induk bukan FILTER).
	Scans []Scan
	// Columns kolom yang dibaca per tabel ("catalog.schema.table" → nama
	// kolom huruf kecil) menurut plan fisik tanpa pushdown filter: kolom
	// filter ikut terhitung. Tabel yang dibaca tanpa kolom (count(*)) tidak ada.
	Columns map[string][]string
}

// Scan satu scan tabel di plan.
type Scan struct {
	Table        string
	ParentFilter string
}

// Column kolom tabel.
type Column struct {
	Name, Type string
}

// TableFilter filter untuk Engine.Tables. Pattern memakai sintaks LIKE; nil = semua.
type TableFilter struct {
	Catalog       *string
	SchemaPattern *string
	TablePattern  *string
	TableTypes    []string
}

// ErrPermission engine menolak akses (mis. file/URL di luar allowed
// directories, G-07).
var ErrPermission = errors.New("engine permission error")

// Stats statistik eksekusi dari profiling engine (G-09).
type Stats struct {
	// PeakMemory puncak memori buffer engine selama statement (byte). Buffer
	// manager dipakai bersama: di bawah konkurensi ikut menghitung statement lain.
	PeakMemory int64
	// PlanHash hash bentuk plan fisik (pohon operator); sama untuk query yang
	// berbeda hanya di literal.
	PlanHash string
}

type statsKey struct{}

// WithStats meminta Query/Exec mengisi st setelah statement selesai (Query:
// saat reader dilepas). Tanpa ini profiling tidak diaktifkan.
func WithStats(ctx context.Context, st *Stats) context.Context {
	return context.WithValue(ctx, statsKey{}, st)
}

// StatsFrom sink statistik di ctx; nil bila tidak diminta.
func StatsFrom(ctx context.Context) *Stats {
	st, _ := ctx.Value(statsKey{}).(*Stats)
	return st
}
