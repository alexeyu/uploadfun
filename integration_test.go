//go:build integration

// Root-package integration test, gated behind the "integration" build
// tag since it needs Docker. Skipped (not failed) if Docker isn't
// reachable. Unlike internal/transport's integration tests - which dial
// transport.DialFTPS/DialSFTP directly - this drives the public
// LoadConfig/Upload entry points, so it's the only place that exercises
// dispatch.go's endpoint-worker logic (retry budgets, give-up-early on a
// permanent error, InsecureSkipVerify wiring) against real servers rather
// than the fakeUploader mocks in dispatch_test.go. Run with:
//
//	go test -tags integration .
package uploadfun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alexeyu/uploadfun/internal/testdocker"
	"github.com/alexeyu/uploadfun/internal/testservers"
)

const (
	rootFTPSContainerName = "uploadfun-it-root-pureftpd"
	rootFTPSControlPort   = 12131
	rootFTPSDataPorts     = "30020-30029"
	rootFTPSUser          = "testuser"
	rootFTPSPassword      = "testpass"

	rootSFTPContainerName = "uploadfun-it-root-sftp"
	rootSFTPPort          = 12132
	rootSFTPUser          = "testuser"
	rootSFTPPassword      = "testpass"
)

var rootITHome string

func TestMain(m *testing.M) {
	if !testdocker.Available() {
		fmt.Println("skipping integration tests: docker not available")
		os.Exit(0)
	}

	var err error
	rootITHome, err = os.MkdirTemp("", "uploadfun-root-integration-home")
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
	// StartAtmozSFTP populates $HOME/.ssh/known_hosts; DialSFTP reads it.
	if err := os.Setenv("HOME", rootITHome); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	stopFTPS := startRootFTPS()
	stopSFTP := startRootSFTP()

	// Deferred cleanup would never run here since os.Exit skips defers,
	// so clean up explicitly before exiting rather than deferring it.
	code := m.Run()
	stopSFTP()
	stopFTPS()
	_ = os.RemoveAll(rootITHome)

	os.Exit(code)
}

// startRootFTPS starts a pure-ftpd container presenting the same
// self-signed cert internal/transport's integration tests use, on a port
// range distinct from theirs so both suites can run concurrently.
func startRootFTPS() func() {
	certPath, err := filepath.Abs("internal/transport/testdata/pure-ftpd.pem")
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	return testservers.StartPureFTPD(testservers.PureFTPDOptions{
		ContainerName: rootFTPSContainerName,
		ControlPort:   rootFTPSControlPort,
		DataPortRange: rootFTPSDataPorts,
		CertPath:      certPath,
		Username:      rootFTPSUser,
		Password:      rootFTPSPassword,
	})
}

// startRootSFTP starts an atmoz/sftp container, on a port distinct from
// internal/transport's so both suites can run concurrently. Its chroot
// root (the SFTP session's default cwd, where Upload's filepath.Base
// remote names land) is deliberately left unwritable - only "upload" is -
// so TestIntegrationSFTPPermissionDeniedGivesUpEndToEnd can rely on it to
// trigger a real SSH_FX_PERMISSION_DENIED.
func startRootSFTP() func() {
	return testservers.StartAtmozSFTP(testservers.AtmozSFTPOptions{
		ContainerName:  rootSFTPContainerName,
		Port:           rootSFTPPort,
		Username:       rootSFTPUser,
		Password:       rootSFTPPassword,
		ChrootSubdir:   "upload",
		KnownHostsHome: rootITHome,
	})
}

func rootFTPSConfig(t *testing.T, insecureSkipVerify string) string {
	t.Helper()
	return writeConfig(t, fmt.Sprintf(`
endpoints:
  - name: root-ftps
    protocol: ftps
    host: 127.0.0.1
    port: %d
    username: %s
    password: %s
    insecure_skip_verify: %s
`, rootFTPSControlPort, rootFTPSUser, rootFTPSPassword, insecureSkipVerify))
}

