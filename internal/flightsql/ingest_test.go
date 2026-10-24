//go:build duckdb_arrow

package flightsql

import (
	"context"
	"testing"
	"time"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	fsql "github.com/apache/arrow-go/v18/arrow/flight/flightsql"
	"github.com/apache/arrow-go/v18/arrow/memory"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func ingestReader(t *testing.T, rows int) array.RecordReader {
	t.Helper()
	sch := arrow.NewSchema([]arrow.Field{{Name: "x", Type: arrow.BinaryTypes.String}}, nil)
	b := array.NewStringBuilder(memory.DefaultAllocator)
	defer b.Release()
	for range rows {
		b.Append("v")
	}
	col := b.NewArray()
	defer col.Release()
	r, err := array.NewRecordReader(sch, []arrow.RecordBatch{array.NewRecordBatch(sch, []arrow.Array{col}, int64(rows))})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Bulk ingest (Fase 8d): izin write per tabel, profil SELECT-only ditolak,
// access history mencatat write; PollFlightInfo & GetSqlInfo.
func TestIngestAndPoll(t *testing.T) {
	g := startGov(t)
	opts := func(catalog, table string, exists fsql.TableDefinitionOptionsTableExistsOption) *fsql.ExecuteIngestOpts {
		return &fsql.ExecuteIngestOpts{Catalog: &catalog, Table: table, TableDefinitionOptions: &fsql.TableDefinitionOptions{
			IfNotExist: fsql.TableDefinitionOptionsTableNotExistOptionCreate, IfExists: exists}}
	}
	ctx := bearer(g.tok["analyst"])
	n, err := g.cl.ExecuteIngest(ctx, ingestReader(t, 100), opts("sandbox", "baru", fsql.TableDefinitionOptionsTableExistsOptionFail))
	if err != nil || n != 100 {
		t.Fatalf("create: %d %v", n, err)
	}
	if n, err = g.cl.ExecuteIngest(ctx, ingestReader(t, 5), opts("sandbox", "baru", fsql.TableDefinitionOptionsTableExistsOptionAppend)); err != nil || n != 5 {
		t.Fatalf("append: %d %v", n, err)
	}
	if _, err = g.cl.ExecuteIngest(ctx, ingestReader(t, 5), opts("sandbox", "baru", fsql.TableDefinitionOptionsTableExistsOptionFail)); status.Code(err) != codes.FailedPrecondition {
		t.Errorf("exists+fail: %v", err)
	}
	if got, _ := g.rows("analyst", "SELECT count(*) FROM sandbox.main.baru"); len(got) != 1 || got[0] != "105|" {
		t.Errorf("rows %v", got)
	}
	// Tanpa izin write pada warehouse; profil SELECT-only.
	if _, err = g.cl.ExecuteIngest(ctx, ingestReader(t, 1), opts("warehouse", "x", fsql.TableDefinitionOptionsTableExistsOptionFail)); status.Code(err) != codes.PermissionDenied {
		t.Errorf("analyst into warehouse: %v", err)
	}
	if _, err = g.cl.ExecuteIngest(bearer(g.tok["agent"]), ingestReader(t, 1), opts("sandbox", "y", fsql.TableDefinitionOptionsTableExistsOptionFail)); status.Code(err) != codes.PermissionDenied {
		t.Errorf("select-only agent: %v", err)
	}
	if err := g.access.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	var writes int
	recs, _ := g.access.Query(context.Background(), time.Now().Add(-time.Hour), time.Now().Add(time.Hour))
	for _, r := range recs {
		if r.Action == "write" && r.Table == "sandbox.main.baru" && r.Status == "OK" {
			writes++
		}
	}
	if writes != 2 {
		t.Errorf("access history writes = %d, want 2 (%v)", writes, len(recs))
	}

	// PollFlightInfo: selesai pada poll pertama, tiket bisa di-DoGet.
	pi, err := g.cl.ExecutePoll(ctx, "SELECT count(*) FROM sandbox.main.baru", nil)
	if err != nil || pi.GetProgress() != 1 || pi.GetFlightDescriptor() != nil || len(pi.GetInfo().GetEndpoint()) != 1 {
		t.Fatalf("poll %v %v", pi, err)
	}
	rdr, err := g.cl.DoGet(ctx, pi.GetInfo().GetEndpoint()[0].GetTicket())
	if err != nil || !rdr.Next() || rdr.RecordBatch().Column(0).ValueStr(0) != "105" {
		t.Fatalf("doget after poll: %v", err)
	}
	rdr.Release()

	// GetSqlInfo: bulk ingest diumumkan, keyword DuckDB terisi.
	info, err := g.cl.GetSqlInfo(ctx, []fsql.SqlInfo{fsql.SqlInfoFlightSqlServerBulkIngestion, fsql.SqlInfoKeywords})
	if err != nil {
		t.Fatal(err)
	}
	sr, err := g.cl.DoGet(ctx, info.Endpoint[0].Ticket)
	if err != nil {
		t.Fatal(err)
	}
	defer sr.Release()
	var got int
	for sr.Next() {
		got += int(sr.RecordBatch().NumRows())
	}
	if got != 2 {
		t.Errorf("sql info rows %d, want 2", got)
	}
}
