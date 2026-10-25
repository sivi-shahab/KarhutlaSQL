package pgwire

import (
	"io"
	"sync"
)

// maxPumped batas byte dari klien yang ditampung sebelum dibaca protokol
// (backpressure: pump berhenti membaca socket sampai buffer berkurang).
const maxPumped = 1 << 20

// pump membaca socket klien terus-menerus ke buffer terbatas, sehingga
// putusnya koneksi terdeteksi saat statement sedang berjalan (bukan baru
// saat pesan berikutnya dibaca) dan statement itu dibatalkan — seperti
// backend PostgreSQL. Protokol membaca lewat Read.
type pump struct {
	mu   sync.Mutex
	cond *sync.Cond
	buf  []byte
	err  error
}

func newPump() *pump {
	p := &pump{}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// run menyalin dari r sampai error/EOF, lalu memanggil onClose.
func (p *pump) run(r io.Reader, onClose func()) {
	b := make([]byte, 32<<10)
	for {
		n, err := r.Read(b)
		p.mu.Lock()
		p.buf = append(p.buf, b[:n]...)
		if err != nil {
			p.err = err
		}
		p.cond.Broadcast()
		for len(p.buf) >= maxPumped && p.err == nil {
			p.cond.Wait()
		}
		p.mu.Unlock()
		if err != nil {
			onClose()
			return
		}
	}
}

// Read io.Reader untuk pgproto3.Backend.
func (p *pump) Read(dst []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for len(p.buf) == 0 && p.err == nil {
		p.cond.Wait()
	}
	if len(p.buf) == 0 {
		return 0, p.err
	}
	n := copy(dst, p.buf)
	p.buf = p.buf[n:]
	p.cond.Broadcast()
	return n, nil
}
