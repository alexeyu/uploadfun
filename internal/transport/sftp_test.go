package transport

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestResolveSFTPPort(t *testing.T) {
	if got := resolveSFTPPort(0); got != defaultSFTPPort {
		t.Errorf("resolveSFTPPort(0) = %d, want default %d", got, defaultSFTPPort)
	}
	if got := resolveSFTPPort(2222); got != 2222 {
		t.Errorf("resolveSFTPPort(2222) = %d, want 2222", got)
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

func TestKnownHostsCallback(t *testing.T) {
	t.Run("missing known_hosts is an error", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
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
		t.Setenv("HOME", home)

		callback, err := knownHostsCallback()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if callback == nil {
			t.Fatal("expected a non-nil callback")
		}
	})
}
