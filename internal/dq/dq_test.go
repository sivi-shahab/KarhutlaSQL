package dq

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bankmega/karhutlasql/internal/objstore"
)

// Fixture dihasilkan GX Core 1.24 (result_format=COMPLETE) atas data berisi
// NIK palsu; semua nilai baris harus hilang setelah sanitasi.
var fakeValues = []string{"3171000000000001", "3171999999999999X", "-5.0", "Traceback", "df.filter", "value_set"}

func TestSanitizeGolden(t *testing.T) {
	raw, err := os.ReadFile("testdata/gx_result_complete.json")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "3171999999999999X") {
		t.Fatal("fixture harus memuat NIK palsu")
	}
	v, err := Sanitize(raw, func(c string) bool { return c == "nik" })
	if err != nil {
		t.Fatal(err)
	}
	v.Table, v.SnapshotID, v.RunBy = "warehouse.main.nasabah", "s1", "oidc:svc_gx"
	data, err := encode(v)
	if err != nil {
		t.Fatal(err)
	}
	back, err := decode(context.Background(), data)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := json.Marshal(back)
	for _, s := range fakeValues {
		if strings.Contains(string(all), s) {
			t.Errorf("hasil sanitasi memuat %q: %s", s, all)
		}
	}
	got := back[0]
	if got.Success || got.Evaluated != 7 || got.FailedCritical != 3 || got.FailedWarning != 2 || got.Suite != "warehouse.main.nasabah" {
		t.Errorf("ringkasan = %+v", got)
	}
	var exps []Expectation
	if err := json.Unmarshal([]byte(got.Results), &exps); err != nil {
		t.Fatal(err)
	}
	want := map[string]map[string]any{
		"expect_column_values_to_match_regex":        {"element_count": 3.0, "unexpected_count": 1.0, "unexpected_percent": 50.0, "missing_count": 1.0, "missing_percent": 100.0 / 3, "unexpected_percent_total": 100.0 / 3, "unexpected_percent_nonmissing": 50.0},
		"expect_column_mean_to_be_between":           {"observed_value": 38.333333333333336},
		"expect_table_row_count_to_be_between":       {"observed_value": 3.0},
		"expect_column_max_to_be_between":            nil, // exception: traceback dibuang
		"expect_column_values_to_be_between":         {"element_count": 3.0, "unexpected_count": 1.0, "unexpected_percent": 100.0 / 3, "missing_count": 0.0, "missing_percent": 0.0, "unexpected_percent_total": 100.0 / 3, "unexpected_percent_nonmissing": 100.0 / 3},
		"expect_column_values_to_not_be_null":        {"element_count": 3.0, "unexpected_count": 1.0, "unexpected_percent": 100.0 / 3},
		"expect_column_distinct_values_to_be_in_set": {"unexpected_count": 1.0},
	}
	for _, e := range exps {
		w, ok := want[e.Type]
		if !ok {
			t.Errorf("tipe tak terduga %s", e.Type)
			continue
		}
		if len(e.Result) != len(w) {
			t.Errorf("%s result = %v, want %v", e.Type, e.Result, w)
		}
		for k, wv := range w {
			if gv, _ := e.Result[k].(float64); gv-wv.(float64) > 1e-9 || wv.(float64)-gv > 1e-9 {
				t.Errorf("%s.%s = %v, want %v", e.Type, k, e.Result[k], wv)
			}
		}
	}
}

