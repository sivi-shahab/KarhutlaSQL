package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/bankmega/karhutlasql/internal/auth"
)

// Tipe event.
const (
	LoginOK        = "login_ok"
	LoginFail      = "login_fail"
	StatementStart = "statement_start"
	StatementEnd   = "statement_end"
	SessionKilled  = "session_killed"
)

// Event satu baris audit (JSON). Field kosong dihilangkan.
type Event struct {
	Time       time.Time `json:"ts"`
	Type       string    `json:"type"`
	RequestID  string    `json:"request_id,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	User       string    `json:"user,omitempty"`
	Source     string    `json:"source,omitempty"`
	Peer       string    `json:"peer,omitempty"`
	Kind       string    `json:"kind,omitempty"` // query | update
	SQLHash    string    `json:"sql_sha256,omitempty"`
	SQL        string    `json:"sql,omitempty"` // hanya jika --audit-log-sql
	Status     string    `json:"status,omitempty"`
	Rows       int64     `json:"rows,omitempty"`
	DurationMS int64     `json:"duration_ms,omitempty"`
	Error      string    `json:"error,omitempty"`
	Target     string    `json:"target,omitempty"` // session_killed: ID sesi yang dimatikan
	Via        string    `json:"via,omitempty"`    // session_killed: sql | http
}

// Sink tujuan event audit. Emit harus aman dipanggil konkuren.
type Sink interface {
	Emit(Event) error
}

// Logger menulis event ke Sink dan memutuskan teks SQL ikut atau hanya hash.
type Logger struct {
	sink    Sink
	withSQL bool
	onErr   func(error)
}

// New membuat Logger. onErr dipanggil saat sink gagal (mis. log + metric).
func New(sink Sink, withSQL bool, onErr func(error)) *Logger {
	return &Logger{sink: sink, withSQL: withSQL, onErr: onErr}
}

// Emit mengisi waktu & request id lalu menulis event.
func (l *Logger) Emit(ctx context.Context, e Event) {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	if e.RequestID == "" {
		e.RequestID = RequestID(ctx)
	}
	if id, ok := auth.FromContext(ctx); ok && e.SessionID == "" {
		e.SessionID = id.TokenID
	}
	if err := l.sink.Emit(e); err != nil && l.onErr != nil {
		l.onErr(err)
	}
}

// Redact mengembalikan sql apa adanya bila --audit-log-sql, selain itu
// "sha256:<hex>" (dipakai di luar audit, mis. karhutla_sessions()).
func (l *Logger) Redact(sql string) string {
	if l.withSQL {
		return sql
	}
	sum := sha256.Sum256([]byte(sql))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// SQL mengisi SQLHash (selalu) dan SQL (jika diizinkan) pada e.
func (l *Logger) SQL(e *Event, sql string) {
	sum := sha256.Sum256([]byte(sql))
	e.SQLHash = hex.EncodeToString(sum[:])
	if l.withSQL {
		e.SQL = sql
	}
}

type reqIDKey struct{}

// WithRequestID menyimpan request id di ctx.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, reqIDKey{}, id)
}

// RequestID request id dari ctx ("" jika tidak ada).
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(reqIDKey{}).(string)
	return id
}

// WriterSink menulis satu JSON per baris ke w (mis. stdout).
type WriterSink struct {
	mu sync.Mutex
	w  io.Writer
}

// NewWriterSink membuat WriterSink.
func NewWriterSink(w io.Writer) *WriterSink { return &WriterSink{w: w} }

// Emit lihat Sink.
func (s *WriterSink) Emit(e Event) error {
	b, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("audit.Emit: %w", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.w.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("audit.Emit: %w", err)
	}
	return nil
}

// FileSink menulis append-only ke <dir>/audit-YYYY-MM-DD.jsonl (mode 0600),
// berganti file setiap hari UTC (arsitektur §7: rotasi harian).
type FileSink struct {
	mu   sync.Mutex
	root *os.Root
	day  string
	f    *os.File
	sink *WriterSink
}

// NewFileSink membuat FileSink di dir (dibuat 0700 jika belum ada).
func NewFileSink(dir string) (*FileSink, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("audit.NewFileSink: %w", err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("audit.NewFileSink: %w", err)
	}
	return &FileSink{root: root}, nil
}

// Emit lihat Sink.
func (s *FileSink) Emit(e Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	day := e.Time.UTC().Format("2006-01-02")
	if day != s.day {
		f, err := s.root.OpenFile("audit-"+day+".jsonl", os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return fmt.Errorf("audit.Emit: %w", err)
		}
		if s.f != nil {
			_ = s.f.Close()
		}
		s.f, s.day, s.sink = f, day, NewWriterSink(f)
	}
	return s.sink.Emit(e)
}

// Close menutup file aktif.
func (s *FileSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var err error
	if s.f != nil {
		err = s.f.Close()
	}
	return errors.Join(err, s.root.Close())
}
