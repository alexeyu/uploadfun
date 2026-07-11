package transport

import (
	"bytes"
	"errors"
	"io"
	"net/textproto"
	"testing"
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
		t.Errorf("expected final progress call to report full size, got sent=%d total=%d", last[0], last[1])
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