func TestSanitizeObservedValueOfTaggedColumn(t *testing.T) {
	raw := `{"success":true,"results":[
	 {"success":true,"expectation_config":{"type":"expect_column_max_to_be_between","kwargs":{"column":"nik"}},"result":{"observed_value":3171000000000001}},
	 {"success":true,"expectation_config":{"type":"expect_column_max_to_be_between","kwargs":{"column":"saldo"}},"result":{"observed_value":"3171000000000001"}},
	 {"success":true,"expectation_config":{"type":"expect_column_values_to_be_unique","kwargs":{"column":"x"}},"result":{"observed_value":true}},
	 {"success":false,"expectation_config":{"type":"expect_column_values_to_be_unique","severity":"warning"},"result":{}}]}`
	v, err := Sanitize([]byte(raw), func(c string) bool { return c == "nik" })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(v.Results, "3171") || !strings.Contains(v.Results, `"observed_value":true`) || v.FailedWarning != 1 || v.FailedCritical != 0 {
		t.Errorf("results = %s", v.Results)
	}
}

func TestSanitizeRejects(t *testing.T) {
	for _, raw := range []string{
		`not json`, `{}`, `{"success":true,"results":[]}`,
		`{"success":true,"results":[{"expectation_config":{"type":"expect_x"}}]}`,
		`{"success":true,"results":[{"success":true,"expectation_config":{"type":"Bad Type 3171"}}]}`,
	} {
		if _, err := Sanitize([]byte(raw), nil); !errors.Is(err, ErrInvalid) {
			t.Errorf("Sanitize(%s) err = %v, want ErrInvalid", raw, err)
		}
	}
}

func TestStoreGate(t *testing.T) {
	ctx := context.Background()
	dir := objstore.Dir(t.TempDir())
	s, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 24, 1, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	pub := func(table, snap string, crit int64, by string) Validation {
		v, err := s.Publish(ctx, Validation{Table: table, SnapshotID: snap, FailedCritical: crit, Success: crit == 0, RunBy: by, Results: "[]"})
		if err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Second)
		return v
	}
	good := pub("w.main.a", "s1", 0, "oidc:gx")
	pub("w.main.b", "s1", 2, "oidc:gx")
	now = now.Add(time.Hour)

	cases := []struct {
		name   string
		snaps  map[string]string
		maxAge time.Duration
		runner string
		ok     bool
		msg    string
	}{
		{"lolos", map[string]string{"w.main.a": "s1"}, 2 * time.Hour, "oidc:dian", true, ""},
		{"tanpa validasi", map[string]string{"w.main.c": "s1"}, 2 * time.Hour, "oidc:dian", false, "no validation"},
		{"snapshot beda", map[string]string{"w.main.a": "s2"}, 2 * time.Hour, "oidc:dian", false, "is for snapshot s1"},
		{"critical gagal", map[string]string{"w.main.b": "s1"}, 2 * time.Hour, "oidc:dian", false, "2 failed critical"},
		{"kedaluwarsa", map[string]string{"w.main.a": "s1"}, 30 * time.Minute, "oidc:dian", false, "older than"},
		{"pemisahan tugas", map[string]string{"w.main.a": "s1"}, 2 * time.Hour, "oidc:gx", false, "separation of duties"},
	}
	for _, c := range cases {
		ids, err := s.Gate(c.snaps, c.maxAge, c.runner)
		if c.ok != (err == nil) || (err != nil && (!errors.Is(err, ErrBlocked) || !strings.Contains(err.Error(), c.msg))) {
			t.Errorf("%s: err = %v", c.name, err)
		}
		if c.ok && (len(ids) != 1 || ids[0] != good.ID) {
			t.Errorf("%s: ids = %v", c.name, ids)
		}
	}

	// Validasi terbaru menang: gagal setelah lolos → diblok.
	pub("w.main.a", "s1", 1, "oidc:gx")
	if _, err := s.Gate(map[string]string{"w.main.a": "s1"}, 2*time.Hour, "oidc:dian"); err == nil {
		t.Error("validasi terbaru gagal harus memblok")
	}
	// Riwayat dimuat ulang dari arsip.
	s2, err := Open(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := s2.All(); len(got) != 3 || got[0].ID != good.ID || !got[0].ValidatedAt.Equal(good.ValidatedAt) || got[0].RunBy != "oidc:gx" {
		t.Errorf("reload = %+v", got)
	}
}
