// Package quota kuota pemakaian per tim per jendela waktu dan penghitung
// chargeback (Fase 8b). Tim = nilai claim identitas (policy quotas.team_claim).
// Penghitung di memori per instance; jendela berjalan direset saat restart,
// total kumulatif diekspor ke Prometheus untuk laporan chargeback.
package quota

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/bankmega/karhutlasql/internal/auth"
)

// DefaultTeam tim identitas tanpa claim tim (termasuk user lokal); batasnya
// juga dipakai tim yang tidak terdaftar di Teams.
const DefaultTeam = "default"

// Limits batas per jendela; 0 = tanpa batas.
type Limits struct {
	MaxQueries     int64   `yaml:"max_queries"`
	MaxExecSeconds float64 `yaml:"max_exec_seconds"`
	MaxRows        int64   `yaml:"max_rows"`
	MaxBytes       int64   `yaml:"max_bytes"`
}

// Config bagian quotas di policy.yaml.
type Config struct {
	TeamClaim string            `yaml:"team_claim"`
	Interval  time.Duration     `yaml:"interval"`
	Teams     map[string]Limits `yaml:"teams"`
}

// Validate memeriksa konfigurasi.
func (c Config) Validate() error {
	var errs []error
	if len(c.Teams) > 0 && c.Interval <= 0 {
		errs = append(errs, errors.New("quotas.interval must be > 0 when teams are set"))
	}
	for name, l := range c.Teams {
		if l.MaxQueries < 0 || l.MaxExecSeconds < 0 || l.MaxRows < 0 || l.MaxBytes < 0 {
			errs = append(errs, fmt.Errorf("quotas.teams.%s: limits must be >= 0", name))
		}
	}
	return errors.Join(errs...)
}

// Usage pemakaian.
type Usage struct {
	Queries     int64
	ExecSeconds float64
	Rows, Bytes int64
}

func (u *Usage) add(v Usage) {
	u.Queries += v.Queries
	u.ExecSeconds += v.ExecSeconds
	u.Rows += v.Rows
	u.Bytes += v.Bytes
}

// ErrExceeded kuota jendela berjalan habis.
var ErrExceeded = errors.New("quota exceeded")

// Tracker penghitung per tim.
type Tracker struct {
	cfg func() Config
	now func() time.Time

	mu     sync.Mutex
	start  time.Time         // awal jendela berjalan
	window map[string]*Usage // jendela berjalan
	total  map[string]*Usage // sejak proses mulai (chargeback)
}

// New Tracker; cfg dibaca setiap pemanggilan (hot reload policy).
func New(cfg func() Config) *Tracker {
	return &Tracker{cfg: cfg, now: time.Now, window: map[string]*Usage{}, total: map[string]*Usage{}}
}

// Team tim identitas.
func (t *Tracker) Team(id auth.Identity) string {
	if c := t.cfg().TeamClaim; c != "" && id.Attrs[c] != "" {
		return id.Attrs[c]
	}
	return DefaultTeam
}

// roll memulai jendela baru bila jendela berjalan sudah lewat (mu terkunci).
func (t *Tracker) roll(c Config) {
	if c.Interval <= 0 {
		return
	}
	if start := t.now().UTC().Truncate(c.Interval); !start.Equal(t.start) {
		t.start, t.window = start, map[string]*Usage{}
	}
}

// Check menolak (ErrExceeded) bila pemakaian tim id di jendela berjalan sudah
// mencapai salah satu batas. Statement yang sedang berjalan boleh melewati
// batas; statement berikutnya ditolak.
func (t *Tracker) Check(id auth.Identity) error {
	c := t.cfg()
	team := t.Team(id)
	l, ok := c.Teams[team]
	if !ok {
		l = c.Teams[DefaultTeam]
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.roll(c)
	u := t.window[team]
	if u == nil {
		return nil
	}
	for _, x := range []struct {
		name      string
		used, max float64
	}{
		{"max_queries", float64(u.Queries), float64(l.MaxQueries)},
		{"max_exec_seconds", u.ExecSeconds, l.MaxExecSeconds},
		{"max_rows", float64(u.Rows), float64(l.MaxRows)},
		{"max_bytes", float64(u.Bytes), float64(l.MaxBytes)},
	} {
		if x.max > 0 && x.used >= x.max {
			return fmt.Errorf("%w: team %s reached %s (%g) for the window starting %s",
				ErrExceeded, team, x.name, x.max, t.start.Format(time.RFC3339))
		}
	}
	return nil
}

// Add mencatat pemakaian satu statement.
func (t *Tracker) Add(id auth.Identity, u Usage) {
	c := t.cfg()
	team := t.Team(id)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.roll(c)
	for _, m := range []map[string]*Usage{t.window, t.total} {
		if m[team] == nil {
			m[team] = &Usage{}
		}
		m[team].add(u)
	}
}

// Row pemakaian satu tim: jendela berjalan dan total sejak start.
type Row struct {
	Team        string
	WindowStart time.Time // zero bila tanpa interval
	Window      Usage
	Total       Usage
}

// Rows pemakaian semua tim, urut nama.
func (t *Tracker) Rows() []Row {
	c := t.cfg()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.roll(c)
	out := make([]Row, 0, len(t.total))
	for _, team := range slices.Sorted(maps.Keys(t.total)) {
		r := Row{Team: team, WindowStart: t.start, Total: *t.total[team]}
		if w := t.window[team]; w != nil {
			r.Window = *w
		}
		out = append(out, r)
	}
	return out
}

var usageDesc = map[string]*prometheus.Desc{}

func init() {
	for _, n := range []string{"queries", "exec_seconds", "rows", "bytes"} {
		usageDesc[n] = prometheus.NewDesc("karhutla_team_"+n+"_total", "Pemakaian kumulatif per tim sejak start (chargeback).", []string{"team"}, nil)
	}
}

// Describe prometheus.Collector.
func (t *Tracker) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range usageDesc {
		ch <- d
	}
}

// Collect prometheus.Collector.
func (t *Tracker) Collect(ch chan<- prometheus.Metric) {
	for _, r := range t.Rows() {
		for n, v := range map[string]float64{"queries": float64(r.Total.Queries), "exec_seconds": r.Total.ExecSeconds,
			"rows": float64(r.Total.Rows), "bytes": float64(r.Total.Bytes)} {
			ch <- prometheus.MustNewConstMetric(usageDesc[n], prometheus.CounterValue, v, r.Team)
		}
	}
}
