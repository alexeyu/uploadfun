//go:build integration

// Real-server integration tests, gated behind the "integration" build
// tag - they need Docker and exercise actual FTP/FTPS/SFTP servers, so
// they never run as part of a plain `go test`. Run with:
//
//	go test -tags integration ./internal/transport/...
//
// TestMain starts a stilliard/pure-ftpd container (serving both plain
// FTP and, via a pre-generated test-only cert in testdata/, explicit
// AUTH TLS) and an atmoz/sftp container, on fixed local ports. If Docker
// isn't reachable, the whole run is skipped with a message rather than
// failing - matching "never block a contributor without Docker".
package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

const (
	ftpContainerName  = "uploadfun-it-pureftpd"
	sftpContainerName = "uploadfun-it-sftp"

	ftpControlPort = 12121
	// stilliard/pure-ftpd's default passive port range when none is set
	// via ADDED_FLAGS; must match what's actually mapped below.
	ftpDataPorts = "30000-30009"
	sftpPort     = 12122

	testUser     = "testuser"
	testPassword = "testpass"
)

var itHome string

func TestMain(m *testing.M) {
	if err := exec.Command("docker", "info").Run(); err != nil {
		fmt.Println("skipping integration tests: docker not available:", err)
		os.Exit(0)
	}

	var err error
	itHome, err = os.MkdirTemp("", "uploadfun-integration-home")
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	// knownHostsCallback reads $HOME/.ssh/known_hosts.
	if err := os.Setenv("HOME", itHome); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	stopFTP := startPureFTPD()
	stopSFTP := startAtmozSFTP()

	// Deferred cleanup would never run here since os.Exit skips defers,
	// so clean up explicitly before exiting rather than deferring it.
	code := m.Run()
	stopSFTP()
	stopFTP()
	_ = os.RemoveAll(itHome)

	os.Exit(code)
}

func mustRunDocker(args ...string) {
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		fmt.Printf("docker %v failed: %v\n%s\n", args, err, out)
		os.Exit(1)
	}
}

func waitForPort(addr string, timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	fmt.Printf("timed out waiting for %s to accept connections\n", addr)
	os.Exit(1)
}

func startPureFTPD() func() {
	cleanup := exec.Command("docker", "rm", "-f", ftpContainerName)
	_ = cleanup.Run() //nolint:errcheck // best-effort cleanup of a leftover run

	certPath, err := filepath.Abs("testdata/pure-ftpd.pem")
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	mustRunDocker("run", "-d", "--name", ftpContainerName,
		"-p", fmt.Sprintf("%d:21", ftpControlPort),
		"-p", ftpDataPorts+":"+ftpDataPorts,
		"-v", certPath+":/etc/ssl/private/pure-ftpd.pem:ro",
		"-e", "PUBLICHOST=127.0.0.1",
		"-e", "FTP_USER_NAME="+testUser,
		"-e", "FTP_USER_PASS="+testPassword,
		"-e", "FTP_USER_HOME=/home/ftpusers/"+testUser,
		"-e", "ADDED_FLAGS=--tls=1",
		"stilliard/pure-ftpd:hardened",
	)
	waitForPort(fmt.Sprintf("127.0.0.1:%d", ftpControlPort), 30*time.Second)

	return func() { _ = exec.Command("docker", "rm", "-f", ftpContainerName).Run() }
}

