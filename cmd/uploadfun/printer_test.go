package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alexeyu/uploadfun"
)

func syntheticEvents() []uploadfun.UploadEvent {
	return []uploadfun.UploadEvent{
		uploadfun.ProgressEvent{Endpoint: "ep1", File: "a.jpg", BytesSent: 50, TotalBytes: 100},
		uploadfun.FileSuccessEvent{Endpoint: "ep1", File: "a.jpg", VerifyMethod: "size"},
		uploadfun.FileErrorEvent{
			Endpoint: "ep2", File: "b.jpg", Attempt: 1, Reason: "boom", Err: errors.New("boom"),
		},
		uploadfun.EndpointDoneEvent{Endpoint: "ep1", Succeeded: 1, Failed: 0},
		uploadfun.EndpointDoneEvent{Endpoint: "ep2", Succeeded: 0, Failed: 1},
	}
}

func runPrinter(mode outputMode, jsonOutput bool) (stdout, stderr string, failed bool) {
	return runPrinterWithEvents(mode, jsonOutput, syntheticEvents())
}

func runPrinterWithEvents(
	mode outputMode, jsonOutput bool, synthetic []uploadfun.UploadEvent,
) (stdout, stderr string, failed bool) {
	return runPrinterWithClock(mode, jsonOutput, synthetic, time.Now)
}

// runPrinterWithClock is runPrinterWithEvents with an injectable clock, so
// tests can exercise the progress debounce's time gate deterministically.
func runPrinterWithClock(
	mode outputMode, jsonOutput bool, synthetic []uploadfun.UploadEvent, now func() time.Time,
) (stdout, stderr string, failed bool) {
	var outBuf, errBuf bytes.Buffer
	p := newPrinter(&outBuf, &errBuf, mode, jsonOutput)
	p.now = now

	events := make(chan uploadfun.UploadEvent)
	go func() {
		for _, e := range synthetic {
			events <- e
		}
		close(events)
	}()
	failed = processEvents(events, p)
	return outBuf.String(), errBuf.String(), failed
}

func TestProcessEventsDetectsFailure(t *testing.T) {
	_, _, failed := runPrinter(modeDefault, false)
	if !failed {
		t.Error("expected failed=true since ep2 has Failed>0")
	}
}

func TestPrinterQuietModeSuppressesNonErrorStdout(t *testing.T) {
	stdout, stderr, _ := runPrinter(modeQuiet, false)
	if stdout != "" {
		t.Errorf("expected no stdout output in quiet mode, got %q", stdout)
	}
	if !strings.Contains(stderr, "boom") {
		t.Errorf("expected errors still printed in quiet mode, got %q", stderr)
	}
}

func TestPrinterDefaultModeOmitsProgress(t *testing.T) {
	stdout, _, _ := runPrinter(modeDefault, false)
	if strings.Contains(stdout, "bytes") {
		t.Errorf("expected no byte-level progress in default mode, got %q", stdout)
	}
	if !strings.Contains(stdout, "uploaded") {
		t.Errorf("expected file success line in default mode, got %q", stdout)
	}
}

func TestPrinterVerboseModeIncludesProgress(t *testing.T) {
	stdout, _, _ := runPrinter(modeVerbose, false)
	if !strings.Contains(stdout, "50/100 bytes") {
		t.Errorf("expected byte-level progress in verbose mode, got %q", stdout)
	}
}

func TestPrinterVerboseModeShowsFileStart(t *testing.T) {
	events := []uploadfun.UploadEvent{
		uploadfun.FileStartEvent{Endpoint: "ep1", File: "a.jpg", Attempt: 1},
	}
	stdout, _, _ := runPrinterWithEvents(modeVerbose, false, events)
	if !strings.Contains(stdout, "starting upload") {
		t.Errorf("expected a starting-upload line in verbose mode, got %q", stdout)
	}
}

func TestPrinterDefaultModeOmitsFileStart(t *testing.T) {
	events := []uploadfun.UploadEvent{
		uploadfun.FileStartEvent{Endpoint: "ep1", File: "a.jpg", Attempt: 1},
	}
	stdout, _, _ := runPrinterWithEvents(modeDefault, false, events)
	if stdout != "" {
		t.Errorf("expected no output for FileStartEvent in default mode, got %q", stdout)
	}
}

