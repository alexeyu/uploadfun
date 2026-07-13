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
// across every file in the batch for as long as nothing fails; any failure
// disconnects and the next attempt reconnects from scratch.
type Uploader interface {
	Connect(ctx context.Context, ep Endpoint) error
	Disconnect(ctx context.Context) error
	// Delete must treat "remote file doesn't exist" as success (a no-op),
	// not an error - it's called unconditionally before every upload when
	// Endpoint.Overwrite is OverwriteDeleteFirst, including the very first
	// upload of a file that was never there.
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

func dispatch(
	ctx context.Context, files []string, endpoints []Endpoint, opts Options, events chan<- UploadEvent,
) {
	var wg sync.WaitGroup
	for _, ep := range endpoints {
		wg.Go(func() {
			runEndpoint(ctx, ep, files, opts, events)
		})
	}
	wg.Wait()
}

func runEndpoint(
	ctx context.Context, ep Endpoint, files []string, opts Options, events chan<- UploadEvent,
) {
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

	(&endpointWorker{ctx: ctx, ep: ep, opts: opts, events: events, up: up}).run(files)
}

// endpointWorker uploads a batch of files to one endpoint over a single
// reused connection, reconnecting and retrying per the endpoint's
// Attempts/RetryDelay budget. It owns the connection's lifecycle so the
// retry loop and file iteration don't have to track it directly.
type endpointWorker struct {
	ctx    context.Context
	ep     Endpoint
	opts   Options
	events chan<- UploadEvent
	up     Uploader

	connected bool
}

// run uploads every file in order, tallies the outcome, and emits the
// terminating EndpointDoneEvent.
func (w *endpointWorker) run(files []string) {
	defer w.disconnect()

	succeeded, failed := 0, 0
	for i, file := range files {
		if w.ctx.Err() != nil {
			failed += len(files) - i
			break
		}
		if w.uploadFile(file) {
			succeeded++
		} else {
			failed++
		}
	}

	w.events <- EndpointDoneEvent{Endpoint: w.ep.Name, Succeeded: succeeded, Failed: failed}
}

// uploadFile uploads one file, retrying up to the endpoint's Attempts
// budget, and reports whether it ultimately succeeded.
func (w *endpointWorker) uploadFile(file string) bool {
	remoteName := filepath.Base(file)
	for attempt := 1; attempt <= w.ep.Attempts; attempt++ {
		if w.ctx.Err() != nil {
			return false
		}

		if !w.connected {
			switch w.connect(file, attempt) {
			case connectCanceled:
				return false
			case connectFailed:
				w.sleepBeforeRetry(attempt)
				continue
			}
		}

		method, err := w.transfer(file, remoteName)
		if err != nil {
			w.events <- FileErrorEvent{
				Endpoint: w.ep.Name, File: file, Attempt: attempt, Reason: err.Error(), Err: err,
			}
			w.disconnect()
			w.sleepBeforeRetry(attempt)
			continue
		}

		w.events <- FileSuccessEvent{Endpoint: w.ep.Name, File: file, VerifyMethod: method}
		return true
	}
	return false
}

type connectResult int

const (
	connectOK connectResult = iota
	connectFailed
	connectCanceled
)

// connect establishes the reused connection, bounding the whole
// connect+login sequence with the endpoint's ConnectTimeout. A failure
// caused only by ctx cancellation reports connectCanceled and emits no
// event, since it isn't a real endpoint failure.
func (w *endpointWorker) connect(file string, attempt int) connectResult {
	connectCtx, cancel := context.WithTimeout(w.ctx, w.ep.ConnectTimeout)
	err := w.up.Connect(connectCtx, w.ep)
	cancel()
	if err != nil {
		if w.ctx.Err() != nil {
			return connectCanceled
		}
		w.events <- FileErrorEvent{
			Endpoint: w.ep.Name, File: file, Attempt: attempt,
			Reason: "connect: " + err.Error(), Err: err,
		}
		return connectFailed
	}
	w.connected = true
	return connectOK
}

// transfer runs the delete/upload/verify sequence for one file on the
// live connection, returning the verification method used ("" if
// verification was skipped) or the first error encountered.
func (w *endpointWorker) transfer(localPath, remoteName string) (verifyMethod string, err error) {
	if w.ep.Overwrite == OverwriteDeleteFirst {
		if err := w.up.Delete(w.ctx, remoteName); err != nil {
			return "", fmt.Errorf("delete existing remote file: %w", err)
		}
	}

	if err := w.up.Upload(w.ctx, localPath, remoteName, func(sent, total int64) {
		w.events <- ProgressEvent{
			Endpoint: w.ep.Name, File: localPath, BytesSent: sent, TotalBytes: total,
		}
	}); err != nil {
		return "", fmt.Errorf("upload: %w", err)
	}

	if w.opts.NoVerify {
		return "", nil
	}
	method, err := w.up.Verify(w.ctx, localPath, remoteName)
	if err != nil {
		return "", fmt.Errorf("verify: %w", err)
	}
	return method, nil
}

// disconnect closes the connection if one is open, resetting state so the
// next attempt reconnects from scratch. Safe to call when not connected.
func (w *endpointWorker) disconnect() {
	if w.connected {
		_ = w.up.Disconnect(w.ctx)
		w.connected = false
	}
}

// sleepBeforeRetry waits RetryDelay before the next attempt, unless this
// was the final attempt (in which case there's nothing to wait for).
func (w *endpointWorker) sleepBeforeRetry(attempt int) {
	if attempt < w.ep.Attempts {
		sleepRetryDelay(w.ctx, w.ep.RetryDelay)
	}
}

// runDryRun performs the --dry-run connectivity check for one endpoint:
// connect, authenticate, list the remote directory, disconnect - never
// touching files.
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
