package flightsql

import (
	"strings"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/bankmega/karhutlasql/internal/audit"
)

func TestClassifyScan(t *testing.T) {
	g := startGov(t)
	const nik = "3171010506900001"
	if _, err := g.cl.ExecuteUpdate(bearer(g.tok["ops"]),
		"CREATE TABLE sandbox.main.pelanggan AS SELECT '"+nik+"' AS no_ktp, 'ani@bankmega.com' AS surel, 1 AS jumlah"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.rows("analyst", "SELECT * FROM karhutla_classify_scan()"); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("analyst scan: %v", err)
	}
	got, err := g.rows("ops", "SELECT table_name, column_name, suggested_tag, reason, current_tag FROM karhutla_classify_scan() ORDER BY 1, 2")
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(got, "\n")
	for _, want := range []string{
		"sandbox.main.pelanggan|no_ktp|pii.nik|pattern+name|(null)|",
		"sandbox.main.pelanggan|surel|pii.email|pattern+name|(null)|",
		"warehouse.main.nasabah|nama|pii.name|name|(null)|",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in\n%s", want, all)
		}
	}
	// Hanya usulan & rasio — nilai sampel tidak pernah keluar.
	if strings.Contains(all, nik) || strings.Contains(all, g.secret) || strings.Contains(all, "jumlah") {
		t.Errorf("scan leaked values or suggested a non-PII column:\n%s", all)
	}
	if ev := g.audit.ofType(audit.ClassificationScan); len(ev) != 1 || ev[0].User != "ops" || ev[0].Rows != int64(len(got)) {
		t.Errorf("audit = %+v", ev)
	}
}
