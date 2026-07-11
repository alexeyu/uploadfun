package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/alexeyu/uploadfun"
)

// outputMode controls which events reach stdout; independent of json,
// which only controls how.
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

// processEvents drains events, printing each per the printer's mode, and
// reports whether any endpoint finished with at least one failed file
// (or, for --dry-run, at least one endpoint that failed to connect/
// authenticate/list) — the exit-code-1 condition.
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
		if p.mode != modeQuiet {
			msg := fmt.Sprintf("[%s] %s: uploaded", e.Endpoint, e.File)
			if e.VerifyMethod != "" {
				msg += fmt.Sprintf(" (verified: %s)", e.VerifyMethod)
			}
			p.write(p.stdout, e, msg)
		}
	case uploadfun.FileErrorEvent:
		// Errors always print, even in --quiet — "nothing" in the output
		// modes table means non-error stdout output, not silence on
		// failure.
		msg := fmt.Sprintf("[%s] %s: attempt %d failed: %s", e.Endpoint, e.File, e.Attempt, e.Reason)
		p.write(p.stderr, e, msg)
	case uploadfun.EndpointDoneEvent:
		if p.mode != modeQuiet {
			msg := fmt.Sprintf("[%s] done: %d succeeded, %d failed", e.Endpoint, e.Succeeded, e.Failed)
			p.write(p.stdout, e, msg)
		}
	case uploadfun.DryRunEvent:
		if e.Err != nil {
			// Always print, even in --quiet, same as FileErrorEvent.
			p.write(p.stderr, e, fmt.Sprintf("[%s] dry-run failed: %s", e.Endpoint, e.Err))
			return
		}
		if p.mode != modeQuiet {
			msg := fmt.Sprintf("[%s] dry-run ok: %d entries in remote directory", e.Endpoint, len(e.Entries))
			p.write(p.stdout, e, msg)
		}
	}
}

func (p *printer) write(w io.Writer, ev uploadfun.UploadEvent, text string) {
	if p.json {
		_ = json.NewEncoder(w).Encode(jsonPayload(ev))
		return
	}
	_, _ = fmt.Fprintln(w, text)
}

// jsonPayload adds a "type" discriminator field so newline-delimited
// JSON consumers can tell the four event kinds apart without relying on
// which fields happen to be present.
func jsonPayload(ev uploadfun.UploadEvent) any {
	switch e := ev.(type) {
	case uploadfun.ProgressEvent:
		return struct {
			Type string `json:"type"`
			uploadfun.ProgressEvent
		}{"progress", e}
	case uploadfun.FileSuccessEvent:
		return struct {
			Type string `json:"type"`
			uploadfun.FileSuccessEvent
		}{"file_success", e}
	case uploadfun.FileErrorEvent:
		return struct {
			Type string `json:"type"`
			uploadfun.FileErrorEvent
		}{"file_error", e}
	case uploadfun.EndpointDoneEvent:
		return struct {
			Type string `json:"type"`
			uploadfun.EndpointDoneEvent
		}{"endpoint_done", e}
	case uploadfun.DryRunEvent:
		errText := ""
		if e.Err != nil {
			errText = e.Err.Error()
		}
		return struct {
			Type string `json:"type"`
			uploadfun.DryRunEvent
			Error string `json:"error,omitempty"`
		}{"dry_run", e, errText}
	default:
		return nil
	}
}
