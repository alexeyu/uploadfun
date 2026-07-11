package transport

import (
	"net"
	"sync/atomic"
	"time"
)

// stallGuard is shared by every connection opened for one endpoint dial
// (the control connection and, for FTP, each data connection). It flips
// once from connecting to established, which switches the guardedConns it
// wraps from passive to idle-timeout enforcement.
type stallGuard struct {
	stallTimeout time.Duration
	established  atomic.Bool
}

// wrap returns c wrapped so its reads and writes enforce the stall timeout
// once the guard is established.
func (g *stallGuard) wrap(c net.Conn) *guardedConn {
	return &guardedConn{Conn: c, guard: g}
}

// markEstablished switches wrapped connections from connect-phase (passive,
// so the dialer's connect deadline governs) to transfer-phase idle-timeout
// enforcement. Called once the session is fully up.
func (g *stallGuard) markEstablished() { g.established.Store(true) }

// guardedConn enforces an idle (stall) timeout: once established, every
// Read/Write first pushes the deadline to now+stallTimeout, so a transfer
// that makes no forward progress for that long fails instead of blocking
// forever. Before the guard is established it leaves the deadline alone, so
// whatever connect deadline the dialer set stays in force.
type guardedConn struct {
	net.Conn
	guard *stallGuard
}

func (c *guardedConn) Read(b []byte) (int, error) {
	c.arm(c.SetReadDeadline)
	return c.Conn.Read(b)
}

func (c *guardedConn) Write(b []byte) (int, error) {
	c.arm(c.SetWriteDeadline)
	return c.Conn.Write(b)
}

func (c *guardedConn) arm(setDeadline func(time.Time) error) {
	if !c.guard.established.Load() || c.guard.stallTimeout <= 0 {
		return
	}
	_ = setDeadline(time.Now().Add(c.guard.stallTimeout))
}
