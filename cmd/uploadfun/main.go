// Command uploadfun is the CLI entry point for the uploadfun library.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"syscall"

	"github.com/alexeyu/uploadfun"
)

const (
	exitOK             = 0
	exitPartialFailure = 1
	exitConfigError    = 2
	exitUsageError     = 3
	// exitSignalAbort is returned when a second interrupt forces an
	// immediate stop; 128+SIGINT is the conventional shell code for it.
	exitSignalAbort = 130
)

// version is overridden at build time via -ldflags "-X main.version=..."
// (Makefile, GoReleaser). resolveVersion falls back to the module version
// when it wasn't, so `go install ...@v1.2.3` still reports the tag.
var version = "dev"

// resolveVersion reports the version with any leading "v" trimmed, so the
// GoReleaser (0.1.0), Makefile, and `go install` (both v0.1.0) build paths
// all print it identically.
func resolveVersion() string {
	return strings.TrimPrefix(rawVersion(), "v")
}

// rawVersion returns the ldflags-injected version if set, else the version
// Go embeds in build info for `go install pkg@vX.Y.Z` builds, else "dev"
// for a plain local build.
func rawVersion() string {
	if version != "dev" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return version
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go handleSignals(sigCh, cancel, os.Exit)

	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// handleSignals cancels ctx on the first interrupt/termination signal so the
// run stops gracefully - no new transfers or retries, results still reported.
// A second signal hard-exits: an in-flight blocking transfer otherwise runs
// to completion, so this is the user's escape hatch from a stuck upload.
func handleSignals(sigCh <-chan os.Signal, cancel context.CancelFunc, exit func(int)) {
	<-sigCh
	cancel()
	<-sigCh
	exit(exitSignalAbort)
}

type cliOptions struct {
	configPath string
	quiet      bool
	verbose    bool
	json       bool
	dryRun     bool
	noVerify   bool
	version    bool
	paths      []string
}

// parseArgs returns nil options (and the exit code to use) for --help,
// --version, and any usage error, so run's caller doesn't need to
// distinguish "asked for help" from "got it wrong" - both just stop.
func parseArgs(args []string, stdout, stderr io.Writer) (*cliOptions, int) {
	fs := flag.NewFlagSet("uploadfun", flag.ContinueOnError)
	fs.SetOutput(stderr)
	opts := &cliOptions{}
	fs.StringVar(&opts.configPath, "config", "", "path to YAML config file (required)")
	fs.BoolVar(&opts.quiet, "quiet", false, "suppress non-error output")
	fs.BoolVar(&opts.verbose, "verbose", false,
		"print the full event stream, including byte-level progress")
	fs.BoolVar(&opts.json, "json", false, "format output as newline-delimited JSON")
	fs.BoolVar(&opts.dryRun, "dry-run", false,
		"connect to each endpoint, verify the target is writable via a "+
			"self-deleting probe file, and report how many files would upload; "+
			"never touches the actual files being sent")
	fs.BoolVar(&opts.noVerify, "no-verify", false, "disable post-upload size/hash verification")
	fs.BoolVar(&opts.version, "version", false, "print version and exit")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: %s <path>... --config <file> [flags]\n\n", fs.Name())
		fs.PrintDefaults()
	}

	paths, err := parseInterleaved(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, exitOK
		}
		return nil, exitUsageError
	}

	if opts.version {
		_, _ = fmt.Fprintln(stdout, "uploadfun", resolveVersion())
		return nil, exitOK
	}

	if opts.quiet && opts.verbose {
		_, _ = fmt.Fprintln(stderr, "uploadfun: --quiet and --verbose are mutually exclusive")
		fs.Usage()
		return nil, exitUsageError
	}
	if opts.configPath == "" {
		_, _ = fmt.Fprintln(stderr, "uploadfun: --config is required")
		fs.Usage()
		return nil, exitUsageError
	}

	opts.paths = paths
	if len(opts.paths) == 0 {
		_, _ = fmt.Fprintln(stderr, "uploadfun: at least one file or directory argument is required")
		fs.Usage()
		return nil, exitUsageError
	}

	return opts, exitOK
}

