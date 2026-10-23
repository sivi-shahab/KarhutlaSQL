// Package approval menegakkan four-eyes (G-11) untuk dokumen governance
// (policy.yaml, classification.yaml): isi file yang berubah menjadi pending
// dan baru aktif setelah disetujui dua identitas berbeda. Isi yang disetujui
// disimpan di state dir (0600) sehingga restart tidak mengaktifkan file yang
// belum disetujui.
package approval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"time"
)

// Required jumlah persetujuan berbeda agar dokumen aktif.
const Required = 2

// Bootstrap approver pseudo saat state belum ada (isi file pertama).
const Bootstrap = "bootstrap"

var (
	// ErrUnknown dokumen tidak terdaftar.
	ErrUnknown = errors.New("unknown document")
	// ErrNoPending tidak ada perubahan yang menunggu atau hash tidak cocok.
	ErrNoPending = errors.New("no pending change with that hash")
	// ErrSameApprover identitas sudah menyetujui perubahan ini.
	ErrSameApprover = errors.New("already approved by this identity (four-eyes requires a different approver)")
)

// Doc satu dokumen. Prepare memvalidasi isi dan mengembalikan fungsi yang
// mengaktifkannya (dipanggil saat cukup persetujuan).
type Doc struct {
	Name, Path string
	Prepare    func(content []byte) (activate func(), err error)
}

// Status keadaan dokumen.
type Status struct {
	Document   string    `json:"document"`
	ActiveHash string    `json:"active_hash"`
	Pending    *Pending  `json:"pending,omitempty"`
	ApprovedAt time.Time `json:"approved_at"`
	ApprovedBy []string  `json:"approved_by"`
}

// Pending perubahan yang menunggu persetujuan.
type Pending struct {
	Hash       string    `json:"hash"`
	ProposedAt time.Time `json:"proposed_at"`
	Approvals  []string  `json:"approvals"`
	Content    string    `json:"content"`
}

// Event perubahan untuk audit (policy_change).
type Event struct {
	Document, By, Hash string
	Status             string // pending | approved | activated
}

// Gate kumpulan dokumen ber-four-eyes.
type Gate struct {
	dir     string // "" = four-eyes mati: perubahan langsung aktif
	onEvent func(Event)
	now     func() time.Time

	mu   sync.Mutex
	docs map[string]*state
}

type state struct {
	doc     Doc
	active  approved
	pending *Pending
}

// approved isi state dir <name>.approved.json.
type approved struct {
	Hash       string    `json:"hash"`
	Content    string    `json:"content"`
	ApprovedAt time.Time `json:"approved_at"`
	ApprovedBy []string  `json:"approved_by"`
}

// New membuat Gate. dir "" = tanpa four-eyes (dev). onEvent boleh nil.
func New(dir string, onEvent func(Event)) *Gate {
	if onEvent == nil {
		onEvent = func(Event) {}
	}
	return &Gate{dir: dir, onEvent: onEvent, now: time.Now, docs: map[string]*state{}}
}

// Enabled apakah four-eyes aktif.
func (g *Gate) Enabled() bool { return g.dir != "" }

