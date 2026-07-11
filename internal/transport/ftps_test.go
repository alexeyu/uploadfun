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
	t.Run("nil config defaults ServerName and session cache", func(t *testing.T) {
		cfg := resolveTLSConfig("ftp.example.com", nil)
		if cfg == nil || cfg.ServerName != "ftp.example.com" {
			t.Errorf("expected default TLS config with ServerName set, got %+v", cfg)
		}
		if cfg.ClientSessionCache == nil {
			t.Error("expected a default ClientSessionCache so data connections can resume the control connection's TLS session")
		}
	})

	t.Run("explicit ServerName preserved, session cache still filled in", func(t *testing.T) {
		explicit := &tls.Config{ServerName: "override.example.com", MinVersion: tls.VersionTLS12}
		cfg := resolveTLSConfig("ftp.example.com", explicit)
		if cfg == explicit {
			t.Error("expected the original config to be cloned, not mutated in place")
		}
		if cfg.ServerName != "override.example.com" {
			t.Errorf("expected explicit ServerName to be preserved, got %q", cfg.ServerName)
		}
		if cfg.MinVersion != tls.VersionTLS12 {
			t.Errorf("expected other explicit fields to be preserved, got MinVersion=%v", cfg.MinVersion)
		}
		if cfg.ClientSessionCache == nil {
			t.Error("expected ClientSessionCache to be filled in even for an explicit config")
		}
		if explicit.ClientSessionCache != nil {
			t.Error("expected the caller's original config to be left unmodified")
		}
	})

	t.Run("explicit session cache preserved", func(t *testing.T) {
		cache := tls.NewLRUClientSessionCache(1)
		explicit := &tls.Config{ClientSessionCache: cache}
		cfg := resolveTLSConfig("ftp.example.com", explicit)
		if cfg.ClientSessionCache != cache {
			t.Error("expected an explicit ClientSessionCache to be preserved, not replaced")
		}
	})
}