func startAtmozSFTP() func() {
	cleanup := exec.Command("docker", "rm", "-f", sftpContainerName)
	_ = cleanup.Run() //nolint:errcheck // best-effort cleanup of a leftover run

	mustRunDocker("run", "-d", "--name", sftpContainerName,
		"-p", fmt.Sprintf("%d:22", sftpPort),
		"atmoz/sftp:latest",
		fmt.Sprintf("%s:%s:1001:1001:upload", testUser, testPassword),
	)
	waitForPort(fmt.Sprintf("127.0.0.1:%d", sftpPort), 30*time.Second)

	// The container's SSH host key is regenerated every run, so populate
	// known_hosts for it before any test dials in - DialSFTP refuses to
	// connect to an unrecognized host.
	if err := os.MkdirAll(filepath.Join(itHome, ".ssh"), 0o700); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	var scanOut []byte
	var scanErr error
	for attempt := 0; attempt < 5; attempt++ {
		scanOut, scanErr = exec.Command(
			"ssh-keyscan", "-p", fmt.Sprintf("%d", sftpPort), "-t", "ed25519,rsa", "127.0.0.1",
		).Output()
		if scanErr == nil && len(scanOut) > 0 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if scanErr != nil || len(scanOut) == 0 {
		fmt.Println("ssh-keyscan failed:", scanErr)
		os.Exit(1)
	}
	if err := os.WriteFile(filepath.Join(itHome, ".ssh", "known_hosts"), scanOut, 0o600); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	return func() { _ = exec.Command("docker", "rm", "-f", sftpContainerName).Run() }
}

// verifyingUploader is the subset of FTPClient/SFTPClient that
// exerciseUploadDeleteVerify drives; both satisfy it structurally.
type verifyingUploader interface {
	Upload(remoteName string, r io.Reader, size int64, progress func(sent, total int64)) error
	Verify(localPath, remoteName string) (string, error)
	Delete(remoteName string) error
	List() ([]string, error)
}

func exerciseUploadDeleteVerify(t *testing.T, client verifyingUploader, remoteName string) {
	t.Helper()

	content := []byte("integration test payload\n")
	localPath := filepath.Join(t.TempDir(), "payload.txt")
	if err := os.WriteFile(localPath, content, 0o600); err != nil {
		t.Fatal(err)
	}

	var lastSent int64
	uploadErr := client.Upload(remoteName, bytes.NewReader(content), int64(len(content)),
		func(sent, total int64) {
			lastSent = sent
		})
	if uploadErr != nil {
		t.Fatalf("Upload: %v", uploadErr)
	}
	if lastSent != int64(len(content)) {
		t.Errorf("expected final progress callback to report %d bytes sent, got %d",
			len(content), lastSent)
	}

	method, err := client.Verify(localPath, remoteName)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if method != "size" {
		t.Errorf("expected verify method %q, got %q", "size", method)
	}

	names, err := client.List()
	if err != nil {
		t.Errorf("List: %v", err)
	}
	for _, name := range names {
		if name == "." || name == ".." {
			t.Errorf("expected List to exclude pseudo-entries, got %v", names)
			break
		}
	}

	if err := client.Delete(remoteName); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Delete must be idempotent: it's called unconditionally before every
	// upload under the default delete-first overwrite mode, including
	// the very first upload of a file that was never there.
	if err := client.Delete(remoteName); err != nil {
		t.Fatalf("Delete of an already-deleted file should be a no-op, got: %v", err)
	}

	if _, err := client.Verify(localPath, remoteName); err == nil {
		t.Error("expected Verify to fail against a deleted remote file")
	}
}

func TestIntegrationFTP(t *testing.T) {
	client, err := DialFTP(context.Background(), FTPDialOptions{
		Host:           "127.0.0.1",
		Port:           ftpControlPort,
		Username:       testUser,
		Password:       testPassword,
		ConnectTimeout: 10 * time.Second,
		StallTimeout:   30 * time.Second,
	})
	if err != nil {
		t.Fatalf("DialFTP: %v", err)
	}
	defer func() { _ = client.Close() }()

	exerciseUploadDeleteVerify(t, client, "payload.txt")
}

func TestIntegrationFTPS(t *testing.T) {
	client, err := DialFTPS(context.Background(), FTPSDialOptions{
		Host:           "127.0.0.1",
		Port:           ftpControlPort,
		Username:       testUser,
		Password:       testPassword,
		ConnectTimeout: 10 * time.Second,
		StallTimeout:   30 * time.Second,
		// testdata/pure-ftpd.pem is a throwaway self-signed test fixture.
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
	})
	if err != nil {
		t.Fatalf("DialFTPS: %v", err)
	}
	defer func() { _ = client.Close() }()

	exerciseUploadDeleteVerify(t, client, "payload.txt")
}

func TestIntegrationSFTP(t *testing.T) {
	client, err := DialSFTP(context.Background(), SFTPDialOptions{
		Host:           "127.0.0.1",
		Port:           sftpPort,
		Username:       testUser,
		Password:       testPassword,
		ConnectTimeout: 10 * time.Second,
		StallTimeout:   30 * time.Second,
	})
	if err != nil {
		t.Fatalf("DialSFTP: %v", err)
	}
	defer func() { _ = client.Close() }()

	// atmoz/sftp chroots the user at their home directory, which (per
	// OpenSSH's ChrootDirectory rules) must not itself be writable - only
	// the "upload" subdirectory declared when the container started is.
	exerciseUploadDeleteVerify(t, client, "upload/payload.txt")
}
