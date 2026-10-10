// Package netutil has small helpers for byte streams.
package netutil

import (
	"io"
	"sync"
)

// Pipe copies data in both directions and closes both ends once either side
// finishes. It returns when both copies are done.
func Pipe(a, b io.ReadWriteCloser) {
	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			a.Close()
			b.Close()
		})
	}
	done := make(chan struct{}, 2)
	go func() { io.Copy(a, b); closeAll(); done <- struct{}{} }()
	go func() { io.Copy(b, a); closeAll(); done <- struct{}{} }()
	<-done
	<-done
}
