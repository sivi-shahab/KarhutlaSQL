package quota

import (
	"errors"
	"testing"
	"time"

	"github.com/bankmega/karhutlasql/internal/auth"
)

func TestTracker(t *testing.T) {
	cfg := Config{TeamClaim: "team", Interval: time.Hour, Teams: map[string]Limits{
		"risk":      {MaxQueries: 2},
		DefaultTeam: {MaxExecSeconds: 10},
	}}
	tr := New(func() Config { return cfg })
	now := time.Date(2026, 10, 24, 9, 10, 0, 0, time.UTC)
	tr.now = func() time.Time { return now }
	risk := auth.Identity{Subject: "ana", Attrs: map[string]string{"team": "risk"}}
	other := auth.Identity{Subject: "x", Attrs: map[string]string{"team": "ops"}} // tidak terdaftar → batas default
	local := auth.Identity{Subject: "admin"}
	if tr.Team(risk) != "risk" || tr.Team(other) != "ops" || tr.Team(local) != DefaultTeam {
		t.Fatal("team resolution")
	}
	for range 2 {
		if err := tr.Check(risk); err != nil {
			t.Fatal(err)
		}
		tr.Add(risk, Usage{Queries: 1, ExecSeconds: 1, Rows: 5})
	}
	if err := tr.Check(risk); !errors.Is(err, ErrExceeded) {
		t.Fatalf("third query: %v", err)
	}
	tr.Add(other, Usage{Queries: 1, ExecSeconds: 12})
	if err := tr.Check(other); !errors.Is(err, ErrExceeded) {
		t.Errorf("default limit not applied to unlisted team: %v", err)
	}
	if err := tr.Check(local); err != nil { // tim default belum memakai apa pun
		t.Errorf("local: %v", err)
	}
	// Jendela baru → reset; total kumulatif tetap.
	now = now.Add(time.Hour)
	if err := tr.Check(risk); err != nil {
		t.Errorf("new window: %v", err)
	}
	rows := tr.Rows()
	if len(rows) != 2 || rows[1].Team != "risk" || rows[1].Total.Queries != 2 || rows[1].Window.Queries != 0 ||
		!rows[1].WindowStart.Equal(time.Date(2026, 10, 24, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("rows %+v", rows)
	}
	if (Config{Teams: map[string]Limits{"a": {}}}).Validate() == nil || (Config{Interval: time.Hour, Teams: map[string]Limits{"a": {MaxRows: -1}}}).Validate() == nil {
		t.Error("invalid config accepted")
	}
}
