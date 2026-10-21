//go:build duckdb_arrow

package flightsql

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/flight"
	fsql "github.com/apache/arrow-go/v18/arrow/flight/flightsql"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/bankmega/karhutlasql/internal/engine/duckdb"
	"github.com/bankmega/karhutlasql/internal/server"
)

// startServer menjalankan server Flight SQL penuh (gRPC asli) di port acak.
func startServer(t *testing.T, initSQL string) *fsql.Client {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	eng, err := duckdb.Open(ctx, duckdb.Config{InitSQL: initSQL})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := New(eng, log)
	if err != nil {
		t.Fatal(err)
	}
	lis, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx, lis, srv, 5*time.Second, log) }()

	cl, err := fsql.NewClient(lis.Addr().String(), nil, nil, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cl.Close()
		cancel()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
		_ = eng.Close()
	})
	return cl
}

// fetch menguras semua endpoint FlightInfo; nilai kolom c disalin sebagai string.
func fetch(ctx context.Context, t *testing.T, cl *fsql.Client, info *flight.FlightInfo, col int) (rows []string, sch *arrow.Schema) {
	t.Helper()
	for _, ep := range info.Endpoint {
		rdr, err := cl.DoGet(ctx, ep.Ticket)
		if err != nil {
			t.Fatal(err)
		}
		sch = rdr.Schema()
		for rdr.Next() {
			rec := rdr.RecordBatch()
			for i := 0; i < int(rec.NumRows()); i++ {
				rows = append(rows, strings.Clone(rec.Column(col).ValueStr(i)))
			}
		}
		if err := rdr.Err(); err != nil {
			t.Fatal(err)
		}
		rdr.Release()
	}
	return rows, sch
}

func TestStatement(t *testing.T) {
	cl := startServer(t, "")
	ctx := context.Background()
	tests := []struct {
		sql      string
		wantRows int
		wantV0   string
	}{
		{"SELECT 42 AS answer", 1, "42"},
		{"SELECT * FROM range(1000000)", 1000000, "0"},
	}
	for _, tt := range tests {
		t.Run(tt.sql, func(t *testing.T) {
			info, err := cl.Execute(ctx, tt.sql)
			if err != nil {
				t.Fatal(err)
			}
			rows, _ := fetch(ctx, t, cl, info, 0)
			if len(rows) != tt.wantRows || rows[0] != tt.wantV0 {
				t.Fatalf("rows=%d v0=%q", len(rows), rows[0])
			}
		})
	}
}

func TestStatement_Errors(t *testing.T) {
	cl := startServer(t, "")
	ctx := context.Background()
	info, err := cl.Execute(ctx, "SELEC 1")
	if err == nil {
		_, err = cl.DoGet(ctx, info.Endpoint[0].Ticket)
		if err == nil {
			t.Fatal("syntax error must fail")
		}
	}
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Errorf("syntax error code = %v", code)
	}

	// Ticket hanya bisa dipakai sekali.
	info, err = cl.Execute(ctx, "SELECT 1")
	if err != nil {
		t.Fatal(err)
	}
	fetch(ctx, t, cl, info, 0)
	rdr, err := cl.DoGet(ctx, info.Endpoint[0].Ticket)
	if err == nil {
		for rdr.Next() {
		}
		err = rdr.Err()
	}
	if code := status.Code(err); code != codes.NotFound {
		t.Errorf("reused ticket code = %v (err %v)", code, err)
	}
}

