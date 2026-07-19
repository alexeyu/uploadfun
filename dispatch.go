package uploadfun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// uploader is the per-protocol transport implementation an endpoint
// worker drives: connected once per Endpoint and reused across the batch
// until a failure disconnects it and the next attempt reconnects fresh.
type uploader interface {
	Connect(ctx context.Context, ep Endpoint) error
	Disconnect(ctx context.Context) error
	// Delete must treat "remote file doesn't exist" as success, not an
	// error - it's called unconditionally before every upload under
	// OverwriteDeleteFirst.
	Delete(ctx context.Context, remoteName string) error
	Upload(ctx context.Context, localPath, remoteName string, progress func(sent, total int64)) error
	// Verify compares the just-uploaded local file against its remote
	// copy. method describes what was actually performed (e.g. "size",
	// "size+hash") so callers can surface the weaker guarantee.
	Verify(ctx context.Context, localPath, remoteName string) (method string, err error)
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
		runDryRun(ctx, up, ep, files, events)
		return
	}

	(&endpointWorker{ctx: ctx, ep: ep, opts: opts, events: events, up: up}).run(files)
}

// endpointWorker uploads a batch of files to one endpoint over a single
// reused connection, reconnecting and retrying per Attempts/RetryDelay.
// It owns the connection's lifecycle so the retry loop doesn't have to.
type endpointWorker struct {
	ctx    context.Context
	ep     Endpoint
	opts   Options
	events chan<- UploadEvent
	up     uploader

	connected bool
	// consecutiveConnectFailures counts connect failures since the last
	// success, persisting across files (unlike the per-file attempt loop)
	// so a dead server doesn't get a fresh Attempts budget per file.
	consecutiveConnectFailures int
	// permanentErr, once set, means a transfer hit an unrecoverable error
	// (isPermanentErr) - the rest of the batch is abandoned without
	// retrying, since the same error would just recur on every file.
	permanentErr error
}

// run uploads every file in order, tallies the outcome, and emits the
// terminating EndpointDoneEvent.
func (w *endpointWorker) run(files []string) {
	defer w.disconnect()

	start := time.Now()
	succeeded, failed := 0, 0
	for i, file := range files {
		if w.ctx.Err() != nil {
			failed += len(files) - i
			break
		}
		if w.circuitOpen() {
			failed += w.skipRemaining(files[i:])
			break
		}
		if w.uploadFile(file) {
			succeeded++
		} else {
			failed++
		}
		if w.permanentErr != nil {
			if rest := files[i+1:]; len(rest) > 0 {
				failed += w.skipGivenUp(rest)
			}
			break
		}
	}

	w.events <- EndpointDoneEvent{
		Endpoint: w.ep.Name, Succeeded: succeeded, Failed: failed, Elapsed: Duration(time.Since(start)),
	}
}

// circuitOpen reports whether consecutiveConnectFailures has reached
// MaxConsecutiveConnectFailures, meaning the endpoint should be treated
// as unreachable for the rest of the batch.
func (w *endpointWorker) circuitOpen() bool {
	return w.consecutiveConnectFailures >= w.ep.MaxConsecutiveConnectFailures
}

// skipRemaining marks every file in files as failed without connecting,
// reporting them in one EndpointUnreachableEvent rather than a
// FileErrorEvent per file. Returns len(files) for the caller's tally.
func (w *endpointWorker) skipRemaining(files []string) int {
	w.events <- EndpointUnreachableEvent{
		Endpoint:            w.ep.Name,
		ConsecutiveFailures: w.consecutiveConnectFailures,
		SkippedFiles:        files,
	}
	return len(files)
}

