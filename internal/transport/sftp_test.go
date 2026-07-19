package transport

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// failWriteCloser records what was written and fails on Write or Close as
// configured, standing in for an sftp.File whose server rejects the upload.
type failWriteCloser struct {
	buf      strings.Builder
	writeErr error
	closeErr error
}

func (f *failWriteCloser) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.buf.Write(p)
}

func (f *failWriteCloser) Close() error { return f.closeErr }

func TestStreamToClose(t *testing.T) {
	errWrite := errors.New("write failed")
	errClose := errors.New("close failed: quota exceeded")

	tests := []struct {
		name     string
		writeErr error
		closeErr error
		want     error
	}{
		{name: "success", want: nil},
		{name: "close error surfaces when copy succeeds", closeErr: errClose, want: errClose},
		{name: "copy error takes precedence over close error",
			writeErr: errWrite, closeErr: errClose, want: errWrite},
		{name: "copy error alone", writeErr: errWrite, want: errWrite},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := &failWriteCloser{writeErr: tt.writeErr, closeErr: tt.closeErr}
			r := strings.NewReader("payload")
			err := streamToClose(w, r, int64(r.Len()), nil)
			if !errors.Is(err, tt.want) {
				t.Fatalf("streamToClose error = %v, want %v", err, tt.want)
			}
		})
	}
}

var _ io.WriteCloser = (*failWriteCloser)(nil)

func TestResolveSFTPPort(t *testing.T) {
	if got := resolvePort(0, defaultSFTPPort); got != defaultSFTPPort {
		t.Errorf("resolvePort(0, sftp default) = %d, want default %d", got, defaultSFTPPort)
	}
	if got := resolvePort(2222, defaultSFTPPort); got != 2222 {
		t.Errorf("resolvePort(2222, sftp default) = %d, want 2222", got)
	}
}

func writeTestKey(t *testing.T, passphrase string) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	var block *pem.Block
	if passphrase != "" {
		block, err = ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte(passphrase))
	} else {
		block, err = ssh.MarshalPrivateKey(priv, "")
	}
	if err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadPrivateKeyUnencrypted(t *testing.T) {
	path := writeTestKey(t, "")
	signer, err := loadPrivateKey(path, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if signer == nil {
		t.Fatal("expected a non-nil signer")
	}
}

func TestLoadPrivateKeyWithPassphrase(t *testing.T) {
	path := writeTestKey(t, "s3cret")

	if _, err := loadPrivateKey(path, ""); err == nil {
		t.Error("expected an error when no passphrase is given for a protected key")
	}

	signer, err := loadPrivateKey(path, "s3cret")
	if err != nil {
		t.Fatalf("unexpected error with correct passphrase: %v", err)
	}
	if signer == nil {
		t.Fatal("expected a non-nil signer")
	}
}

func TestSFTPAuthMethodsPrecedence(t *testing.T) {
	path := writeTestKey(t, "")

	t.Run("private key preferred when both set", func(t *testing.T) {
		methods, err := sftpAuthMethods(SFTPDialOptions{
			PrivateKeyPath: path, Password: "unused-as-login-password",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(methods) != 1 {
			t.Fatalf("expected 1 auth method, got %d", len(methods))
		}
	})

	t.Run("password used when no key", func(t *testing.T) {
		methods, err := sftpAuthMethods(SFTPDialOptions{Password: "hunter2"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(methods) != 1 {
			t.Fatalf("expected 1 auth method, got %d", len(methods))
		}
	})

	t.Run("neither set is an error", func(t *testing.T) {
		if _, err := sftpAuthMethods(SFTPDialOptions{}); err == nil {
			t.Error("expected an error when neither password nor private key is set")
		}
	})
}

// setHome points os.UserHomeDir at dir on the current platform: it reads
// HOME on unix but USERPROFILE on Windows, so set both.
func setHome(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
}

func TestKnownHostsCallback(t *testing.T) {
	t.Run("missing known_hosts is an error", func(t *testing.T) {
		setHome(t, t.TempDir())
		if _, err := knownHostsCallback(); err == nil {
			t.Error("expected an error when known_hosts doesn't exist")
		}
	})

	t.Run("present known_hosts succeeds", func(t *testing.T) {
		home := t.TempDir()
		if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(home, ".ssh", "known_hosts"), nil, 0o600); err != nil {
			t.Fatal(err)
		}
		setHome(t, home)

		callback, err := knownHostsCallback()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if callback == nil {
			t.Fatal("expected a non-nil callback")
		}
	})
}