func TestUpdate(t *testing.T) {
	cl := startServer(t, "")
	ctx := context.Background()
	if _, err := cl.ExecuteUpdate(ctx, "CREATE TABLE t (i INTEGER, s VARCHAR)"); err != nil {
		t.Fatal(err)
	}
	n, err := cl.ExecuteUpdate(ctx, "INSERT INTO t SELECT range, 'x' FROM range(3)")
	if err != nil || n != 3 {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestPreparedStatement(t *testing.T) {
	cl := startServer(t, "CREATE TABLE t (i INTEGER, s VARCHAR)")
	ctx := context.Background()
	mem := memory.NewGoAllocator()

	params := func(i int64, s string) arrow.RecordBatch {
		sch := arrow.NewSchema([]arrow.Field{{Name: "i", Type: arrow.PrimitiveTypes.Int64}, {Name: "s", Type: arrow.BinaryTypes.String}}, nil)
		b := array.NewRecordBuilder(mem, sch)
		defer b.Release()
		b.Field(0).(*array.Int64Builder).Append(i)
		b.Field(1).(*array.StringBuilder).Append(s)
		return b.NewRecordBatch()
	}

	ins, err := cl.Prepare(ctx, "INSERT INTO t VALUES ($1, $2)")
	if err != nil {
		t.Fatal(err)
	}
	for i, s := range []string{"a", "b"} {
		rec := params(int64(i), s)
		ins.SetParameters(rec)
		if n, err := ins.ExecuteUpdate(ctx); err != nil || n != 1 {
			t.Fatalf("insert n=%d err=%v", n, err)
		}
		rec.Release()
	}
	if err := ins.Close(ctx); err != nil {
		t.Fatal(err)
	}

	sel, err := cl.Prepare(ctx, "SELECT s FROM t WHERE i = $1 AND s = $2")
	if err != nil {
		t.Fatal(err)
	}
	if ds := sel.DatasetSchema(); ds == nil || ds.NumFields() != 1 || ds.Field(0).Name != "s" {
		t.Fatalf("dataset schema = %v", ds)
	}
	defer func() { _ = sel.Close(ctx) }()
	rec := params(1, "b")
	defer rec.Release()
	sel.SetParameters(rec)
	info, err := sel.Execute(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rows, _ := fetch(ctx, t, cl, info, 0); len(rows) != 1 || rows[0] != "b" {
		t.Fatalf("rows %v", rows)
	}
}

func TestMetadata(t *testing.T) {
	cl := startServer(t, "CREATE SCHEMA s1; CREATE TABLE s1.orders (id INTEGER, note VARCHAR)")
	ctx := context.Background()
	s1, ord := "s1", "ord%"

	info, err := cl.GetCatalogs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rows, sch := fetch(ctx, t, cl, info, 0); !contains(rows, "memory") || sch.Field(0).Name != "catalog_name" {
		t.Errorf("catalogs %v schema %v", rows, sch)
	}

	info, err = cl.GetDBSchemas(ctx, &fsql.GetDBSchemasOpts{DbSchemaFilterPattern: &s1})
	if err != nil {
		t.Fatal(err)
	}
	if rows, sch := fetch(ctx, t, cl, info, 1); len(rows) != 1 || rows[0] != "s1" || sch.Field(1).Name != "db_schema_name" {
		t.Errorf("schemas %v", rows)
	}

	info, err = cl.GetTables(ctx, &fsql.GetTablesOpts{DbSchemaFilterPattern: &s1, TableNameFilterPattern: &ord, IncludeSchema: true})
	if err != nil {
		t.Fatal(err)
	}
	rdr, err := cl.DoGet(ctx, info.Endpoint[0].Ticket)
	if err != nil {
		t.Fatal(err)
	}
	defer rdr.Release()
	if !rdr.Next() {
		t.Fatalf("no tables: %v", rdr.Err())
	}
	rec := rdr.RecordBatch()
	if rec.NumRows() != 1 || rec.Column(2).ValueStr(0) != "orders" {
		t.Fatalf("tables rec %v", rec)
	}
	ts, err := flight.DeserializeSchema(rec.Column(4).(*array.Binary).Value(0), memory.DefaultAllocator)
	if err != nil || ts.NumFields() != 2 || ts.Field(1).Name != "note" {
		t.Fatalf("table_schema %v err %v", ts, err)
	}

	info, err = cl.GetTableTypes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rows, _ := fetch(ctx, t, cl, info, 0); !contains(rows, "VIEW") {
		t.Errorf("table types %v", rows)
	}

	info, err = cl.GetSqlInfo(ctx, []fsql.SqlInfo{fsql.SqlInfoFlightSqlServerName})
	if err != nil {
		t.Fatal(err)
	}
	if rows, _ := fetch(ctx, t, cl, info, 1); len(rows) != 1 || !strings.Contains(rows[0], "KarhutlaSQL") {
		t.Errorf("sql info %v", rows)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
