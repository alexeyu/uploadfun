package transport

import (
	"crypto/tls"
	"testing"
)

func TestResolvePort(t *testing.T) {
	if got := resolvePort(0); got != defaultFTPPort {
		t.Errorf("resolvePort(0) = %d, want default %d", got, defaultFTPPort)
	}
	if got := resolvePort(2121); got != 2121 {
		t.Errorf("resolvePort(2121) = %d, want 2121", got)
	}
}

func TestResolveTLSConfig(t *testing.T) {
	t.Run("nil config defaults to ServerName", func(t *testing.T) {
		cfg := resolveTLSConfig("ftp.example.com", nil)
		if cfg == nil || cfg.ServerName != "ftp.example.com" {
			t.Errorf("expected default TLS config with ServerName set, got %+v", cfg)
		}
	})

	t.Run("explicit config passed through unchanged", func(t *testing.T) {
		explicit := &tls.Config{ServerName: "override.example.com", MinVersion: tls.VersionTLS12}
		cfg := resolveTLSConfig("ftp.example.com", explicit)
		if cfg != explicit {
			t.Errorf("expected explicit TLS config to be returned unchanged, got %+v", cfg)
		}
	})
}
