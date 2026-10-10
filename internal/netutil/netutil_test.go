package netutil

import (
	"io"
	"net"
	"testing"
	"time"
)

func TestPipeCopiesBothWaysAndClosesBoth(t *testing.T) {
	a1, a2 := net.Pipe()
	b1, b2 := net.Pipe()
	done := make(chan struct{})
	go func() { Pipe(a2, b1); close(done) }()

	go a1.Write([]byte("to b"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(b2, buf); err != nil || string(buf) != "to b" {
		t.Fatalf("a->b: %q %v", buf, err)
	}
	go b2.Write([]byte("to a"))
	if _, err := io.ReadFull(a1, buf); err != nil || string(buf) != "to a" {
		t.Fatalf("b->a: %q %v", buf, err)
	}

	a1.Close() // one side finishing closes the other
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Pipe did not return after one side closed")
	}
	if _, err := b2.Read(buf); err == nil {
		t.Fatal("other side still open")
	}
}
