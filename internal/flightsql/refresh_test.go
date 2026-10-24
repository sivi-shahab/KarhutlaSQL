package flightsql

import (
	"encoding/json"
	"testing"

	"github.com/apache/arrow-go/v18/arrow/flight"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRefreshAction(t *testing.T) {
	g := startGov(t)
	refresh := func(user, table, sql string) (int64, error) {
		b, _ := json.Marshal(map[string]string{"table": table, "sql": sql})
		st, err := g.cl.Client.DoAction(bearer(g.tok[user]), &flight.Action{Type: ActionRefreshTable, Body: b})
		if err != nil {
			return 0, err
		}
		r, err := st.Recv()
		if err != nil {
			return 0, err
		}
		var out struct{ Rows int64 }
		return out.Rows, json.Unmarshal(r.Body, &out)
	}
	ctx := bearer(g.tok["analyst"])
	if _, err := g.cl.ExecuteUpdate(ctx, "CREATE TABLE sandbox.main.src AS SELECT 1 AS x"); err != nil {
		t.Fatal(err)
	}
	const mv, sel = "sandbox.main.mv", "SELECT x * 2 AS y FROM sandbox.main.src;"
	if n, err := refresh("analyst", mv, sel); err != nil || n != 1 {
		t.Fatalf("first refresh = %d %v", n, err)
	}
	if _, err := g.cl.ExecuteUpdate(ctx, "INSERT INTO sandbox.main.src VALUES (5)"); err != nil {
		t.Fatal(err)
	}
	if n, err := refresh("analyst", mv, sel); err != nil || n != 2 {
		t.Fatalf("second refresh = %d %v", n, err)
	}
	if got, err := g.rows("analyst", "SELECT sum(y) FROM sandbox.main.mv"); err != nil || got[0] != "12|" {
		t.Fatalf("mv = %v %v", got, err)
	}
	for _, c := range []struct {
		name, user, table, sql string
		code                   codes.Code
	}{
		{"no write on target", "analyst", "warehouse.main.mv", "SELECT 1 AS a", codes.PermissionDenied},
		{"masked source", "analyst", mv, "SELECT nik FROM warehouse.main.nasabah", codes.PermissionDenied},
		{"bad table name", "analyst", "sandbox.main.mv; DROP TABLE sandbox.main.src", "SELECT 1", codes.InvalidArgument},
		{"two-part name", "analyst", "main.mv", "SELECT 1", codes.InvalidArgument},
		{"empty sql", "analyst", mv, " ; ", codes.InvalidArgument},
	} {
		if _, err := refresh(c.user, c.table, c.sql); status.Code(err) != c.code {
			t.Errorf("%s: err = %v, want %s", c.name, err, c.code)
		}
	}
	// Statement kedua tidak pernah dieksekusi.
	if _, err := refresh("analyst", mv, "SELECT 1 AS y; DROP TABLE sandbox.main.src"); err == nil {
		t.Error("multi-statement refresh accepted")
	}
	if got, err := g.rows("analyst", "SELECT count(*) FROM sandbox.main.src"); err != nil || got[0] != "2|" {
		t.Fatalf("src after rejected refresh = %v %v", got, err)
	}
}