// skipGivenUp marks every file in files as failed without attempting
// them, after permanentErr made further attempts on this endpoint
// pointless. Returns len(files) for the caller's tally.
func (w *endpointWorker) skipGivenUp(files []string) int {
	w.events <- EndpointGivenUpEvent{
		Endpoint:     w.ep.Name,
		Reason:       w.permanentErr.Error(),
		SkippedFiles: files,
	}
	return len(files)
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
				if w.circuitOpen() {
					w.reportGivingUp(file, attempt)
					return false
				}
				w.sleepBeforeRetry(attempt)
				continue
			}
		}

		w.events <- FileStartEvent{Endpoint: w.ep.Name, File: file, Attempt: attempt}
		start := time.Now()
		method, err := w.transfer(file, remoteName)
		if err != nil {
			w.events <- FileErrorEvent{
				Endpoint: w.ep.Name, File: file, Attempt: attempt, Reason: err.Error(), Err: err,
			}
			w.disconnect()
			if isPermanentErr(err) {
				w.permanentErr = err
				return false
			}
			w.sleepBeforeRetry(attempt)
			continue
		}

		w.events <- FileSuccessEvent{
			Endpoint: w.ep.Name, File: file, VerifyMethod: method, Elapsed: Duration(time.Since(start)),
		}
		return true
	}
	return false
}

// isPermanentErr reports whether a transfer error is unrecoverable -
// retrying the identical delete/upload/verify call can't succeed, so
// there's no point burning the rest of the Attempts budget or the rest
// of the batch on this endpoint. In practice this only ever matches
// SFTP: the pkg/sftp client normalizes an SSH_FX_PERMISSION_DENIED
// response into os.ErrPermission, while jlaffaye/ftp surfaces raw
// textproto reply codes that never satisfy this check - so FTP/FTPS
// retry behavior is unchanged.
func isPermanentErr(err error) bool {
	return errors.Is(err, os.ErrPermission)
}

type connectResult int

const (
	connectOK connectResult = iota
	connectFailed
	connectCanceled
)

// connect establishes the reused connection, bounding the whole
// connect+login sequence with ConnectTimeout. A failure caused only by
// ctx cancellation reports connectCanceled and emits no event.
func (w *endpointWorker) connect(file string, attempt int) connectResult {
	connectCtx, cancel := context.WithTimeout(w.ctx, w.ep.ConnectTimeout)
	err := w.up.Connect(connectCtx, w.ep)
	cancel()
	if err != nil {
		if w.ctx.Err() != nil {
			return connectCanceled
		}
		w.consecutiveConnectFailures++
		w.events <- FileErrorEvent{
			Endpoint: w.ep.Name, File: file, Attempt: attempt,
			Reason: "connect: " + err.Error(), Err: err,
		}
		return connectFailed
	}
	w.consecutiveConnectFailures = 0
	w.connected = true
	return connectOK
}

// reportGivingUp explains why a file's attempt loop stopped short of its
// Attempts budget: the circuit breaker tripped mid-file (possible when
// MaxConsecutiveConnectFailures < Attempts).
func (w *endpointWorker) reportGivingUp(file string, attempt int) {
	err := fmt.Errorf(
		"endpoint unreachable: %d consecutive connect failures, giving up on this file",
		w.consecutiveConnectFailures,
	)
	w.events <- FileErrorEvent{
		Endpoint: w.ep.Name, File: file, Attempt: attempt, Reason: err.Error(), Err: err,
	}
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

// runDryRun performs the --dry-run preflight for one endpoint: connect and
// authenticate to prove the endpoint is reachable, disconnect, and report
// how many files a real run would upload - never touching any file.
func runDryRun(
	ctx context.Context,
	up uploader,
	ep Endpoint,
	files []string,
	events chan<- UploadEvent,
) {
	connectCtx, cancel := context.WithTimeout(ctx, ep.ConnectTimeout)
	err := up.Connect(connectCtx, ep)
	cancel()
	if err != nil {
		events <- DryRunEvent{Endpoint: ep.Name, Err: err}
		return
	}
	_ = up.Disconnect(ctx)
	events <- DryRunEvent{Endpoint: ep.Name, Files: len(files)}
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
