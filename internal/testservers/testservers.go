// Package testservers starts the real FTP/FTPS/SFTP servers this repo's
// integration tests (internal/transport and the root package) run
// against, on top of internal/testdocker's generic container lifecycle
// helpers. It has no build tag itself since it carries no test-only
// imports, but nothing outside integration tests references it.
package testservers

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/alexeyu/uploadfun/internal/testdocker"
)

// PureFTPDOptions configures a stilliard/pure-ftpd:hardened container.
type PureFTPDOptions struct {
	ContainerName string
	ControlPort   int
	// DataPortRange is "first-last": published to the host and also set
	// as the container's own passive port range, so its PASV replies
	// point somewhere actually reachable (needed since two containers -
	// one per test suite - may run at once, each needing its own range).
	DataPortRange      string
	CertPath           string
	Username, Password string
}

// StartPureFTPD starts a pure-ftpd container per opts and waits for its
// control port to accept connections. Exits the process on failure -
// meant to be called from TestMain, where a deferred cleanup would never
// run anyway since m.Run()'s caller exits via os.Exit.
func StartPureFTPD(opts PureFTPDOptions) func() {
	testdocker.Run(opts.ContainerName,
		"-p", fmt.Sprintf("%d:21", opts.ControlPort),
		"-p", opts.DataPortRange+":"+opts.DataPortRange,
		"-v", opts.CertPath+":/etc/ssl/private/pure-ftpd.pem:ro",
		"-e", "PUBLICHOST=127.0.0.1",
		"-e", "FTP_USER_NAME="+opts.Username,
		"-e", "FTP_USER_PASS="+opts.Password,
		"-e", "FTP_USER_HOME=/home/ftpusers/"+opts.Username,
		// FTP_PASSIVE_PORTS, not ADDED_FLAGS: the image's run.sh appends
		// its own default "-p 30000:30009" after ADDED_FLAGS, so a
		// --passiveportrange placed there gets silently overridden.
		"-e", "FTP_PASSIVE_PORTS="+strings.Replace(opts.DataPortRange, "-", ":", 1),
		"-e", "ADDED_FLAGS=--tls=1",
		"stilliard/pure-ftpd:hardened",
	)
	testdocker.WaitForPort(fmt.Sprintf("127.0.0.1:%d", opts.ControlPort), 30*time.Second)

	return func() { testdocker.Remove(opts.ContainerName) }
}

// AtmozSFTPOptions configures an atmoz/sftp container.
type AtmozSFTPOptions struct {
	ContainerName      string
	Port               int
	Username, Password string
	// ChrootSubdir is the one writable subdirectory atmoz/sftp creates
	// under the user's chroot - the chroot root itself must stay
	// unwritable per OpenSSH's ChrootDirectory rules.
	ChrootSubdir string
	// KnownHostsHome is $HOME for the process that will dial in. The
	// container's SSH host key is regenerated every run, so this
	// populates .ssh/known_hosts there before any test connects -
	// DialSFTP refuses to connect to an unrecognized host otherwise.
	KnownHostsHome string
}

// StartAtmozSFTP starts an atmoz/sftp container per opts, waits for its
// port to accept connections, and populates KnownHostsHome's known_hosts
// with the container's host key. Exits the process on failure.
func StartAtmozSFTP(opts AtmozSFTPOptions) func() {
	testdocker.Run(opts.ContainerName,
		"-p", fmt.Sprintf("%d:22", opts.Port),
		"atmoz/sftp:latest",
		fmt.Sprintf("%s:%s:1001:1001:%s", opts.Username, opts.Password, opts.ChrootSubdir),
	)
	testdocker.WaitForPort(fmt.Sprintf("127.0.0.1:%d", opts.Port), 30*time.Second)
	populateKnownHosts(opts.Port, opts.KnownHostsHome)

	return func() { testdocker.Remove(opts.ContainerName) }
}

func populateKnownHosts(port int, home string) {
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0o700); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	var scanOut []byte
	var scanErr error
	for attempt := 0; attempt < 5; attempt++ {
		scanOut, scanErr = exec.Command(
			"ssh-keyscan", "-p", fmt.Sprintf("%d", port), "-t", "ed25519,rsa", "127.0.0.1",
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
	if err := os.WriteFile(filepath.Join(home, ".ssh", "known_hosts"), scanOut, 0o600); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
