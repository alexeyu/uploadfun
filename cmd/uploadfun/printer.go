package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/alexeyu/uploadfun"
)

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
}

func newPrinter(stdout, stderr io.Writer, mode outputMode, jsonOutput bool) *printer {
	return &printer{stdout: stdout, stderr: stderr, mode: mode, json: jsonOutput}
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
	case uploadfun.ProgressEvent:
		if p.mode == modeVerbose {
			msg := fmt.Sprintf("[%s] %s: %d/%d bytes", e.Endpoint, e.File, e.BytesSent, e.TotalBytes)
			p.write(p.stdout, e, msg)
		}
	case uploadfun.FileSuccessEvent:
		msg := fmt.Sprintf("[%s] %s: uploaded", e.Endpoint, e.File)
		if e.VerifyMethod != "" {
			msg += fmt.Sprintf(" (verified: %s)", e.VerifyMethod)
		}
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
		msg := fmt.Sprintf("[%s] done: %d succeeded, %d failed", e.Endpoint, e.Succeeded, e.Failed)
		p.writeUnlessQuiet(e, msg)
	case uploadfun.DryRunEvent:
		if e.Err != nil {
			p.write(p.stderr, e, fmt.Sprintf("[%s] dry-run failed: %s", e.Endpoint, e.Err))
			return
		}
		msg := fmt.Sprintf("[%s] dry-run ok: %d entries in remote directory", e.Endpoint, len(e.Entries))
		p.writeUnlessQuiet(e, msg)
	}
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
