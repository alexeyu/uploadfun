package transport

import (
	"bytes"
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

func TestProgressReader(t *testing.T) {
	data := []byte("hello world, this is some test data")
	var calls [][2]int64
	pr := &progressReader{
		r:     bytes.NewReader(data),
		total: int64(len(data)),
		onProgress: func(sent, total int64) {
			calls = append(calls, [2]int64{sent, total})
		},
	}

	got, err := io.ReadAll(pr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("expected read data to match source, got %q", got)
	}
	if len(calls) == 0 {
		t.Fatal("expected at least one progress callback")
	}
	last := calls[len(calls)-1]
	if last[0] != int64(len(data)) || last[1] != int64(len(data)) {
		t.Errorf("expected final progress call to report full size, got sent=%d total=%d",
			last[0], last[1])
	}
	for i := 1; i < len(calls); i++ {
		if calls[i][0] <= calls[i-1][0] {
			t.Errorf("expected cumulative sent bytes to increase monotonically, got %v", calls)
			break
		}
	}
}

func TestProgressReaderNilCallback(t *testing.T) {
	pr := &progressReader{r: bytes.NewReader([]byte("data")), total: 4}
	if _, err := io.ReadAll(pr); err != nil {
		t.Fatalf("unexpected error with nil onProgress: %v", err)
	}
}

func TestProgressReaderSize(t *testing.T) {
	pr := &progressReader{total: 12345}
	if got := pr.Size(); got != 12345 {
		t.Errorf("Size() = %d, want 12345", got)
	}
}
