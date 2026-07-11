package transport

import (
	"fmt"
	"net"
	"os"
	"sync/atomic"
	"time"
)

// verifyBySize confirms remoteName is the same size as localPath, the
// size-only verification both FTP/FTPS and SFTP fall back to (neither
// exposes a portable remote-hash command; see ARCHITECTURE.md
// "Verification"). remoteSize fetches the remote file's size for whichever
// transport is calling. On success method is always "size".
func verifyBySize(localPath, remoteName string, remoteSize func(string) (int64, error)) (
	method string, err error,
) {
	info, err := os.Stat(localPath)
	if err != nil {
		return "", fmt.Errorf("stat local file: %w", err)
	}
	rSize, err := remoteSize(remoteName)
	if err != nil {
		return "", fmt.Errorf("remote size: %w", err)
	}
	if rSize != info.Size() {
		return "", fmt.Errorf("size mismatch: local %d bytes, remote %d bytes", info.Size(), rSize)
	}
	return "size", nil
}

// resolvePort returns port, or def when port is 0 (unset), so each
// transport can supply its own protocol default.
func resolvePort(port, def int) int {
	if port == 0 {
		return def
	}
	return port
}

// dirEntryNames collects entry names for a --dry-run listing, dropping the
// "." and ".." pseudo-entries some servers include (MLSD-answering servers
// like pure-ftpd among them) — a caller displaying "N entries" shouldn't
// count them. name extracts the filename from each transport's own entry
// type.
func dirEntryNames[T any](entries []T, name func(T) string) []string {
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if n := name(e); n != "." && n != ".." {
			names = append(names, n)
		}
	}
	return names
}

// armConnectDeadline sets timeout as a deadline on conn covering the whole
// connect+login/handshake phase, so a server that accepts TCP then stalls
// mid-negotiation fails instead of blocking forever. A non-positive timeout
// leaves the conn without a deadline. Cleared with clearDeadline once the
// session is established, handing enforcement over to the stall guard.
func armConnectDeadline(conn net.Conn, timeout time.Duration) {
	if timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}
}

// clearDeadline removes any deadline armConnectDeadline set, called once the
// session is up so transfers run under the idle stall timeout instead.
func clearDeadline(conn net.Conn) {
	_ = conn.SetDeadline(time.Time{})
}

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
