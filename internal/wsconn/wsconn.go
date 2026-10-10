// Package wsconn holds helpers shared by the relay binaries.
package wsconn

import (
	"context"
	"io"
	"net"
	"sync"

	"github.com/coder/websocket"
)

// ReadLimit is the max size of a single websocket message.
const ReadLimit = 1 << 20

// NetConn wraps a websocket as a net.Conn carrying binary messages.
func NetConn(ctx context.Context, c *websocket.Conn) net.Conn {
	c.SetReadLimit(ReadLimit)
	return websocket.NetConn(ctx, c, websocket.MessageBinary)
}

// Pipe copies data in both directions and closes both ends once either side finishes.
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
