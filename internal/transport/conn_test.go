package transport

import (
	"io"
	"net"
	"testing"
	"time"
)

// recordingConn is a net.Conn stand-in that records the deadlines set on it
// and serves canned Read/Write results, so guardedConn's behavior can be
// checked without a real network connection.
type recordingConn struct {
	net.Conn
	readDeadlines  []time.Time
	writeDeadlines []time.Time
}

func (c *recordingConn) SetReadDeadline(t time.Time) error {
	c.readDeadlines = append(c.readDeadlines, t)
	return nil
}

func (c *recordingConn) SetWriteDeadline(t time.Time) error {
	c.writeDeadlines = append(c.writeDeadlines, t)
	return nil
}

func (c *recordingConn) Read([]byte) (int, error)    { return 0, io.EOF }
func (c *recordingConn) Write(b []byte) (int, error) { return len(b), nil }

func TestGuardedConnPassiveUntilEstablished(t *testing.T) {
	rec := &recordingConn{}
	guard := &stallGuard{stallTimeout: time.Minute}
	gc := guard.wrap(rec)

	_, _ = gc.Read(make([]byte, 4))
	_, _ = gc.Write([]byte("x"))
	if len(rec.readDeadlines) != 0 || len(rec.writeDeadlines) != 0 {
		t.Fatalf("expected no deadlines before established, got read=%v write=%v",
			rec.readDeadlines, rec.writeDeadlines)
	}

	guard.markEstablished()
	before := time.Now()
	_, _ = gc.Read(make([]byte, 4))
	_, _ = gc.Write([]byte("x"))
	if len(rec.readDeadlines) != 1 || len(rec.writeDeadlines) != 1 {
		t.Fatalf("expected one read and one write deadline after established, got read=%v write=%v",
			rec.readDeadlines, rec.writeDeadlines)
	}
	if rec.readDeadlines[0].Before(before.Add(time.Minute)) {
		t.Errorf("expected read deadline at/after now+stallTimeout, got %v", rec.readDeadlines[0])
	}
}

func TestGuardedConnZeroTimeoutDisabled(t *testing.T) {
	rec := &recordingConn{}
	guard := &stallGuard{stallTimeout: 0}
	guard.markEstablished()
	gc := guard.wrap(rec)

	_, _ = gc.Read(make([]byte, 4))
	_, _ = gc.Write([]byte("x"))
	if len(rec.readDeadlines) != 0 || len(rec.writeDeadlines) != 0 {
		t.Errorf("expected no deadlines when stallTimeout is 0, got read=%v write=%v",
			rec.readDeadlines, rec.writeDeadlines)
	}
}
