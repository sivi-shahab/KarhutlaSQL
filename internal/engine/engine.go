package engine

import (
	"context"

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
}

// TableFilter filter untuk Engine.Tables. Pattern memakai sintaks LIKE; nil = semua.
type TableFilter struct {
	Catalog       *string
	SchemaPattern *string
	TablePattern  *string
	TableTypes    []string
}