func uploadOneFile(t *testing.T, endpoints []Endpoint) []UploadEvent {
	t.Helper()
	dir := t.TempDir()
	localPath := filepath.Join(dir, "payload.txt")
	if err := os.WriteFile(localPath, []byte("integration test payload\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return collectEvents(Upload(context.Background(), []string{localPath}, endpoints, Options{}))
}

// TestIntegrationInsecureSkipVerifyEndToEnd proves the insecure_skip_verify
// YAML flag actually reaches the TLS handshake, not just config.go's
// validation: a self-signed cert must fail verification by default and
// succeed once the flag is set, driven entirely through LoadConfig/Upload.
func TestIntegrationInsecureSkipVerifyEndToEnd(t *testing.T) {
	t.Run("true permits a self-signed cert", func(t *testing.T) {
		endpoints, err := LoadConfig(rootFTPSConfig(t, "true"))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}

		// A stray FileErrorEvent from the endpoint's normal retry budget
		// (e.g. the container's control port accepting TCP fractionally
		// before pure-ftpd is ready to TLS-handshake) isn't what this test
		// is after - only whether the upload eventually succeeds, which it
		// can't if the self-signed cert were actually being rejected.
		events := uploadOneFile(t, endpoints)
		if counts := countByType(events); counts["success"] != 1 {
			t.Errorf("expected the upload to eventually succeed, got %+v events", counts)
		}
	})

	t.Run("false rejects a self-signed cert", func(t *testing.T) {
		endpoints, err := LoadConfig(rootFTPSConfig(t, "false"))
		if err != nil {
			t.Fatalf("LoadConfig: %v", err)
		}

		events := uploadOneFile(t, endpoints)
		if counts := countByType(events); counts["success"] != 0 {
			t.Errorf("expected the self-signed cert to be rejected, got %+v events", counts)
		}
	})
}

func rootSFTPEndpoint() Endpoint {
	return Endpoint{
		Name:                          "root-sftp",
		Protocol:                      ProtocolSFTP,
		Host:                          "127.0.0.1",
		Port:                          rootSFTPPort,
		Username:                      rootSFTPUser,
		Password:                      rootSFTPPassword,
		Overwrite:                     OverwriteDeleteFirst,
		Attempts:                      3,
		RetryDelay:                    100 * time.Millisecond,
		ConnectTimeout:                10 * time.Second,
		StallTimeout:                  30 * time.Second,
		MaxConsecutiveConnectFailures: 3,
	}
}

// TestIntegrationSFTPPermissionDeniedGivesUpEndToEnd validates
// dispatch.go's isPermanentErr/EndpointGivenUpEvent path against a real
// permission-denied response, not the mocked errors.New("...") stand-ins
// dispatch_test.go uses. isPermanentErr's own comment admits it's tuned
// to how pkg/sftp specifically normalizes SSH_FX_PERMISSION_DENIED into
// os.ErrPermission - a mapping only a real SFTP server can confirm.
//
// atmoz/sftp's chroot root (where Upload's filepath.Base remote names
// land, since no per-endpoint remote directory exists to redirect them
// into "upload/") is unwritable by the login user, so every upload on
// this endpoint fails the same way - the endpoint should give up after
// the first file's first attempt, not burn its full Attempts budget on
// it nor attempt the second file at all.
func TestIntegrationSFTPPermissionDeniedGivesUpEndToEnd(t *testing.T) {
	dir := t.TempDir()
	var files []string
	for _, name := range []string{"a.txt", "b.txt"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("payload\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		files = append(files, path)
	}

	events := collectEvents(Upload(
		context.Background(), files, []Endpoint{rootSFTPEndpoint()}, Options{},
	))

	counts := countByType(events)
	if counts["success"] != 0 {
		t.Errorf("expected no successful uploads, got %+v events", counts)
	}
	if counts["error"] != 1 {
		t.Errorf(
			"expected exactly 1 error (no retries burned on a permanent error), got %+v events",
			counts)
	}
	if counts["given_up"] != 1 {
		t.Fatalf("expected exactly 1 EndpointGivenUpEvent, got %+v events", counts)
	}

	var givenUp EndpointGivenUpEvent
	for _, e := range events {
		if g, ok := e.(EndpointGivenUpEvent); ok {
			givenUp = g
		}
	}
	if len(givenUp.SkippedFiles) != 1 || givenUp.SkippedFiles[0] != files[1] {
		t.Errorf("expected the second file to be reported skipped, got %+v", givenUp.SkippedFiles)
	}
}
