package transport

import (
	"fmt"
	"io"
	"net"
	"os"
	"sync/atomic"
	"time"
)

// verifyBySize confirms remoteName matches localPath's size - the size-only
// check both FTP/FTPS and SFTP fall back to, lacking a portable remote-hash
// command. remoteSize fetches the remote size for the calling transport.
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

// resolvePort returns port, or def when port is 0 (unset).
func resolvePort(port, def int) int {
	if port == 0 {
		return def
	}
	return port
}

// armConnectDeadline gives conn a deadline covering the whole
// connect+login/handshake phase, so a stalling server fails instead of
// blocking forever. clearDeadline lifts it once the session is up.
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

// stallGuard is shared by every connection opened for one endpoint
// dial. It flips once from connecting to established, switching the
// guardedConns it wraps from passive to idle-timeout enforcement.
type stallGuard struct {
	stallTimeout time.Duration
	established  atomic.Bool
}

// wrap returns c wrapped so its reads and writes enforce the stall timeout
// once the guard is established.
func (g *stallGuard) wrap(c net.Conn) *guardedConn {
	return &guardedConn{Conn: c, guard: g}
}

func (g *stallGuard) markEstablished() { g.established.Store(true) }

// guardedConn enforces an idle (stall) timeout: once established, every
// Read/Write pushes the deadline to now+stallTimeout, so a stalled
// transfer fails instead of blocking forever.
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

// progressReader wraps r to report cumulative bytes read via onProgress,
// shared by FTP's Stor and SFTP's io.Copy upload paths.
type progressReader struct {
	r          io.Reader
	sent       int64
	total      int64
	onProgress func(sent, total int64)
}

func (p *progressReader) Read(buf []byte) (int, error) {
	n, err := p.r.Read(buf)
	if n > 0 {
		p.sent += int64(n)
		if p.onProgress != nil {
			p.onProgress(p.sent, p.total)
		}
	}
	return n, err
}

// Size reports the total bytes the wrapped reader will yield. pkg/sftp's
// File.ReadFrom type-switches for a Size/Len/Stat method to size its
// concurrent-write pipeline; without this, it can't tell progressReader's
// length apart from "unknown" and silently falls back to one write in
// flight at a time.
func (p *progressReader) Size() int64 {
	return p.total
}
