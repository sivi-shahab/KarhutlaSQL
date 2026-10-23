// Package session mengelola registry sesi, siklus hidup statement handle dan
// revocation. Satu sesi = satu token (ID sesi = jti); kill session membatalkan
// statement aktif dan me-revoke token sampai kedaluwarsa.
package session

import (
	"context"
	"errors"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bankmega/karhutlasql/internal/auth"
)

// ErrRevoked token sesi sudah di-kill.
var ErrRevoked = errors.New("session killed")

// MinRevocation lama minimum token sesi yang di-kill tetap ditolak.
const MinRevocation = 24 * time.Hour

// ErrKilled cause pembatalan statement oleh kill session.
var ErrKilled = errors.New("session killed by administrator")

// Registry sesi in-memory.
type Registry struct {
	idle time.Duration
	now  func() time.Time

	mu       sync.Mutex
	sessions map[string]*Session
	revoked  map[string]time.Time // jti → exp token
}

// NewRegistry membuat registry; sesi tanpa statement aktif yang idle lebih
// lama dari idle dibuang oleh Sweep (statement/prepared handle-nya ikut hilang).
func NewRegistry(idle time.Duration) *Registry {
	return &Registry{idle: idle, now: time.Now, sessions: map[string]*Session{}, revoked: map[string]time.Time{}}
}

// Session satu sesi klien.
type Session struct {
	ID, User, Source, Peer string
	StartedAt              time.Time
	exp                    time.Time // exp token

	Rows, Bytes atomic.Int64 // dikirim ke klien

	mu         sync.Mutex
	lastActive time.Time
	nextStmt   uint64
	stmts      map[uint64]*statement
	handles    map[string]any
}

type statement struct {
	sql       string
	startedAt time.Time
	cancel    context.CancelCauseFunc
}

// Attach mengembalikan sesi milik id (dibuat bila belum ada) dan menandai
// aktivitas. ErrRevoked bila token sesi sudah di-kill.
func (r *Registry) Attach(id auth.Identity, peer string) (*Session, error) {
	if id.TokenID == "" || id.ExpiresAt.IsZero() {
		return nil, errors.New("session.Attach: identity without token id or expiry")
	}
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.revoked[id.TokenID]; ok {
		return nil, ErrRevoked
	}
	s, ok := r.sessions[id.TokenID]
	if !ok {
		s = &Session{ID: id.TokenID, User: id.Subject, Source: id.Source, Peer: peer, StartedAt: now, exp: id.ExpiresAt,
			stmts: map[uint64]*statement{}, handles: map[string]any{}}
		r.sessions[id.TokenID] = s
	}
	s.mu.Lock()
	s.lastActive = now
	s.mu.Unlock()
	return s, nil
}

// Revoked melaporkan apakah token dengan jti tersebut sudah di-kill.
func (r *Registry) Revoked(tokenID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.revoked[tokenID]
	return ok
}

// Kill membatalkan semua statement aktif sesi id dan me-revoke tokennya sampai
// kedaluwarsa. false bila sesi tidak ada di registry.
func (r *Registry) Kill(id string) bool {
	r.mu.Lock()
	s, ok := r.sessions[id]
	if ok {
		delete(r.sessions, id)
		// Minimal 24 jam: token OIDC baru hasil refresh membawa sid yang sama
		// dengan exp lebih lama (spec §6: TTL token ≤ 24h).
		until := r.now().Add(MinRevocation)
		if s.exp.After(until) {
			until = s.exp
		}
		r.revoked[id] = until
	}
	r.mu.Unlock()
	if !ok {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.stmts {
		st.cancel(ErrKilled)
	}
	clear(s.handles)
	return true
}

// Sweep membuang sesi idle dan revocation yang tokennya sudah kedaluwarsa.
func (r *Registry) Sweep() {
	now := r.now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, exp := range r.revoked {
		if now.After(exp) {
			delete(r.revoked, id)
		}
	}
	for id, s := range r.sessions {
		s.mu.Lock()
		idle := len(s.stmts) == 0 && now.Sub(s.lastActive) > r.idle
		s.mu.Unlock()
		if idle || now.After(s.exp) {
			delete(r.sessions, id)
		}
	}
}

// Run memanggil Sweep berkala sampai ctx selesai.
func (r *Registry) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.Sweep()
		}
	}
}

// Info snapshot satu sesi.
type Info struct {
	ID, User, Source, Peer string
	StartedAt, LastActive  time.Time
	ActiveStatements       int
	// SQL & SQLStartedAt statement aktif paling lama (kosong bila idle).
	SQL          string
	SQLStartedAt time.Time
	Rows, Bytes  int64
}

// Snapshot daftar sesi, urut StartedAt.
func (r *Registry) Snapshot() []Info {
	r.mu.Lock()
	out := make([]Info, 0, len(r.sessions))
	for _, s := range r.sessions {
		out = append(out, s.info())
	}
	r.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// Counts jumlah sesi per source dan statement aktif (untuk metrics).
func (r *Registry) Counts() (bySource map[string]int, statements int) {
	bySource = map[string]int{}
	for _, in := range r.Snapshot() {
		bySource[in.Source]++
		statements += in.ActiveStatements
	}
	return bySource, statements
}

func (s *Session) info() Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	in := Info{ID: s.ID, User: s.User, Source: s.Source, Peer: s.Peer, StartedAt: s.StartedAt,
		LastActive: s.lastActive, ActiveStatements: len(s.stmts), Rows: s.Rows.Load(), Bytes: s.Bytes.Load()}
	for _, st := range s.stmts {
		if in.SQL == "" || st.startedAt.Before(in.SQLStartedAt) {
			in.SQL, in.SQLStartedAt = st.sql, st.startedAt
		}
	}
	return in
}

// Begin mendaftarkan statement aktif; ctx hasil dibatalkan oleh Kill (cause
// ErrKilled). done wajib dipanggil saat statement selesai.
func (s *Session) Begin(ctx context.Context, sql string) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	s.mu.Lock()
	s.nextStmt++
	id := s.nextStmt
	s.stmts[id] = &statement{sql: sql, startedAt: time.Now(), cancel: cancel}
	s.mu.Unlock()
	return ctx, func() {
		s.mu.Lock()
		delete(s.stmts, id)
		s.lastActive = time.Now()
		s.mu.Unlock()
		cancel(nil)
	}
}

// Put menyimpan handle (statement/prepared) milik sesi.
func (s *Session) Put(handle string, v any) {
	s.mu.Lock()
	s.handles[handle] = v
	s.mu.Unlock()
}

// Get mengambil handle; false bila tidak ada (atau milik sesi lain).
func (s *Session) Get(handle string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.handles[handle]
	return v, ok
}

// Take mengambil lalu menghapus handle.
func (s *Session) Take(handle string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.handles[handle]
	delete(s.handles, handle)
	return v, ok
}

// Delete menghapus handle.
func (s *Session) Delete(handle string) {
	s.mu.Lock()
	delete(s.handles, handle)
	s.mu.Unlock()
}

type ctxKey struct{}

// WithSession menyimpan s di ctx.
func WithSession(ctx context.Context, s *Session) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// FromContext sesi dari ctx (nil bila tidak ada).
func FromContext(ctx context.Context) *Session {
	s, _ := ctx.Value(ctxKey{}).(*Session)
	return s
}
