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
	"strings"

	"github.com/alexeyu/uploadfun"
)

const (
	exitOK             = 0
	exitPartialFailure = 1
	exitConfigError    = 2
	exitUsageError     = 3
)

// version is overridden at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
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
// distinguish "asked for help" from "got it wrong" — both just stop.
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
		"connect, authenticate, and list each endpoint's remote directory; "+
			"no transfer, no delete, no writes")
	fs.BoolVar(&opts.noVerify, "no-verify", false, "disable post-upload size/hash verification")
	fs.BoolVar(&opts.version, "version", false, "print version and exit")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: %s <path>... --config <file> [flags]\n\n", fs.Name())
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil, exitOK
		}
		return nil, exitUsageError
	}

	if opts.version {
		_, _ = fmt.Fprintln(stdout, "uploadfun", version)
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

	opts.paths = fs.Args()
	if len(opts.paths) == 0 {
		_, _ = fmt.Fprintln(stderr, "uploadfun: at least one file or directory argument is required")
		fs.Usage()
		return nil, exitUsageError
	}

	return opts, exitOK
}

// expandPaths turns the positional file/dir arguments into a flat file
// list: directories expand non-recursively, every regular file included,
// no extension filtering, subdirectories and hidden/dotfiles silently
// skipped (see ARCHITECTURE.md "CLI" invocation & input files).
func expandPaths(paths []string) ([]string, error) {
	var files []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
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
	return files, nil
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
