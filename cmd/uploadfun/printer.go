package main

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/alexeyu/uploadfun"
)

// progressPercentStep is the finest resolution verbose progress lines are
// printed at; progressMinInterval additionally debounces by wall-clock
// time so a small/fast file - which can cross several 10% steps within
// milliseconds - doesn't still print one line per step.
const (
	progressPercentStep = 10
	progressMinInterval = time.Second
)

// progressState is a printer's per-(endpoint,file) memory of the last
// verbose progress line it printed, used to throttle ProgressEvent output.
type progressState struct {
	lastPercent int
	lastPrinted time.Time
}

type outputMode int

const (
	modeDefault outputMode = iota
	modeQuiet
	modeVerbose
)

func outputModeFor(quiet, verbose bool) outputMode {
	switch {
	case quiet:
		return modeQuiet
	case verbose:
		return modeVerbose
	default:
		return modeDefault
	}
}

type printer struct {
	stdout, stderr io.Writer
	mode           outputMode
	json           bool
	progress       map[string]*progressState
	// now is a seam for tests to fake wall-clock time; real callers get
	// time.Now.
	now func() time.Time
}

func newPrinter(stdout, stderr io.Writer, mode outputMode, jsonOutput bool) *printer {
	return &printer{
		stdout: stdout, stderr: stderr, mode: mode, json: jsonOutput,
		progress: make(map[string]*progressState), now: time.Now,
	}
}

func processEvents(events <-chan uploadfun.UploadEvent, p *printer) (failed bool) {
	for ev := range events {
		p.handle(ev)
		switch e := ev.(type) {
		case uploadfun.EndpointDoneEvent:
			if e.Failed > 0 {
				failed = true
			}
		case uploadfun.DryRunEvent:
			if e.Err != nil {
				failed = true
			}
		}
	}
	return failed
}

func (p *printer) handle(ev uploadfun.UploadEvent) {
	switch e := ev.(type) {
	case uploadfun.FileStartEvent:
		if p.mode == modeVerbose {
			p.progress[progressKey(e.Endpoint, e.File)] = &progressState{lastPercent: -1}
			msg := fmt.Sprintf("[%s] %s: starting upload", e.Endpoint, e.File)
			p.write(p.stdout, e, msg)
		}
	case uploadfun.ProgressEvent:
		if p.mode == modeVerbose {
			p.handleProgress(e)
		}
	case uploadfun.FileSuccessEvent:
		delete(p.progress, progressKey(e.Endpoint, e.File))
		msg := fmt.Sprintf("[%s] %s: uploaded", e.Endpoint, e.File)
		if e.VerifyMethod != "" {
			msg += fmt.Sprintf(" (verified: %s)", e.VerifyMethod)
		}
		msg += fmt.Sprintf(" in %s", formatSeconds(e.Elapsed))
		p.writeUnlessQuiet(e, msg)
	case uploadfun.FileErrorEvent:
		msg := fmt.Sprintf("[%s] %s: attempt %d failed: %s", e.Endpoint, e.File, e.Attempt, e.Reason)
		p.write(p.stderr, e, msg)
	case uploadfun.EndpointUnreachableEvent:
		msg := fmt.Sprintf(
			"[%s] endpoint unreachable after %d consecutive connect failures: skipping %d remaining file(s)",
			e.Endpoint, e.ConsecutiveFailures, len(e.SkippedFiles),
		)
		p.write(p.stderr, e, msg)
	case uploadfun.EndpointGivenUpEvent:
		msg := fmt.Sprintf(
			"[%s] giving up after unrecoverable error (%s): skipping %d remaining file(s)",
			e.Endpoint, e.Reason, len(e.SkippedFiles),
		)
		p.write(p.stderr, e, msg)
	case uploadfun.EndpointDoneEvent:
		msg := fmt.Sprintf("[%s] done: %d succeeded, %d failed in %s",
			e.Endpoint, e.Succeeded, e.Failed, formatSeconds(e.Elapsed))
		p.writeUnlessQuiet(e, msg)
	case uploadfun.DryRunEvent:
		if e.Err != nil {
			p.write(p.stderr, e, fmt.Sprintf("[%s] dry-run failed: %s", e.Endpoint, e.Err))
			return
		}
		msg := fmt.Sprintf(
			"[%s] dry-run ok: reachable and writable, would upload %d files",
			e.Endpoint,
			e.Files,
		)
		p.writeUnlessQuiet(e, msg)
	}
}

