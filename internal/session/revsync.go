package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/bankmega/karhutlasql/internal/objstore"
)

// RevocationPrefix awalan key revocation di arsip bersama.
const RevocationPrefix = "revocations/"

// RevocationSync berbagi kill session antar instance (topologi
// multi-instance, ADR 39) lewat arsip objek yang sudah dipakai bersama: satu
// objek per revocation di revocations/until=<tanggal>/, dipoll setiap
// interval. Objek WORM sekaligus jejak audit kill.
type RevocationSync struct {
	Store    objstore.Store
	Registry *Registry
	Instance string
	Now      func() time.Time

	mu   sync.Mutex
	seen map[string]bool
}

type revocation struct {
	SessionID string    `json:"session_id"`
	Until     time.Time `json:"until"`
	Instance  string    `json:"instance"`
	At        time.Time `json:"at"`
}

func (s *RevocationSync) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func key(id string, until time.Time) string {
	h := sha256.Sum256([]byte(id))
	return RevocationPrefix + "until=" + until.UTC().Format(time.DateOnly) + "/" + hex.EncodeToString(h[:16]) + ".json"
}

// Publish mencatat revocation id sampai until di arsip (dipasang lewat
// Registry.SetPublisher). Revocation yang sama sudah ada = sukses.
func (s *RevocationSync) Publish(ctx context.Context, id string, until time.Time) error {
	b, _ := json.Marshal(revocation{SessionID: id, Until: until.UTC(), Instance: s.Instance, At: s.now().UTC()}) // struct sederhana
	k := key(id, until)
	if err := s.Store.Put(ctx, k, b); err != nil && !errors.Is(err, objstore.ErrExists) {
		return fmt.Errorf("session.Publish: %w", err)
	}
	s.mu.Lock()
	if s.seen == nil {
		s.seen = map[string]bool{}
	}
	s.seen[k] = true
	s.mu.Unlock()
	return nil
}

// Sync menerapkan revocation baru dari instance lain yang belum kedaluwarsa
// (partisi until = hari ini dan sesudahnya; MinRevocation 24 jam dan TTL
// token ≤ 24 jam, ditambah kelonggaran).
func (s *RevocationSync) Sync(ctx context.Context) error {
	now := s.now().UTC()
	var errs []error
	for d := range 8 {
		prefix := RevocationPrefix + "until=" + now.AddDate(0, 0, d).Format(time.DateOnly) + "/"
		keys, err := s.Store.List(ctx, prefix)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, k := range keys {
			s.mu.Lock()
			done := s.seen[k]
			s.mu.Unlock()
			if done {
				continue
			}
			b, err := s.Store.Get(ctx, k)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			var r revocation
			if err := json.Unmarshal(b, &r); err != nil || r.SessionID == "" {
				errs = append(errs, fmt.Errorf("%s: invalid revocation", k))
			} else if r.Until.After(now) {
				s.Registry.Revoke(r.SessionID, r.Until)
			}
			s.mu.Lock()
			if s.seen == nil {
				s.seen = map[string]bool{}
			}
			s.seen[k] = true
			s.mu.Unlock()
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("session.Sync: %w", err)
	}
	return nil
}

// Run Sync setiap interval sampai ctx selesai; error dilaporkan ke onErr.
func (s *RevocationSync) Run(ctx context.Context, every time.Duration, onErr func(error)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Sync(ctx); err != nil {
				onErr(err)
			}
		}
	}
}