func hash(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// Open mendaftarkan dan mengaktifkan d: isi yang terakhir disetujui (state
// dir) bila ada — isi file yang berbeda menjadi pending — selain itu isi file
// (bootstrap, dicatat sebagai disetujui).
func (g *Gate) Open(d Doc) error {
	file, err := os.ReadFile(filepath.Clean(d.Path))
	if err != nil {
		return fmt.Errorf("approval.Open %s: %w", d.Name, err)
	}
	st := &state{doc: d}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.docs[d.Name] = st
	if g.dir != "" {
		if b, err := os.ReadFile(g.statePath(d.Name)); err == nil {
			if err := json.Unmarshal(b, &st.active); err != nil {
				return fmt.Errorf("approval.Open %s: state: %w", d.Name, err)
			}
			if hash([]byte(st.active.Content)) != st.active.Hash {
				return fmt.Errorf("approval.Open %s: approved state corrupted (hash mismatch)", d.Name)
			}
			activate, err := d.Prepare([]byte(st.active.Content))
			if err != nil {
				return fmt.Errorf("approval.Open %s: approved content: %w", d.Name, err)
			}
			activate()
			if h := hash(file); h != st.active.Hash {
				if _, err := d.Prepare(file); err == nil { // file tidak valid: tetap tanpa pending
					st.pending = &Pending{Hash: h, ProposedAt: g.now(), Content: string(file)}
					g.onEvent(Event{Document: d.Name, Hash: h, Status: "pending"})
				}
			}
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("approval.Open %s: %w", d.Name, err)
		}
	}
	activate, err := d.Prepare(file)
	if err != nil {
		return fmt.Errorf("approval.Open %s: %w", d.Name, err)
	}
	activate()
	st.active = approved{Hash: hash(file), Content: string(file), ApprovedAt: g.now(), ApprovedBy: []string{Bootstrap}}
	if g.dir != "" {
		if err := g.save(d.Name, st.active); err != nil {
			return err
		}
		g.onEvent(Event{Document: d.Name, By: Bootstrap, Hash: st.active.Hash, Status: "activated"})
	}
	return nil
}

// Propose membaca ulang file dokumen. Four-eyes mati → langsung aktif.
// Selain itu isi berbeda dari yang aktif menjadi pending; by (boleh "" untuk
// SIGHUP) dihitung sebagai persetujuan pertama.
func (g *Gate) Propose(name, by string) (Status, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.docs[name]
	if !ok {
		return Status{}, fmt.Errorf("approval.Propose %s: %w", name, ErrUnknown)
	}
	file, err := os.ReadFile(filepath.Clean(st.doc.Path))
	if err != nil {
		return Status{}, fmt.Errorf("approval.Propose %s: %w", name, err)
	}
	activate, err := st.doc.Prepare(file)
	if err != nil {
		return Status{}, fmt.Errorf("approval.Propose %s: %w", name, err)
	}
	h := hash(file)
	switch {
	case g.dir == "":
		activate()
		st.active = approved{Hash: h, Content: string(file), ApprovedAt: g.now(), ApprovedBy: []string{by}}
		g.onEvent(Event{Document: name, By: by, Hash: h, Status: "activated"})
		return st.status(), nil
	case h == st.active.Hash:
		st.pending = nil
		return st.status(), nil
	case st.pending == nil || st.pending.Hash != h:
		st.pending = &Pending{Hash: h, ProposedAt: g.now(), Content: string(file)}
		g.onEvent(Event{Document: name, By: by, Hash: h, Status: "pending"})
	}
	if by == "" {
		return st.status(), nil
	}
	return g.approve(st, by, h)
}

// Approve menambah persetujuan by atas pending hash; aktif bila sudah
// Required persetujuan berbeda.
func (g *Gate) Approve(name, by, h string) (Status, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.docs[name]
	if !ok {
		return Status{}, fmt.Errorf("approval.Approve %s: %w", name, ErrUnknown)
	}
	return g.approve(st, by, h)
}

func (g *Gate) approve(st *state, by, h string) (Status, error) {
	name := st.doc.Name
	if by == "" {
		return Status{}, errors.New("approval.Approve: approver identity required")
	}
	if st.pending == nil || st.pending.Hash != h {
		return Status{}, fmt.Errorf("approval.Approve %s: %w", name, ErrNoPending)
	}
	if slices.Contains(st.pending.Approvals, by) {
		return Status{}, fmt.Errorf("approval.Approve %s: %w", name, ErrSameApprover)
	}
	st.pending.Approvals = append(st.pending.Approvals, by)
	g.onEvent(Event{Document: name, By: by, Hash: h, Status: "approved"})
	if len(st.pending.Approvals) < Required {
		return st.status(), nil
	}
	activate, err := st.doc.Prepare([]byte(st.pending.Content))
	if err != nil {
		return Status{}, fmt.Errorf("approval.Approve %s: %w", name, err)
	}
	next := approved{Hash: h, Content: st.pending.Content, ApprovedAt: g.now(), ApprovedBy: st.pending.Approvals}
	if err := g.save(name, next); err != nil { // simpan dulu: aktif tanpa state = hilang saat restart
		return Status{}, err
	}
	activate()
	st.active, st.pending = next, nil
	g.onEvent(Event{Document: name, By: by, Hash: h, Status: "activated"})
	return st.status(), nil
}

// Status keadaan semua dokumen, urut nama.
func (g *Gate) Status() []Status {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Status, 0, len(g.docs))
	for _, st := range g.docs {
		out = append(out, st.status())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Document < out[j].Document })
	return out
}

func (st *state) status() Status {
	s := Status{Document: st.doc.Name, ActiveHash: st.active.Hash, ApprovedAt: st.active.ApprovedAt, ApprovedBy: st.active.ApprovedBy}
	if st.pending != nil {
		p := *st.pending
		p.Approvals = slices.Clone(p.Approvals)
		s.Pending = &p
	}
	return s
}

func (g *Gate) statePath(name string) string { return filepath.Join(g.dir, name+".approved.json") }

// save menulis state secara atomik (tmp + rename), file 0600, dir 0700.
func (g *Gate) save(name string, a approved) error {
	if err := os.MkdirAll(g.dir, 0o700); err != nil {
		return fmt.Errorf("approval.save: %w", err)
	}
	b, _ := json.Marshal(a) // struct string/time: tidak pernah error
	tmp, err := os.CreateTemp(g.dir, name+".*.tmp")
	if err != nil {
		return fmt.Errorf("approval.save: %w", err)
	}
	_, err = tmp.Write(b)
	if err = errors.Join(err, tmp.Chmod(0o600), tmp.Sync(), tmp.Close()); err == nil {
		err = os.Rename(tmp.Name(), g.statePath(name))
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return fmt.Errorf("approval.save: %w", err)
	}
	return nil
}
