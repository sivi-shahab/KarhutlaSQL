package pgwire

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"
)

func TestPump(t *testing.T) {
	client, server := net.Pipe()
	p := newPump()
	closed := make(chan struct{})
	go p.run(server, func() { close(closed) })
	go func() {
		_, _ = client.Write([]byte("hello "))
		_, _ = client.Write(bytes.Repeat([]byte("x"), 100))
	}()
	got := make([]byte, 106)
	if _, err := io.ReadFull(p, got); err != nil || string(got[:6]) != "hello " {
		t.Fatalf("read %q %v", got[:6], err)
	}
	select {
	case <-closed:
		t.Fatal("onClose before disconnect")
	default:
	}
	_ = client.Close() // klien putus → onClose tanpa ada yang membaca
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("disconnect not detected")
	}
	if _, err := p.Read(make([]byte, 1)); err == nil {
		t.Fatal("read after disconnect: want error")
	}
}
