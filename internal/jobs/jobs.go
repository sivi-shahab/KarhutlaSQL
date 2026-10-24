// Package jobs menjalankan job terjadwal (backlog fase 9b): report run dan
// refresh tabel materialisasi menurut jadwal cron. Package ini hanya
// menjadwalkan; eksekusi (identitas "job:<name>", otorisasi, antrean, audit)
// dilakukan Exec yang disuntikkan, yaitu panggilan Flight SQL in-process
// seperti klien biasa.
package jobs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"go.yaml.in/yaml/v3"
)

// Job satu job dari --jobs-file. Tepat satu dari Report / Refresh.
type Job struct {
	Name     string   `yaml:"-"`
	Schedule string   `yaml:"schedule"`
	Report   string   `yaml:"report"`
	Refresh  *Refresh `yaml:"refresh"`

	sched cron.Schedule
}

// Refresh tabel materialisasi: Table dibangun ulang dari SQL setiap jalan.
type Refresh struct {
	Table string `yaml:"table"` // catalog.schema.table
	SQL   string `yaml:"sql"`
}

var (
	namePattern  = regexp.MustCompile(`^[a-z0-9_]+$`)
	identPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Load membaca file; path kosong = tanpa job.
func Load(path string) ([]Job, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("jobs.Load: %w", err)
	}
	var f struct {
		Jobs map[string]Job `yaml:"jobs"`
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("jobs.Load: %w", err)
	}
	var out []Job
	var errs []error
	for _, name := range slices.Sorted(maps.Keys(f.Jobs)) {
		j := f.Jobs[name]
		j.Name = name
		if err := j.validate(); err != nil {
			errs = append(errs, fmt.Errorf("job %q: %w", name, err))
			continue
		}
		out = append(out, j)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, fmt.Errorf("jobs.Load: %w", err)
	}
	return out, nil
}

func (j *Job) validate() error {
	if !namePattern.MatchString(j.Name) {
		return errors.New("name must match [a-z0-9_]+")
	}
	s, err := cron.ParseStandard(j.Schedule)
	if err != nil {
		return fmt.Errorf("schedule: %w", err)
	}
	j.sched = s
	switch {
	case (j.Report == "") == (j.Refresh == nil):
		return errors.New("exactly one of report or refresh is required")
	case j.Report != "" && !namePattern.MatchString(j.Report):
		return errors.New("report: name must match [a-z0-9_]+")
	case j.Refresh != nil:
		parts := strings.Split(j.Refresh.Table, ".")
		if len(parts) != 3 || slices.ContainsFunc(parts, func(p string) bool { return !identPattern.MatchString(p) }) {
			return errors.New("refresh.table: want catalog.schema.table (letters, digits, _)")
		}
		if strings.TrimSpace(j.Refresh.SQL) == "" {
			return errors.New("refresh.sql is required")
		}
	}
	return nil
}

// Kind "report" atau "refresh".
func (j Job) Kind() string {
	if j.Refresh != nil {
		return "refresh"
	}
	return "report"
}

// Exec menjalankan job sebagai identitas job dan mengembalikan jumlah baris
// (hasil report / baris tabel refresh).
type Exec func(ctx context.Context, job Job) (int64, error)

// State keadaan satu job (karhutla_jobs()).
type State struct {
	Job        Job
	Next       time.Time
	Running    bool
	LastStart  time.Time
	LastEnd    time.Time
	LastStatus string // "" (belum pernah), "OK", "FAILED", "SKIPPED"
	LastError  string
	LastRows   int64
}

// Runner penjadwal job. Satu job tidak pernah berjalan tumpang tindih:
// jadwal yang jatuh saat job masih berjalan dicatat SKIPPED.
type Runner struct {
	exec Exec
	now  func() time.Time

	mu     sync.Mutex
	ctx    context.Context // ctx Run; nil = runner belum berjalan
	states []*State
	wg     sync.WaitGroup
}

// NewRunner membuat runner untuk jobs.
func NewRunner(jobs []Job, exec Exec) *Runner {
	r := &Runner{exec: exec, now: time.Now}
	for _, j := range jobs {
		r.states = append(r.states, &State{Job: j})
	}
	return r
}

// Run menjadwalkan sampai ctx selesai, lalu menunggu job yang berjalan.
func (r *Runner) Run(ctx context.Context) {
	defer r.wg.Wait()
	r.mu.Lock()
	r.ctx = ctx
	for _, s := range r.states {
		s.Next = s.Job.sched.Next(r.now())
	}
	r.mu.Unlock()
	for {
		r.mu.Lock()
		var next time.Time
		for _, s := range r.states {
			if next.IsZero() || s.Next.Before(next) {
				next = s.Next
			}
		}
		r.mu.Unlock()
		wait := time.Hour // tanpa job: hanya Trigger
		if !next.IsZero() {
			wait = max(time.Until(next), 0)
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		now := r.now()
		r.mu.Lock()
		for _, s := range r.states {
			if !s.Next.IsZero() && !s.Next.After(now) {
				r.startLocked(ctx, s)
				s.Next = s.Job.sched.Next(now)
			}
		}
		r.mu.Unlock()
	}
}

// Trigger menjalankan job name sekarang (di luar jadwal) dengan ctx Run.
// Error bila runner belum berjalan, job tidak ada, atau sedang berjalan.
func (r *Runner) Trigger(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ctx == nil {
		return errors.New("jobs.Trigger: runner is not running")
	}
	for _, s := range r.states {
		if s.Job.Name == name {
			if s.Running {
				return fmt.Errorf("jobs.Trigger: job %q is already running", name)
			}
			r.startLocked(r.ctx, s)
			return nil
		}
	}
	return fmt.Errorf("jobs.Trigger: unknown job %q", name)
}

// startLocked; r.mu dipegang.
func (r *Runner) startLocked(ctx context.Context, s *State) {
	if s.Running {
		s.LastStatus, s.LastError = "SKIPPED", "previous run still running"
		return
	}
	s.Running, s.LastStart = true, r.now()
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		rows, err := r.exec(ctx, s.Job)
		r.mu.Lock()
		defer r.mu.Unlock()
		s.Running, s.LastEnd, s.LastRows, s.LastError = false, r.now(), rows, ""
		s.LastStatus = "OK"
		if err != nil {
			s.LastStatus, s.LastError = "FAILED", err.Error()
		}
	}()
}

// Snapshot salinan keadaan semua job, urut nama.
func (r *Runner) Snapshot() []State {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]State, len(r.states))
	for i, s := range r.states {
		out[i] = *s
	}
	return out
}
