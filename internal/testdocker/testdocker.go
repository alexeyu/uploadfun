// Package testdocker provides minimal Docker container lifecycle helpers
// shared by this repo's integration tests (gated behind the "integration"
// build tag in their own packages). It has no build tag itself since it
// carries no test-only imports, but nothing outside integration tests
// references it.
package testdocker

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"time"
)

// Available reports whether a Docker daemon is reachable.
func Available() bool {
	return exec.Command("docker", "info").Run() == nil
}

// Run starts a container named name via `docker run -d --name name
// runArgs...`, first force-removing any leftover container with the same
// name from a previous run. It exits the process on failure: this is
// meant to be called from TestMain, where a deferred cleanup would never
// run anyway since m.Run()'s caller exits via os.Exit.
func Run(name string, runArgs ...string) {
	Remove(name)

	args := append([]string{"run", "-d", "--name", name}, runArgs...)
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		fmt.Printf("docker %v failed: %v\n%s\n", args, err, out)
		os.Exit(1)
	}
}

// Remove force-removes a container by name, ignoring errors so it's safe
// to call as a best-effort cleanup whether or not the container exists.
func Remove(name string) {
	_ = exec.Command("docker", "rm", "-f", name).Run()
}

// WaitForPort blocks until addr accepts TCP connections, or exits the
// process once timeout elapses.
func WaitForPort(addr string, timeout time.Duration) {
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
