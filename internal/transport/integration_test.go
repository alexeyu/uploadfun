//go:build integration

// Real-server integration tests, gated behind the "integration" build
// tag since they need Docker. Skipped (not failed) if Docker isn't
// reachable. Run with:
//
//	go test -tags integration ./internal/transport/...
package transport

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alexeyu/uploadfun/internal/testdocker"
	"github.com/alexeyu/uploadfun/internal/testservers"
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
	if !testdocker.Available() {
		fmt.Println("skipping integration tests: docker not available")
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

func startPureFTPD() func() {
	certPath, err := filepath.Abs("testdata/pure-ftpd.pem")
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	return testservers.StartPureFTPD(testservers.PureFTPDOptions{
		ContainerName: ftpContainerName,
		ControlPort:   ftpControlPort,
		DataPortRange: ftpDataPorts,
		CertPath:      certPath,
		Username:      testUser,
		Password:      testPassword,
	})
}

func startAtmozSFTP() func() {
	return testservers.StartAtmozSFTP(testservers.AtmozSFTPOptions{
		ContainerName:  sftpContainerName,
		Port:           sftpPort,
		Username:       testUser,
		Password:       testPassword,
		ChrootSubdir:   "upload",
		KnownHostsHome: itHome,
	})
}

// verifyingUploader is the subset of FTPClient/SFTPClient that
// exerciseUploadDeleteVerify drives; both satisfy it structurally.
type verifyingUploader interface {
	Upload(remoteName string, r io.Reader, size int64, progress func(sent, total int64)) error
	Verify(localPath, remoteName string) (string, error)
	Delete(remoteName string) error
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

// TestIntegrationSFTPLargeUploadIntegrity checks content, not just size,
// for an upload spanning many SFTP write packets - the kind of transfer
// exercised by UseConcurrentWrites (internal/transport/sftp.go). Verify
// is size-only, and exerciseUploadDeleteVerify's tiny payload fits in a
// single packet, so neither would catch a chunking/offset regression in
// the multi-packet path.
func TestIntegrationSFTPLargeUploadIntegrity(t *testing.T) {
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

	// 8MiB spans hundreds of the client's 32KiB write packets.
	content := make([]byte, 8*1024*1024)
	if _, err := rand.Read(content); err != nil {
		t.Fatalf("generate payload: %v", err)
	}

	const remoteName = "upload/large.bin"
	var lastSent int64
	uploadErr := client.Upload(remoteName, bytes.NewReader(content), int64(len(content)),
		func(sent, total int64) { lastSent = sent })
	if uploadErr != nil {
		t.Fatalf("Upload: %v", uploadErr)
	}
	if lastSent != int64(len(content)) {
		t.Errorf("expected final progress callback to report %d bytes sent, got %d",
			len(content), lastSent)
	}
	defer func() { _ = client.Delete(remoteName) }()

	f, err := client.sftp.Open(remoteName)
	if err != nil {
		t.Fatalf("open remote file for readback: %v", err)
	}
	defer func() { _ = f.Close() }()

	got, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read back uploaded file: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("uploaded content does not match source (got %d bytes, want %d bytes)",
			len(got), len(content))
	}
}
