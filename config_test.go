package uploadfun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeConfig(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadConfigValid(t *testing.T) {
	t.Setenv("SHUTTERSTOCK_FTP_PASSWORD", "s3cret")
	path := writeConfig(t, `
endpoints:
  - name: shutterstock
    protocol: ftps
    host: ftp.shutterstock.com
    username: myuser
    password: ${SHUTTERSTOCK_FTP_PASSWORD}

  - name: dreamstime
    protocol: sftp
    host: sftp.dreamstime.com
    username: myuser
    private_key: ~/.ssh/id_ed25519

  - name: legacy-agency
    protocol: ftp
    host: ftp.legacy-agency.example
    username: myuser
    password: hunter2

attempts: 3
retry_delay: 2s
connect_timeout: 30s
stall_timeout: 5m
`)

	endpoints, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(endpoints) != 3 {
		t.Fatalf("expected 3 endpoints, got %d", len(endpoints))
	}

	shutterstock := endpoints[0]
	if shutterstock.Password != "s3cret" {
		t.Errorf("expected interpolated password, got %q", shutterstock.Password)
	}
	if shutterstock.Overwrite != OverwriteDeleteFirst {
		t.Errorf("expected default overwrite delete-first, got %q", shutterstock.Overwrite)
	}
	if shutterstock.Attempts != 3 || shutterstock.RetryDelay != 2*time.Second {
		t.Errorf("expected global defaults applied, got attempts=%d retry_delay=%v", shutterstock.Attempts, shutterstock.RetryDelay)
	}

	dreamstime := endpoints[1]
	home, _ := os.UserHomeDir()
	if dreamstime.PrivateKey != filepath.Join(home, ".ssh/id_ed25519") {
		t.Errorf("expected expanded private key path, got %q", dreamstime.PrivateKey)
	}
}

func TestLoadConfigPerEndpointOverride(t *testing.T) {
	path := writeConfig(t, `
endpoints:
  - name: a
    protocol: ftp
    host: ftp.example.com
    username: u
    password: p
    attempts: 5
    retry_delay: 500ms
attempts: 3
retry_delay: 2s
`)
	endpoints, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if endpoints[0].Attempts != 5 {
		t.Errorf("expected per-endpoint override attempts=5, got %d", endpoints[0].Attempts)
	}
	if endpoints[0].RetryDelay != 500*time.Millisecond {
		t.Errorf("expected per-endpoint override retry_delay=500ms, got %v", endpoints[0].RetryDelay)
	}
}

func TestLoadConfigCollectsAllErrors(t *testing.T) {
	path := writeConfig(t, `
endpoints:
  - name: dup
    protocol: bogus
    host: ftp.example.com
  - name: dup
    protocol: ftp
    host: ftp.example.com
    username: u
  - name: keyed-ftp
    protocol: ftp
    host: ftp.example.com
    username: u
    password: p
    private_key: ~/.ssh/id_ed25519
`)
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	for _, want := range []string{
		"unknown protocol",
		"username is required",
		"password is required",
		"duplicate endpoint name",
		"private_key is not supported",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("expected error message to contain %q, got:\n%s", want, msg)
		}
	}
}

func TestLoadConfigMissingEnvVar(t *testing.T) {
	_ = os.Unsetenv("DOES_NOT_EXIST_XYZ")
	path := writeConfig(t, `
endpoints:
  - name: a
    protocol: ftp
    host: ftp.example.com
    username: u
    password: ${DOES_NOT_EXIST_XYZ}
`)
	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "undefined environment variable") {
		t.Fatalf("expected undefined environment variable error, got %v", err)
	}
}

func TestLoadConfigSFTPRequiresPasswordOrKey(t *testing.T) {
	path := writeConfig(t, `
endpoints:
  - name: a
    protocol: sftp
    host: sftp.example.com
    username: u
`)
	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "sftp requires password or private_key") {
		t.Fatalf("expected sftp requires password or private_key error, got %v", err)
	}
}

func TestLoadConfigNoEndpoints(t *testing.T) {
	path := writeConfig(t, `attempts: 3`)
	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "at least one endpoint is required") {
		t.Fatalf("expected at least one endpoint error, got %v", err)
	}
}

func TestLoadConfigFileNotFound(t *testing.T) {
	_, err := LoadConfig("/nonexistent/config.yaml")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
}