// TestPrinterProgressThrottlesByPercentStep asserts that, with time never
// standing in the way, a progress line only prints on crossing a new 10%
// step - not the 0-9% sliver, and not within a step that was already
// printed.
func TestPrinterProgressThrottlesByPercentStep(t *testing.T) {
	base := time.Unix(0, 0)
	tick := 0
	// Each call advances the clock well past progressMinInterval, so only
	// the percent-step gate is under test here.
	clock := func() time.Time {
		tick++
		return base.Add(time.Duration(tick) * 2 * time.Second)
	}

	percents := []int64{5, 10, 15, 20, 25, 50, 75, 100}
	var events []uploadfun.UploadEvent
	for _, pct := range percents {
		events = append(events, uploadfun.ProgressEvent{
			Endpoint: "ep1", File: "a.jpg", BytesSent: pct, TotalBytes: 100,
		})
	}

	stdout, _, _ := runPrinterWithClock(modeVerbose, false, events, clock)
	lines := strings.Split(strings.TrimSpace(stdout), "\n")

	wantSubstrings := []string{
		"10% (10/100", "20% (20/100", "50% (50/100", "75% (75/100", "100% (100/100",
	}
	if len(lines) != len(wantSubstrings) {
		t.Fatalf("expected %d progress lines, got %d: %q", len(wantSubstrings), len(lines), stdout)
	}
	for i, want := range wantSubstrings {
		if !strings.Contains(lines[i], want) {
			t.Errorf("line %d: expected to contain %q, got %q", i, want, lines[i])
		}
	}
	for _, unwanted := range []string{"5% (5/100", "15% (15/100", "25% (25/100"} {
		if strings.Contains(stdout, unwanted) {
			t.Errorf("expected no line for a within-step percent, but found %q in %q", unwanted, stdout)
		}
	}
}

// TestPrinterProgressDebouncesByTime is a regression test for the reported
// noise: a small/fast file whose reads cross several 10% steps within
// milliseconds must not print one line per step - only time-spaced updates
// (plus the final 100%) do.
func TestPrinterProgressDebouncesByTime(t *testing.T) {
	frozen := time.Unix(0, 0)
	clock := func() time.Time { return frozen }

	events := []uploadfun.UploadEvent{
		uploadfun.ProgressEvent{Endpoint: "ep1", File: "a.jpg", BytesSent: 10, TotalBytes: 100},
		uploadfun.ProgressEvent{Endpoint: "ep1", File: "a.jpg", BytesSent: 20, TotalBytes: 100},
		uploadfun.ProgressEvent{Endpoint: "ep1", File: "a.jpg", BytesSent: 30, TotalBytes: 100},
		uploadfun.ProgressEvent{Endpoint: "ep1", File: "a.jpg", BytesSent: 100, TotalBytes: 100},
	}

	stdout, _, _ := runPrinterWithClock(modeVerbose, false, events, clock)
	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected exactly 2 lines (first step + final), got %d: %q", len(lines), stdout)
	}
	if !strings.Contains(lines[0], "10% (10/100") {
		t.Errorf("expected the first line to be the 10%% step, got %q", lines[0])
	}
	if !strings.Contains(lines[1], "100% (100/100") {
		t.Errorf("expected the second line to be the final 100%%, got %q", lines[1])
	}
}

// TestPrinterProgressResetsOnFileStart ensures a retry's progress starts
// fresh: without the reset, a second attempt that also reaches (say) 10%
// would be suppressed as "already printed" from the first attempt.
func TestPrinterProgressResetsOnFileStart(t *testing.T) {
	base := time.Unix(0, 0)
	tick := 0
	clock := func() time.Time {
		tick++
		return base.Add(time.Duration(tick) * 2 * time.Second)
	}

	events := []uploadfun.UploadEvent{
		uploadfun.FileStartEvent{Endpoint: "ep1", File: "a.jpg", Attempt: 1},
		uploadfun.ProgressEvent{Endpoint: "ep1", File: "a.jpg", BytesSent: 10, TotalBytes: 100},
		uploadfun.FileStartEvent{Endpoint: "ep1", File: "a.jpg", Attempt: 2},
		uploadfun.ProgressEvent{Endpoint: "ep1", File: "a.jpg", BytesSent: 10, TotalBytes: 100},
	}

	stdout, _, _ := runPrinterWithClock(modeVerbose, false, events, clock)
	if got := strings.Count(stdout, "10% (10/100"); got != 2 {
		t.Errorf("expected the 10%% step to print once per attempt (2 total), got %d in %q", got, stdout)
	}
}