// formatSeconds renders an event's timing as seconds with two decimals -
// friendlier than raw milliseconds for a per-file or per-endpoint time.
func formatSeconds(d uploadfun.Duration) string {
	return fmt.Sprintf("%.2fs", time.Duration(d).Seconds())
}

// progressKey identifies one (endpoint,file) pair's progress-throttle state.
// A literal NUL can't appear in either field, so it's a safe separator.
func progressKey(endpoint, file string) string {
	return endpoint + "\x00" + file
}

// handleProgress prints a verbose progress line at most every
// progressPercentStep points and progressMinInterval, so a large/slow
// transfer gets periodic updates without a small/fast one flooding the
// output with a line per internal read-buffer chunk.
func (p *printer) handleProgress(e uploadfun.ProgressEvent) {
	key := progressKey(e.Endpoint, e.File)
	st, ok := p.progress[key]
	if !ok {
		st = &progressState{lastPercent: -1}
		p.progress[key] = st
	}

	percent := 100
	if e.TotalBytes > 0 {
		percent = int(e.BytesSent * 100 / e.TotalBytes)
	}
	bucket := percent - percent%progressPercentStep
	final := e.BytesSent >= e.TotalBytes

	// Skip the earliest 0-9% sliver: FileStartEvent already announced the
	// upload beginning, so the first useful update is the first full step.
	if bucket == 0 && !final {
		return
	}
	if bucket == st.lastPercent {
		return
	}
	now := p.now()
	if !st.lastPrinted.IsZero() && now.Sub(st.lastPrinted) < progressMinInterval && !final {
		return
	}

	st.lastPercent = bucket
	st.lastPrinted = now
	msg := fmt.Sprintf(
		"[%s] %s: %d%% (%d/%d bytes)", e.Endpoint, e.File, percent, e.BytesSent, e.TotalBytes)
	p.write(p.stdout, e, msg)
}

func (p *printer) writeUnlessQuiet(ev uploadfun.UploadEvent, msg string) {
	if p.mode != modeQuiet {
		p.write(p.stdout, ev, msg)
	}
}

func (p *printer) write(w io.Writer, ev uploadfun.UploadEvent, text string) {
	if p.json {
		_ = json.NewEncoder(w).Encode(jsonPayload(ev))
		return
	}
	_, _ = fmt.Fprintln(w, text)
}

// eventTypeName returns the "type" discriminator jsonPayload injects,
// so JSON consumers can tell events apart without relying on which
// fields happen to be present.
func eventTypeName(ev uploadfun.UploadEvent) string {
	switch ev.(type) {
	case uploadfun.FileStartEvent:
		return "file_start"
	case uploadfun.ProgressEvent:
		return "progress"
	case uploadfun.FileSuccessEvent:
		return "file_success"
	case uploadfun.FileErrorEvent:
		return "file_error"
	case uploadfun.EndpointUnreachableEvent:
		return "endpoint_unreachable"
	case uploadfun.EndpointGivenUpEvent:
		return "endpoint_given_up"
	case uploadfun.EndpointDoneEvent:
		return "endpoint_done"
	case uploadfun.DryRunEvent:
		return "dry_run"
	default:
		return ""
	}
}

func jsonPayload(ev uploadfun.UploadEvent) any {
	typ := eventTypeName(ev)
	if typ == "" {
		return nil
	}
	b, err := json.Marshal(ev)
	if err != nil {
		return nil
	}
	var payload map[string]any
	if err := json.Unmarshal(b, &payload); err != nil {
		return nil
	}
	payload["type"] = typ
	if e, ok := ev.(uploadfun.DryRunEvent); ok && e.Err != nil {
		payload["error"] = e.Err.Error()
	}
	return payload
}
