// Package wsconn adapts WebSockets to net.Conn.
package wsconn

import (
	"context"
	"net"

	"github.com/coder/websocket"
)

// ReadLimit is the max size of a single websocket message.
const ReadLimit = 1 << 20

// NetConn wraps a websocket as a net.Conn carrying binary messages.
func NetConn(ctx context.Context, c *websocket.Conn) net.Conn {
	c.SetReadLimit(ReadLimit)
	return websocket.NetConn(ctx, c, websocket.MessageBinary)
}