func TestPrinterJSONOutput(t *testing.T) {
	stdout, stderr, _ := runPrinter(modeDefault, true)

	lines := strings.Split(strings.TrimSpace(stdout), "\n")
	var sawSuccess, sawDone bool
	for _, line := range lines {
		var payload map[string]any
		if err := json.Unmarshal([]byte(line), &payload); err != nil {
			t.Fatalf("expected valid JSON line, got %q: %v", line, err)
		}
		switch payload["type"] {
		case "file_success":
			sawSuccess = true
			if payload["verifyMethod"] != "size" {
				t.Errorf("expected verifyMethod=size, got %v", payload["verifyMethod"])
			}
		case "endpoint_done":
			sawDone = true
		case "progress":
			t.Error("did not expect progress events in default mode JSON output")
		}
	}
	if !sawSuccess || !sawDone {
		t.Errorf("expected both file_success and endpoint_done events, got stdout=%q", stdout)
	}

	var errPayload map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stderr)), &errPayload); err != nil {
		t.Fatalf("expected valid JSON on stderr, got %q: %v", stderr, err)
	}
	if errPayload["type"] != "file_error" {
		t.Errorf("expected file_error type, got %v", errPayload["type"])
	}
	if _, hasErr := errPayload["Err"]; hasErr {
		t.Error("expected Err field to be excluded from JSON output")
	}
}

func TestPrinterDryRunSuccess(t *testing.T) {
	events := []uploadfun.UploadEvent{
		uploadfun.DryRunEvent{Endpoint: "ep1", Entries: []string{"a.jpg", "b.jpg"}},
	}
	stdout, stderr, failed := runPrinterWithEvents(modeDefault, false, events)
	if failed {
		t.Error("expected a successful dry run not to count as a failure")
	}
	if stderr != "" {
		t.Errorf("expected no stderr output, got %q", stderr)
	}
	if !strings.Contains(stdout, "dry-run ok") || !strings.Contains(stdout, "2 entries") {
		t.Errorf("expected a dry-run summary on stdout, got %q", stdout)
	}
}

func TestPrinterDryRunFailure(t *testing.T) {
	events := []uploadfun.UploadEvent{
		uploadfun.DryRunEvent{Endpoint: "ep1", Err: errors.New("connect refused")},
	}
	stdout, stderr, failed := runPrinterWithEvents(modeDefault, false, events)
	if !failed {
		t.Error("expected a failed dry run to count as a failure")
	}
	if stdout != "" {
		t.Errorf("expected no stdout output on dry-run failure, got %q", stdout)
	}
	if !strings.Contains(stderr, "connect refused") {
		t.Errorf("expected the error on stderr, got %q", stderr)
	}
}

func TestPrinterDryRunFailurePrintsEvenWhenQuiet(t *testing.T) {
	events := []uploadfun.UploadEvent{
		uploadfun.DryRunEvent{Endpoint: "ep1", Err: errors.New("connect refused")},
	}
	_, stderr, failed := runPrinterWithEvents(modeQuiet, false, events)
	if !failed {
		t.Error("expected failure to be detected even in quiet mode")
	}
	if !strings.Contains(stderr, "connect refused") {
		t.Errorf("expected the error on stderr even in quiet mode, got %q", stderr)
	}
}

func TestPrinterDryRunJSON(t *testing.T) {
	events := []uploadfun.UploadEvent{
		uploadfun.DryRunEvent{Endpoint: "ep1", Entries: []string{"a.jpg"}},
	}
	stdout, _, _ := runPrinterWithEvents(modeDefault, true, events)

	var payload map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &payload); err != nil {
		t.Fatalf("expected valid JSON, got %q: %v", stdout, err)
	}
	if payload["type"] != "dry_run" {
		t.Errorf("expected type=dry_run, got %v", payload["type"])
	}
	entries, ok := payload["entries"].([]any)
	if !ok || len(entries) != 1 {
		t.Errorf("expected entries=[a.jpg], got %v", payload["entries"])
	}
}
