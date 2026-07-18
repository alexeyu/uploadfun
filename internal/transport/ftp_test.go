package transport

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestIsNotExist(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"550 file unavailable", &textproto.Error{Code: 550, Msg: "No such file or directory"}, true},
		{"other ftp error", &textproto.Error{Code: 530, Msg: "Not logged in"}, false},
		{"non-ftp error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isNotExist(tt.err); got != tt.want {
				t.Errorf("isNotExist(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
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

// fakeFTPServer is a minimal single-connection FTP server: just enough of
// the login and PASV/STOR sequence for a real client library to drive a
// data-connection upload against it.
type fakeFTPServer struct {
	listener net.Listener
	received []byte
}

func newFakeFTPServer(t *testing.T) *fakeFTPServer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeFTPServer{listener: l}
	t.Cleanup(func() { _ = l.Close() })
	go s.serve()
	return s
}

func (s *fakeFTPServer) addr() string { return s.listener.Addr().String() }

func (s *fakeFTPServer) serve() {
	conn, err := s.listener.Accept()
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()

	tp := textproto.NewConn(conn)
	_ = tp.PrintfLine("220 fake ftp ready")

	var dataDone chan struct{}
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}

		switch strings.SplitN(line, " ", 2)[0] {
		case "USER":
			_ = tp.PrintfLine("331 send password")
		case "PASS":
			_ = tp.PrintfLine("230 logged in")
		case "FEAT", "EPSV":
			// Reported as unsupported so the client falls back to PASV,
			// keeping this fake server to one code path.
			_ = tp.PrintfLine("502 not implemented")
		case "TYPE":
			_ = tp.PrintfLine("200 type set")
		case "PASV":
			dl, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				_ = tp.PrintfLine("451 %s", err)
				continue
			}
			_, portStr, _ := net.SplitHostPort(dl.Addr().String())
			port, _ := strconv.Atoi(portStr)

			done := make(chan struct{})
			dataDone = done
			go func() {
				defer close(done)
				dc, err := dl.Accept()
				_ = dl.Close()
				if err != nil {
					return
				}
				defer func() { _ = dc.Close() }()
				s.received, _ = io.ReadAll(dc)
			}()

			_ = tp.PrintfLine("227 Entering Passive Mode (127,0,0,1,%d,%d)", port/256, port%256)
		case "STOR":
			_ = tp.PrintfLine("150 opening data connection")
			if dataDone != nil {
				<-dataDone
			}
			_ = tp.PrintfLine("226 transfer complete")
		case "QUIT":
			_ = tp.PrintfLine("221 bye")
			return
		default:
			_ = tp.PrintfLine("502 command not implemented")
		}
	}
}

// TestDialFTPDataConnSurvivesConnectCtxCancellation guards against a
// regression where ftpDialer kept using the connect-scoped ctx for data
// connections opened after login, which dispatch.go cancels on return.
func TestDialFTPDataConnSurvivesConnectCtxCancellation(t *testing.T) {
	server := newFakeFTPServer(t)
	host, portStr, err := net.SplitHostPort(server.addr())
	if err != nil {
		t.Fatalf("split addr: %v", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}

	connectCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	client, err := DialFTP(connectCtx, FTPDialOptions{
		Host:           host,
		Port:           port,
		Username:       "user",
		Password:       "pass",
		ConnectTimeout: 2 * time.Second,
	})
	cancel() // mirrors dispatch.go's connect(): canceled right after Connect returns
	if err != nil {
		t.Fatalf("DialFTP: %v", err)
	}
	defer func() { _ = client.Close() }()

	if err := client.Upload("file.txt", strings.NewReader("hello"), 5, nil); err != nil {
		t.Fatalf("Upload after connect ctx canceled: %v", err)
	}
	if got := string(server.received); got != "hello" {
		t.Errorf("server received %q, want %q", got, "hello")
	}
}
