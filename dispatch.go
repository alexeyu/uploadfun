package uploadfun

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"
)

// Uploader is the per-protocol transport implementation an endpoint
// worker drives. One Uploader is connected once per Endpoint and reused
// across every file in the batch for as long as nothing fails (see
// ARCHITECTURE.md "Concurrency & retry model"); any failure disconnects
// and the next attempt reconnects from scratch.
//
// Delete must treat "remote file doesn't exist" as success (a no-op),
// not an error — it's called unconditionally before every upload when
// Endpoint.Overwrite is OverwriteDeleteFirst, including the very first
// upload of a file that was never there.
type Uploader interface {
	Connect(ctx context.Context, ep Endpoint) error
	Disconnect(ctx context.Context) error
	Delete(ctx context.Context, remoteName string) error
	Upload(ctx context.Context, localPath, remoteName string, progress func(sent, total int64)) error
	// Verify compares the just-uploaded local file against its remote
	// copy. method describes what verification was actually performed
	// (e.g. "size", "size+hash"), so callers can surface the weaker
	// size-only guarantee distinctly rather than silently.
	Verify(ctx context.Context, localPath, remoteName string) (method string, err error)
	// List returns remote directory entries; used only for --dry-run.
	List(ctx context.Context) ([]string, error)
}

// newUploader selects the Uploader implementation for protocol. It's a
// package variable so tests can substitute a fake, and later build steps
// (PLAN.md tasks 4-7) register the real ftp/ftps/sftp transports here.
var newUploader = func(protocol Protocol) (Uploader, error) {
	return nil, fmt.Errorf("no transport registered for protocol %q", protocol)
}

func dispatch(ctx context.Context, files []string, endpoints []Endpoint, opts Options, events chan<- UploadEvent) {
	var wg sync.WaitGroup
	for _, ep := range endpoints {
		wg.Add(1)
		go func(ep Endpoint) {
			defer wg.Done()
			runEndpoint(ctx, ep, files, opts, events)
		}(ep)
	}
	wg.Wait()
}

func runEndpoint(ctx context.Context, ep Endpoint, files []string, opts Options, events chan<- UploadEvent) {
	up, err := newUploader(ep.Protocol)
	if err != nil {
		if opts.DryRun {
			events <- DryRunEvent{Endpoint: ep.Name, Err: err}
		} else {
			failAllFiles(ep, files, err, events)
		}
		return
	}

	if opts.DryRun {
		runDryRun(ctx, up, ep, events)
		return
	}

	connected := false
	defer func() {
		if connected {
			_ = up.Disconnect(ctx)
		}
	}()

	succeeded, failed := 0, 0
	for _, file := range files {
		if ctx.Err() != nil {
			failed += len(files) - succeeded - failed
			break
		}

		remoteName := filepath.Base(file)
		ok := false
		for attempt := 1; attempt <= ep.Attempts; attempt++ {
			if !connected {
				connectCtx, cancel := context.WithTimeout(ctx, ep.ConnectTimeout)
				connErr := up.Connect(connectCtx, ep)
				cancel()
				if connErr != nil {
					events <- FileErrorEvent{Endpoint: ep.Name, File: file, Attempt: attempt, Reason: "connect", Err: connErr}
					if attempt < ep.Attempts {
						sleepRetryDelay(ctx, ep.RetryDelay)
					}
					continue
				}
				connected = true
			}

			method, upErr := attemptUpload(ctx, up, ep, file, remoteName, opts, events)
			if upErr != nil {
				events <- FileErrorEvent{Endpoint: ep.Name, File: file, Attempt: attempt, Reason: upErr.Error(), Err: upErr}
				_ = up.Disconnect(ctx)
				connected = false
				if attempt < ep.Attempts {
					sleepRetryDelay(ctx, ep.RetryDelay)
				}
				continue
			}

			events <- FileSuccessEvent{Endpoint: ep.Name, File: file, VerifyMethod: method}
			ok = true
			break
		}
		if ok {
			succeeded++
		} else {
			failed++
		}
	}

	events <- EndpointDoneEvent{Endpoint: ep.Name, Succeeded: succeeded, Failed: failed}
}

func attemptUpload(ctx context.Context, up Uploader, ep Endpoint, localPath, remoteName string, opts Options, events chan<- UploadEvent) (verifyMethod string, err error) {
	if ep.Overwrite == OverwriteDeleteFirst {
		if err := up.Delete(ctx, remoteName); err != nil {
			return "", fmt.Errorf("delete existing remote file: %w", err)
		}
	}

	if err := up.Upload(ctx, localPath, remoteName, func(sent, total int64) {
		events <- ProgressEvent{Endpoint: ep.Name, File: localPath, BytesSent: sent, TotalBytes: total}
	}); err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}

	if opts.NoVerify {
		return "", nil
	}
	method, err := up.Verify(ctx, localPath, remoteName)
	if err != nil {
		return "", fmt.Errorf("verify: %w", err)
	}
	return method, nil
}

// runDryRun performs the --dry-run connectivity check for one endpoint:
// connect, authenticate, list the remote directory, disconnect — never
// touching files, matching "no transfer, no delete, no writes" (see
// ARCHITECTURE.md "CLI" other flags).
func runDryRun(ctx context.Context, up Uploader, ep Endpoint, events chan<- UploadEvent) {
	connectCtx, cancel := context.WithTimeout(ctx, ep.ConnectTimeout)
	err := up.Connect(connectCtx, ep)
	cancel()
	if err != nil {
		events <- DryRunEvent{Endpoint: ep.Name, Err: err}
		return
	}
	defer func() { _ = up.Disconnect(ctx) }()

	entries, err := up.List(ctx)
	if err != nil {
		events <- DryRunEvent{Endpoint: ep.Name, Err: err}
		return
	}
	events <- DryRunEvent{Endpoint: ep.Name, Entries: entries}
}

func failAllFiles(ep Endpoint, files []string, err error, events chan<- UploadEvent) {
	for _, f := range files {
		events <- FileErrorEvent{Endpoint: ep.Name, File: f, Attempt: 1, Reason: err.Error(), Err: err}
	}
	events <- EndpointDoneEvent{Endpoint: ep.Name, Succeeded: 0, Failed: len(files)}
}

func sleepRetryDelay(ctx context.Context, d time.Duration) {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}
