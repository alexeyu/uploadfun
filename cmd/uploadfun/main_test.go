package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseArgsRequiresConfig(t *testing.T) {
	var stderr bytes.Buffer
	opts, code := parseArgs([]string{"a.jpg"}, &stderr)
	if opts != nil {
		t.Fatal("expected nil opts when --config is missing")
	}
	if code != exitUsageError {
		t.Errorf("expected exitUsageError, got %d", code)
	}
	if !strings.Contains(stderr.String(), "--config is required") {
		t.Errorf("expected a helpful message, got %q", stderr.String())
	}
}

func TestParseArgsRequiresAtLeastOnePath(t *testing.T) {
	var stderr bytes.Buffer
	opts, code := parseArgs([]string{"--config", "x.yaml"}, &stderr)
	if opts != nil {
		t.Fatal("expected nil opts when no paths are given")
	}
	if code != exitUsageError {
		t.Errorf("expected exitUsageError, got %d", code)
	}
}

func TestParseArgsQuietAndVerboseAreMutuallyExclusive(t *testing.T) {
	var stderr bytes.Buffer
	opts, code := parseArgs([]string{"--config", "x.yaml", "--quiet", "--verbose", "a.jpg"}, &stderr)
	if opts != nil {
		t.Fatal("expected nil opts for conflicting flags")
	}
	if code != exitUsageError {
		t.Errorf("expected exitUsageError, got %d", code)
	}
}

func TestParseArgsValid(t *testing.T) {
	var stderr bytes.Buffer
	opts, code := parseArgs([]string{"--config", "x.yaml", "--json", "a.jpg", "dir/"}, &stderr)
	if opts == nil {
		t.Fatalf("expected valid opts, got nil (code=%d, stderr=%q)", code, stderr.String())
	}
	if opts.configPath != "x.yaml" || !opts.json || opts.quiet || opts.verbose {
		t.Errorf("unexpected opts: %+v", opts)
	}
	if len(opts.paths) != 2 || opts.paths[0] != "a.jpg" || opts.paths[1] != "dir/" {
		t.Errorf("unexpected paths: %v", opts.paths)
	}
}

func TestParseArgsHelp(t *testing.T) {
	var stderr bytes.Buffer
	opts, code := parseArgs([]string{"--help"}, &stderr)
	if opts != nil {
		t.Fatal("expected nil opts for --help")
	}
	if code != exitOK {
		t.Errorf("expected exitOK for --help, got %d", code)
	}
}

func TestExpandPaths(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.jpg"), "a")
	writeFile(t, filepath.Join(dir, "b.txt"), "b")
	writeFile(t, filepath.Join(dir, ".hidden"), "hidden")
	if err := os.Mkdir(filepath.Join(dir, "subdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "subdir", "nested.jpg"), "nested")

	standaloneFile := filepath.Join(t.TempDir(), "standalone.png")
	writeFile(t, standaloneFile, "standalone")

	files, err := expandPaths([]string{dir, standaloneFile})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	want := map[string]bool{
		filepath.Join(dir, "a.jpg"): true,
		filepath.Join(dir, "b.txt"): true,
		standaloneFile:              true,
	}
	if len(files) != len(want) {
		t.Fatalf("expected %d files, got %d: %v", len(want), len(files), files)
	}
	for _, f := range files {
		if !want[f] {
			t.Errorf("unexpected file in expansion: %s", f)
		}
	}
}

func TestExpandPathsNonexistent(t *testing.T) {
	if _, err := expandPaths([]string{"/nonexistent/path/xyz"}); err == nil {
		t.Fatal("expected an error for a nonexistent path")
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRunConfigError(t *testing.T) {
	inputFile := filepath.Join(t.TempDir(), "a.jpg")
	writeFile(t, inputFile, "data")

	var stdout, stderr bytes.Buffer
	code := run(nil, []string{"--config", "/nonexistent/config.yaml", inputFile}, &stdout, &stderr)
	if code != exitConfigError {
		t.Errorf("expected exitConfigError, got %d", code)
	}
	if !strings.Contains(stderr.String(), "config error") {
		t.Errorf("expected a config error message, got %q", stderr.String())
	}
}

func TestRunUsageErrorForMissingInputPath(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	writeFile(t, configPath, "endpoints:\n  - name: a\n    protocol: ftp\n    host: h\n    username: u\n    password: p\n")

	var stdout, stderr bytes.Buffer
	code := run(nil, []string{"--config", configPath, "/nonexistent/input.jpg"}, &stdout, &stderr)
	if code != exitUsageError {
		t.Errorf("expected exitUsageError, got %d", code)
	}
}