// parseInterleaved runs fs.Parse repeatedly so flags and positionals may
// appear in any order - stdlib flag otherwise stops at the first
// positional. Everything after the first "--" is taken literally, so
// filenames beginning with "-" can be uploaded (e.g. `--config c.yml --
// -weird.jpg`); the re-parse loop would otherwise re-interpret them as
// flags. Returns the collected positionals, or the first error.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var trailing []string
	if i := slices.Index(args, "--"); i >= 0 {
		trailing = args[i+1:]
		args = args[:i]
	}

	var positionals []string
	rest := args
	for len(rest) > 0 {
		if err := fs.Parse(rest); err != nil {
			return nil, err
		}
		rest = fs.Args()
		if len(rest) == 0 {
			break
		}
		positionals = append(positionals, rest[0])
		rest = rest[1:]
	}
	return append(positionals, trailing...), nil
}

// expandPaths turns positional file/dir arguments into a flat file list:
// directories expand non-recursively, subdirectories and hidden/dotfiles
// are skipped. A direct argument that isn't a regular file (FIFO, device,
// socket) is rejected rather than silently accepted - opening it later
// could hang the upload forever (e.g. a FIFO with no writer).
func expandPaths(paths []string) ([]string, error) {
	var files []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("%s: not a regular file", p)
			}
			files = append(files, p)
			continue
		}

		entries, err := os.ReadDir(p)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
				continue
			}
			entryInfo, err := entry.Info()
			if err != nil {
				return nil, err
			}
			if !entryInfo.Mode().IsRegular() {
				continue
			}
			files = append(files, filepath.Join(p, entry.Name()))
		}
	}
	if err := checkBasenameCollisions(files); err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no regular files found in the given path(s)")
	}
	return files, nil
}

// checkBasenameCollisions rejects inputs that would map to the same
// remote filename (its basename) - e.g. a/img.jpg and b/img.jpg - which
// under the default delete-first overwrite would silently clobber.
func checkBasenameCollisions(files []string) error {
	seen := make(map[string]string, len(files))
	for _, f := range files {
		base := filepath.Base(f)
		if prev, ok := seen[base]; ok {
			return fmt.Errorf(
				"inputs %q and %q both upload to remote name %q", prev, f, base)
		}
		seen[base] = f
	}
	return nil
}

// warnIfConfigReadable prints an advisory to stderr if the config file's
// permissions let the group or others read it - config files routinely
// hold plaintext credentials (see config.yml.example), and unlike a
// missing endpoint field this can't be caught by validation.
func warnIfConfigReadable(path string, stderr io.Writer) {
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	if info.Mode().Perm()&0o044 != 0 {
		_, _ = fmt.Fprintf(stderr,
			"uploadfun: warning: %s is readable by group/other (mode %s); "+
				"it may contain plaintext credentials, consider chmod 600\n",
			path, info.Mode().Perm())
	}
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	opts, code := parseArgs(args, stdout, stderr)
	if opts == nil {
		return code
	}

	files, err := expandPaths(opts.paths)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "uploadfun:", err)
		return exitUsageError
	}

	warnIfConfigReadable(opts.configPath, stderr)

	endpoints, err := uploadfun.LoadConfig(opts.configPath)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "uploadfun: config error:")
		_, _ = fmt.Fprintln(stderr, err)
		return exitConfigError
	}

	p := newPrinter(stdout, stderr, outputModeFor(opts.quiet, opts.verbose), opts.json)
	events := uploadfun.Upload(ctx, files, endpoints, uploadfun.Options{
		DryRun: opts.dryRun, NoVerify: opts.noVerify,
	})
	if processEvents(events, p) {
		return exitPartialFailure
	}
	return exitOK
}
