package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/alexeyu/uploadfun"
)

func syntheticEvents() []uploadfun.UploadEvent {
	return []uploadfun.UploadEvent{
		uploadfun.ProgressEvent{Endpoint: "ep1", File: "a.jpg", BytesSent: 50, TotalBytes: 100},
		uploadfun.FileSuccessEvent{Endpoint: "ep1", File: "a.jpg", VerifyMethod: "size"},
		uploadfun.FileErrorEvent{Endpoint: "ep2", File: "b.jpg", Attempt: 1, Reason: "boom", Err: errors.New("boom")},
		uploadfun.EndpointDoneEvent{Endpoint: "ep1", Succeeded: 1, Failed: 0},
		uploadfun.EndpointDoneEvent{Endpoint: "ep2", Succeeded: 0, Failed: 1},
	}
}

func runPrinter(mode outputMode, jsonOutput bool) (stdout, stderr string, failed bool) {
	var outBuf, errBuf bytes.Buffer
	p := newPrinter(&outBuf, &errBuf, mode, jsonOutput)

	events := make(chan uploadfun.UploadEvent)
	go func() {
		for _, e := range syntheticEvents() {
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
