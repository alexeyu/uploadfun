//go:build unix

package main

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// Uses a FIFO to exercise the non-regular-file rejection; syscall.Mkfifo
// only exists on Unix, so this test is built there only.
func TestExpandPathsRejectsNonRegularDirectArg(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unsupported on this platform: %v", err)
	}

	_, err := expandPaths([]string{fifo})
	if err == nil {
		t.Fatal("expected an error for a non-regular file passed directly")
	}
	if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("expected a helpful message, got %q", err.Error())
	}
}
