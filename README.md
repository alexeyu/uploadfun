# uploadfun

[![CI](https://github.com/alexeyu/uploadfun/actions/workflows/ci.yml/badge.svg)](https://github.com/alexeyu/uploadfun/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/alexeyu/uploadfun.svg)](https://pkg.go.dev/github.com/alexeyu/uploadfun)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)

A headless, config-driven uploader that fans one batch of files out to many
FTP, FTPS, and SFTP endpoints at once, concurrently, with retries and
post-upload verification. Built for unattended automation (cron, CI,
folder-watchers), not interactive use: no prompts, no GUI, a single static
binary and one YAML file.

You point it at some files and a list of endpoints; it uploads every file to
every endpoint in parallel, verifies each transfer, retries transient
failures, and exits non-zero if anything failed so a wrapping script can react.

```console
$ uploadfun ./batch --config stocks.yml
[shutterstock] photo1.jpg: uploaded (verified: size) in 1.21s
[dreamstime] photo1.jpg: uploaded (verified: size) in 0.98s
[shutterstock] photo2.jpg: uploaded (verified: size) in 1.15s
[dreamstime] photo2.jpg: uploaded (verified: size) in 1.04s
[shutterstock] done: 2 succeeded, 0 failed in 3.10s
[dreamstime] done: 2 succeeded, 0 failed in 2.87s
```

## Features

- **One batch, many destinations.** Declare endpoints in YAML; add or remove
  one without touching code. Every endpoint uploads in its own goroutine.
- **Three protocols, one model:** `ftp`, `ftps` (explicit AUTH TLS), and
  `sftp` (password or private-key auth, host keys checked against
  `~/.ssh/known_hosts`).
- **Verification on by default.** Every upload's remote size is checked
  against the local file to catch truncation. Disable with `--no-verify`.
- **Retries with reconnect.** A fixed number of attempts per file with a
  fixed delay, each attempt reconnecting from scratch. Configurable globally
  and per endpoint.
- **Fail-fast preflight** (`--dry-run`): connect, authenticate, and prove the
  target directory is writable with a self-deleting probe file, without
  touching the files you are actually sending.
- **Automation-friendly output:** quiet / default / verbose text, or
  newline-delimited JSON (`--json`), with meaningful exit codes and errors
  always on stderr even under `--quiet`.
- **Secrets via env vars:** `${ENV_VAR}` interpolation in any config field,
  resolved at load time. Literal values still allowed.
- **Zero runtime dependencies:** one static binary, no daemon, no config
  server.

## Install

### Homebrew (macOS / Linux)

```console
brew install alexeyu/tap/uploadfun
```

### go install

```console
go install github.com/alexeyu/uploadfun/cmd/uploadfun@latest
```

### Prebuilt binaries

Download the archive for your OS/architecture from the
[latest release](https://github.com/alexeyu/uploadfun/releases/latest),
unpack it, and put `uploadfun` on your `PATH`. Linux, macOS, and Windows on
amd64 and arm64 are built for every release; each archive is listed in
`checksums.txt`.

## Quick start

1. Write a config (`stocks.yml`). Start from
   [`config.yml.example`](config.yml.example):

   ```yaml
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
   ```

2. Preflight it before you trust it to a cron job:

   ```console
   $ export SHUTTERSTOCK_FTP_PASSWORD=...
   $ uploadfun ./batch --config stocks.yml --dry-run
   [shutterstock] dry-run ok: reachable and writable, would upload 12 files
   [dreamstime] dry-run ok: reachable and writable, would upload 12 files
   ```

3. Upload for real:

   ```console
   $ uploadfun ./batch photo-extra.jpg --config stocks.yml
   ```

Positional arguments may be files or directories, mixed freely. Directories
expand **non-recursively**: every regular file is included; subdirectories and
dotfiles are skipped. Two inputs that would land under the same remote name
(same basename) are rejected up front rather than silently clobbering.

## Configuration

Config is a YAML file with a list of `endpoints` plus optional global policy
defaults. Every policy key can be overridden per endpoint.

```yaml
endpoints:
  - name: shutterstock          # unique label, used in output and errors
    protocol: ftps              # ftp | ftps | sftp
    host: ftp.shutterstock.com
    port: 21                    # optional; defaults to the protocol's standard port
    username: myuser
    password: ${SHUTTERSTOCK_FTP_PASSWORD}
    overwrite: delete-first     # delete-first (default) | direct

  - name: dreamstime
    protocol: sftp
    host: sftp.dreamstime.com
    username: myuser
    private_key: ~/.ssh/id_ed25519   # ~ is expanded

  - name: legacy-agency
    protocol: ftp
    host: ftp.legacy-agency.example
    username: myuser
    password: hunter2           # literal secrets allowed, not forced to env vars
    attempts: 5                 # per-endpoint override of the global default below

# Global policy defaults (each overridable per endpoint):
attempts: 3                     # total tries per file
retry_delay: 2s
connect_timeout: 30s            # bounds the whole connect + login, not just the TCP dial
stall_timeout: 5m               # fail a transfer idle this long; 0 disables
max_consecutive_connect_failures: 3   # write an endpoint off after this many connect failures in a row
```

### Endpoint fields

| Field | Applies to | Notes |
|---|---|---|
| `name` | all | Required, unique. Shown in all output. |
| `protocol` | all | `ftp`, `ftps`, or `sftp`. Required. |
| `host` | all | Required. |
| `port` | all | Optional; defaults to the protocol's standard port. |
| `username` | all | Required. |
| `password` | ftp, ftps, sftp | Required for ftp/ftps. For sftp, an alternative or complement to `private_key`. |
| `private_key` | sftp | Path to an SSH private key (`~` expanded). If both this and `password` are set, the key wins and `password` is used only as its passphrase when the key is encrypted. |
| `overwrite` | all | `delete-first` (default) deletes any existing remote file first; `direct` uploads straight over it. |
| `insecure_skip_verify` | ftps | Disables TLS certificate verification. For self-signed / test servers only; never use against a production endpoint. |

### Policy fields (global or per-endpoint)

| Field | Default | Meaning |
|---|---|---|
| `attempts` | `3` | Total tries per file before it is counted as failed. |
| `retry_delay` | `2s` | Fixed wait between attempts (Go duration string). |
| `connect_timeout` | `30s` | Bounds the entire connect + authenticate sequence. |
| `stall_timeout` | `5m` | Fails a transfer that makes no forward progress for this long. `0` disables. |
| `max_consecutive_connect_failures` | `3` | After this many connect failures in a row, the endpoint's remaining files are skipped as unreachable instead of each retrying. |

### Secrets

`${ENV_VAR}` interpolation works in any string field and is resolved when the
config loads. A referenced variable that is unset is a load-time error, so a
typo surfaces immediately instead of as a confusing auth failure mid-run.

Literal secrets are allowed but discouraged. Because a config file can hold
plaintext credentials, `uploadfun` prints a warning if the file is readable by
group or others; `chmod 600 stocks.yml` and keep it out of version control.

## Usage

```
uploadfun <path>... --config <file> [flags]
```

| Flag | Effect |
|---|---|
| `--config <file>` | Path to the YAML config. Required. |
| `--dry-run` | Connect, authenticate, and probe each endpoint for writability, then report how many files would upload. No transfers, no deletes of real files. |
| `--no-verify` | Skip post-upload size verification. |
| `--quiet` | Suppress non-error stdout. Errors still print to stderr. |
| `--verbose` | Print the full event stream, including byte-level progress. |
| `--json` | Emit newline-delimited JSON instead of text (honors the verbosity level). |
| `--version` | Print version and exit. |
| `--help` | Print usage and exit. |

`--quiet` and `--verbose` are mutually exclusive. Flags and paths may appear in
any order.

### Output modes

| Mode | stdout | stderr |
|---|---|---|
| `--quiet` | nothing | errors |
| default | per-file success + per-endpoint summary | errors |
| `--verbose` | full event stream incl. byte-level progress | errors |

Failure events always go to stderr, even under `--quiet`: an unattended job
still needs to see what broke.

### JSON output

`--json` reformats whatever the current verbosity level would print as one JSON
object per line, keeping the same stdout/stderr split. Each line carries a
`type` discriminator (`file_start`, `progress`, `file_success`, `file_error`,
`endpoint_unreachable`, `endpoint_given_up`, `endpoint_done`, `dry_run`):

```console
$ uploadfun ./batch --config stocks.yml --json
{"type":"file_success","endpoint":"shutterstock","file":"batch/photo1.jpg","verifyMethod":"size","durationSec":1.21}
{"type":"endpoint_done","endpoint":"shutterstock","succeeded":1,"failed":0,"durationSec":1.34}
```

### Exit codes

| Code | Meaning |
|---|---|
| `0` | Every file uploaded (and verified) on every endpoint. |
| `1` | Partial failure: at least one file failed after exhausting retries on at least one endpoint. |
| `2` | Configuration error (bad or invalid YAML, validation failures). |
| `3` | Usage error (bad flags, an input path that does not exist). |

## How it works

- **Concurrency.** One goroutine per endpoint, fully parallel. Within an
  endpoint, files upload sequentially over a single reused connection (connect
  once, upload all, disconnect), which amortizes login cost and respects
  servers that cap concurrent connections.
- **Retries.** A fixed attempt count with a fixed delay between tries. Each
  retry reconnects. Upload and verification are retried together: a
  verification failure is treated the same as an upload failure.
- **Verification.** After each upload the remote size is compared against the
  local file (`SIZE` / `Stat`), which catches truncation cheaply and works on
  all three protocols. Hash-based verification is planned but not in v1; SFTP
  has no standard equivalent. `--no-verify` turns it off.
- **SSH host keys.** SFTP endpoints are verified against `~/.ssh/known_hosts`.
  There is no insecure-by-default fallback: a host you have not trusted yet
  fails with an error, the same trust-on-first-use flow as OpenSSH. Connect
  once with `ssh` (or `ssh-keyscan`) to record the key first.
- **Cancellation.** Ctrl-C / SIGTERM cancels cleanly: workers stop starting new
  transfers and retries promptly, though an in-flight blocking transfer runs to
  completion first. Each endpoint still reports its results.

## Use as a Go library

The CLI is a thin wrapper over the `uploadfun` package. `Upload` returns a
channel of typed events you range over:

```go
package main

import (
	"context"
	"fmt"

	"github.com/alexeyu/uploadfun"
)

func main() {
	endpoints, err := uploadfun.LoadConfig("stocks.yml")
	if err != nil {
		panic(err)
	}

	files := []string{"photo1.jpg", "photo2.jpg"}
	events := uploadfun.Upload(context.Background(), files, endpoints, uploadfun.Options{})

	// You MUST keep receiving until the channel closes: workers send
	// unbuffered, so bailing out early would deadlock them.
	for ev := range events {
		switch e := ev.(type) {
		case uploadfun.FileSuccessEvent:
			fmt.Printf("%s -> %s ok\n", e.File, e.Endpoint)
		case uploadfun.FileErrorEvent:
			fmt.Printf("%s -> %s failed: %s\n", e.File, e.Endpoint, e.Reason)
		}
	}
}
```

You can also build `[]uploadfun.Endpoint` yourself instead of loading YAML. See
the [package reference](https://pkg.go.dev/github.com/alexeyu/uploadfun) for the
full event vocabulary and `Options`.

## Building from source

Requires Go (see `go.mod` for the version). A `Makefile` wraps the common
tasks:

```console
make build              # build ./uploadfun with the version stamped in
make test               # unit tests, race detector on
make test-integration   # integration tests against real servers in Docker
make lint               # golangci-lint
```

Integration tests spin up real `pure-ftpd` and `atmoz/sftp` containers via the
`docker` CLI and are gated behind the `integration` build tag, so a plain
`go test ./...` never needs Docker. They skip themselves with a message if
Docker is not reachable.

## Releasing (maintainers)

Releases are cut by [GoReleaser](https://goreleaser.com) from a tag:

```console
git tag v0.1.0
git push origin v0.1.0
```

The `Release` workflow then cross-compiles every target, attaches
checksummed archives to the GitHub Release, and updates the Homebrew tap.
Publishing the Homebrew cask requires a `HOMEBREW_TAP_GITHUB_TOKEN` repository
secret: a token with write access to `alexeyu/homebrew-tap`. If that secret is
absent the release still succeeds and only the cask push is skipped. Validate
config changes locally with `goreleaser check` and dry-run a full build with
`goreleaser release --snapshot --clean`.

## Stability

Pre-1.0. The tool works and is tested, but the config schema and the Go library
API may still change between minor versions. `v1.0.0` will mark both as stable.

## License

[MIT](LICENSE).
