package uploadfun

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeUploader is the in-memory uploader used to test the fan-out/retry
// engine without any network activity.
type fakeUploader struct {
	mu sync.Mutex

	failConnectN int
	connectCalls int

	failUploadN int
	uploadCalls int
	// uploadErr, if set, is returned instead of the default "simulated
	// upload failure" for the calls covered by failUploadN - lets tests
	// inject a specific error (e.g. os.ErrPermission) to exercise
	// error-classification paths.
	uploadErr error

	failDeleteN int
	deleteCalls int

	verifyErr    error
	verifyMethod string

	disconnectCalls int

	listResult []string
	listErr    error
	listCalls  int

	// beforeUpload, if set, runs at the start of each Upload with the
	// 1-based call number - a hook for tests to cancel mid-transfer.
	beforeUpload func(call int)
}

func (f *fakeUploader) Connect(ctx context.Context, ep Endpoint) error {
	// Real transports honor ctx at connect time; mirror that so tests can
	// exercise cancellation during the retry loop's reconnect.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.connectCalls++
	if f.connectCalls <= f.failConnectN {
		return errors.New("simulated connect failure")
	}
	return nil
}

func (f *fakeUploader) Disconnect(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disconnectCalls++
	return nil
}

func (f *fakeUploader) Delete(ctx context.Context, remoteName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleteCalls++
	if f.deleteCalls <= f.failDeleteN {
		return errors.New("simulated delete failure")
	}
	return nil
}

func (f *fakeUploader) Upload(
	ctx context.Context, localPath, remoteName string, progress func(sent, total int64),
) error {
	f.mu.Lock()
	f.uploadCalls++
	calls := f.uploadCalls
	f.mu.Unlock()
	if f.beforeUpload != nil {
		f.beforeUpload(calls)
	}
	progress(50, 100)
	progress(100, 100)
	if calls <= f.failUploadN {
		if f.uploadErr != nil {
			return f.uploadErr
		}
		return errors.New("simulated upload failure")
	}
	return nil
}

func (f *fakeUploader) Verify(ctx context.Context, localPath, remoteName string) (string, error) {
	if f.verifyErr != nil {
		return "", f.verifyErr
	}
	method := f.verifyMethod
	if method == "" {
		method = "size"
	}
	return method, nil
}

func (f *fakeUploader) List(ctx context.Context) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	return f.listResult, f.listErr
}

// withFakeUploader registers f as the transport for every protocol for
// the duration of the calling test, restoring the previous newUploader
// afterwards.
func withFakeUploader(t *testing.T, f *fakeUploader) {
	t.Helper()
	prev := newUploader
	newUploader = func(protocol Protocol) (uploader, error) {
		return f, nil
	}
	t.Cleanup(func() { newUploader = prev })
}

func testEndpoint(name string) Endpoint {
	return Endpoint{
		Name:                          name,
		Protocol:                      ProtocolFTP,
		Host:                          "ftp.example.com",
		Username:                      "u",
		Password:                      "p",
		Overwrite:                     OverwriteDeleteFirst,
		Attempts:                      3,
		RetryDelay:                    time.Millisecond,
		ConnectTimeout:                time.Second,
		StallTimeout:                  time.Second,
		MaxConsecutiveConnectFailures: 3,
	}
}

func collectEvents(ch <-chan UploadEvent) []UploadEvent {
	var out []UploadEvent
	for e := range ch {
		out = append(out, e)
	}
	return out
}

func countByType(events []UploadEvent) map[string]int {
	counts := map[string]int{}
	for _, e := range events {
		switch e.(type) {
		case ProgressEvent:
			counts["progress"]++
		case FileSuccessEvent:
			counts["success"]++
		case FileErrorEvent:
			counts["error"]++
		case EndpointUnreachableEvent:
			counts["unreachable"]++
		case EndpointGivenUpEvent:
			counts["given_up"]++
		case EndpointDoneEvent:
			counts["done"]++
		}
	}
	return counts
}

func TestDispatchSuccess(t *testing.T) {
	f := &fakeUploader{}
	withFakeUploader(t, f)

	events := collectEvents(Upload(
		context.Background(), []string{"a.jpg", "b.jpg"}, []Endpoint{testEndpoint("ep1")}, Options{},
	))

	counts := countByType(events)
	if counts["success"] != 2 {
		t.Errorf("expected 2 success events, got %d (%+v)", counts["success"], counts)
	}
	if counts["error"] != 0 {
		t.Errorf("expected 0 error events, got %d", counts["error"])
	}
	if counts["done"] != 1 {
		t.Errorf("expected 1 done event, got %d", counts["done"])
	}
	if f.connectCalls != 1 {
		t.Errorf("expected connection reused across files (1 connect call), got %d", f.connectCalls)
	}
	if f.disconnectCalls != 1 {
		t.Errorf("expected exactly 1 disconnect at end of batch, got %d", f.disconnectCalls)
	}

	var done EndpointDoneEvent
	for _, e := range events {
		if d, ok := e.(EndpointDoneEvent); ok {
			done = d
		}
	}
	if done.Succeeded != 2 || done.Failed != 0 {
		t.Errorf("expected EndpointDoneEvent{Succeeded:2,Failed:0}, got %+v", done)
	}
}

func TestDispatchRetrySucceedsAfterFailure(t *testing.T) {
	f := &fakeUploader{failUploadN: 1}
	withFakeUploader(t, f)

	events := collectEvents(Upload(
		context.Background(), []string{"a.jpg"}, []Endpoint{testEndpoint("ep1")}, Options{},
	))

	counts := countByType(events)
	if counts["error"] != 1 {
		t.Errorf("expected 1 error event before the retry succeeds, got %d", counts["error"])
	}
	if counts["success"] != 1 {
		t.Errorf("expected 1 success event after retry, got %d", counts["success"])
	}
	if f.connectCalls != 2 {
		t.Errorf("expected reconnect after the failed attempt (2 connect calls), got %d", f.connectCalls)
	}

	var errEvent FileErrorEvent
	for _, e := range events {
		if fe, ok := e.(FileErrorEvent); ok {
			errEvent = fe
		}
	}
	if errEvent.Attempt != 1 {
		t.Errorf("expected failure recorded as attempt 1, got %d", errEvent.Attempt)
	}
}

func TestDispatchExhaustsRetries(t *testing.T) {
	f := &fakeUploader{failUploadN: 999}
	withFakeUploader(t, f)

	ep := testEndpoint("ep1")
	ep.Attempts = 2
	events := collectEvents(Upload(context.Background(), []string{"a.jpg"}, []Endpoint{ep}, Options{}))

	counts := countByType(events)
	if counts["error"] != 2 {
		t.Errorf("expected 2 error events (one per attempt), got %d", counts["error"])
	}
	if counts["success"] != 0 {
		t.Errorf("expected no success events, got %d", counts["success"])
	}

	var done EndpointDoneEvent
	for _, e := range events {
		if d, ok := e.(EndpointDoneEvent); ok {
			done = d
		}
	}
	if done.Succeeded != 0 || done.Failed != 1 {
		t.Errorf("expected EndpointDoneEvent{Succeeded:0,Failed:1}, got %+v", done)
	}
}

func TestDispatchMultipleEndpointsInParallel(t *testing.T) {
	f := &fakeUploader{}
	withFakeUploader(t, f)

	endpoints := []Endpoint{testEndpoint("ep1"), testEndpoint("ep2"), testEndpoint("ep3")}
	events := collectEvents(Upload(context.Background(), []string{"a.jpg"}, endpoints, Options{}))

	doneByEndpoint := map[string]bool{}
	for _, e := range events {
		if d, ok := e.(EndpointDoneEvent); ok {
			doneByEndpoint[d.Endpoint] = true
		}
	}
	for _, ep := range endpoints {
		if !doneByEndpoint[ep.Name] {
			t.Errorf("expected an EndpointDoneEvent for %q", ep.Name)
		}
	}
}

func TestDispatchNoVerifySkipsVerification(t *testing.T) {
	f := &fakeUploader{verifyErr: errors.New("verify would always fail")}
	withFakeUploader(t, f)

	events := collectEvents(Upload(
		context.Background(), []string{"a.jpg"}, []Endpoint{testEndpoint("ep1")}, Options{NoVerify: true},
	))

	counts := countByType(events)
	if counts["success"] != 1 {
		t.Errorf("expected verification to be skipped and upload to succeed, got counts=%+v", counts)
	}
}

func TestDispatchOverwriteModes(t *testing.T) {
	t.Run("delete-first calls Delete before Upload", func(t *testing.T) {
		f := &fakeUploader{}
		withFakeUploader(t, f)
		ep := testEndpoint("ep1")
		ep.Overwrite = OverwriteDeleteFirst
		collectEvents(Upload(context.Background(), []string{"a.jpg"}, []Endpoint{ep}, Options{}))
		if f.deleteCalls != 1 {
			t.Errorf("expected 1 delete call, got %d", f.deleteCalls)
		}
	})

	t.Run("direct never calls Delete", func(t *testing.T) {
		f := &fakeUploader{}
		withFakeUploader(t, f)
		ep := testEndpoint("ep1")
		ep.Overwrite = OverwriteDirect
		collectEvents(Upload(context.Background(), []string{"a.jpg"}, []Endpoint{ep}, Options{}))
		if f.deleteCalls != 0 {
			t.Errorf("expected 0 delete calls, got %d", f.deleteCalls)
		}
	})
}

func TestDispatchUnregisteredProtocol(t *testing.T) {
	// Deliberately don't register a fake transport: exercises the
	// no-transport-for-protocol path via a Protocol value config
	// validation would never let through (LoadConfig rejects anything
	// other than ftp/ftps/sftp), but Upload is a public API a caller
	// could still invoke directly with one.
	ep := testEndpoint("ep1")
	ep.Protocol = Protocol("bogus")
	events := collectEvents(Upload(
		context.Background(), []string{"a.jpg", "b.jpg"}, []Endpoint{ep}, Options{},
	))

	counts := countByType(events)
	if counts["error"] != 2 {
		t.Errorf("expected 1 error event per file, got %d", counts["error"])
	}

	var done EndpointDoneEvent
	for _, e := range events {
		if d, ok := e.(EndpointDoneEvent); ok {
			done = d
		}
	}
	if done.Failed != 2 {
		t.Errorf("expected EndpointDoneEvent{Failed:2}, got %+v", done)
	}
}

func TestDispatchConnectFailureReasonIncludesCause(t *testing.T) {
	f := &fakeUploader{failConnectN: 999}
	withFakeUploader(t, f)

	ep := testEndpoint("ep1")
	ep.Attempts = 1
	events := collectEvents(Upload(context.Background(), []string{"a.jpg"}, []Endpoint{ep}, Options{}))

	var errEvent FileErrorEvent
	for _, e := range events {
		if fe, ok := e.(FileErrorEvent); ok {
			errEvent = fe
		}
	}
	if errEvent.Reason != "connect: simulated connect failure" {
		t.Errorf("expected the connect error's cause in Reason, got %q", errEvent.Reason)
	}
}

func TestDispatchCircuitBreakerSkipsRemainingFilesAfterConnectFailures(t *testing.T) {
	f := &fakeUploader{failConnectN: 999}
	withFakeUploader(t, f)

	ep := testEndpoint("ep1")
	ep.Attempts = 3
	ep.MaxConsecutiveConnectFailures = 3
	files := []string{"a.jpg", "b.jpg", "c.jpg", "d.jpg", "e.jpg"}
	events := collectEvents(Upload(context.Background(), files, []Endpoint{ep}, Options{}))

	if f.connectCalls != ep.MaxConsecutiveConnectFailures {
		t.Errorf("expected the circuit breaker to stop dialing after %d consecutive failures, "+
			"got %d connect calls", ep.MaxConsecutiveConnectFailures, f.connectCalls)
	}

	counts := countByType(events)
	// 3 genuine connect-failure events for the first file, plus 1 more
	// explaining why it gave up before exhausting Attempts... except here
	// MaxConsecutiveConnectFailures == Attempts, so the trip coincides
	// with the file's own last attempt.
	wantFileErrors := ep.MaxConsecutiveConnectFailures + 1
	if counts["error"] != wantFileErrors {
		t.Errorf("expected %d error events, got %d", wantFileErrors, counts["error"])
	}
	// Every other remaining file is bundled into a single event, never
	// dialed - not one FileErrorEvent apiece.
	if counts["unreachable"] != 1 {
		t.Errorf("expected exactly 1 unreachable event (not one per skipped file), got %d",
			counts["unreachable"])
	}
	for _, e := range events {
		if ue, ok := e.(EndpointUnreachableEvent); ok && len(ue.SkippedFiles) != len(files)-1 {
			t.Errorf("expected %d skipped files bundled together, got %v", len(files)-1, ue.SkippedFiles)
		}
	}

	var done EndpointDoneEvent
	for _, e := range events {
		if d, ok := e.(EndpointDoneEvent); ok {
			done = d
		}
	}
	if done.Succeeded != 0 || done.Failed != len(files) {
		t.Errorf("expected EndpointDoneEvent{Succeeded:0,Failed:%d}, got %+v", len(files), done)
	}
}

func TestDispatchCircuitBreakerTripsMidFileWhenLowerThanAttempts(t *testing.T) {
	// Attempts (10) is deliberately much larger than
	// MaxConsecutiveConnectFailures (3): the very first file should stop
	// retrying once the breaker trips, rather than burning its whole
	// 10-attempt budget against a server that already isn't answering.
	f := &fakeUploader{failConnectN: 999}
	withFakeUploader(t, f)

	ep := testEndpoint("ep1")
	ep.Attempts = 10
	ep.MaxConsecutiveConnectFailures = 3
	events := collectEvents(Upload(context.Background(), []string{"a.jpg"}, []Endpoint{ep}, Options{}))

	if f.connectCalls != ep.MaxConsecutiveConnectFailures {
		t.Errorf("expected dialing to stop at %d connect calls (the breaker threshold), got %d",
			ep.MaxConsecutiveConnectFailures, f.connectCalls)
	}

	counts := countByType(events)
	// The 3 real connect-failure events, plus 1 explaining why the file
	// stopped short of its 10-attempt budget.
	wantFileErrors := ep.MaxConsecutiveConnectFailures + 1
	if counts["error"] != wantFileErrors {
		t.Errorf("expected %d error events, got %d", wantFileErrors, counts["error"])
	}
}

// TestDispatchCircuitBreakerTripMidFileIsExplainedAndRestBundled is a
// regression test: the breaker used to trip mid-file with no explanation
// and no bundled EndpointUnreachableEvent for the remaining files.
func TestDispatchCircuitBreakerTripMidFileIsExplainedAndRestBundled(t *testing.T) {
	f := &fakeUploader{failConnectN: 999}
	withFakeUploader(t, f)

	ep := testEndpoint("ep1")
	ep.Attempts = 3
	ep.MaxConsecutiveConnectFailures = 5
	files := []string{"f1.txt", "f2.txt", "f3.txt", "f4.txt"}
	events := collectEvents(Upload(context.Background(), files, []Endpoint{ep}, Options{}))

	// file1 uses its full 3-attempt budget; file2 stops after 2 attempts
	// (3+2=5, tripping the breaker).
	if f.connectCalls != 5 {
		t.Errorf("expected 5 connect calls (3 for file1, 2 for file2), got %d", f.connectCalls)
	}

	var file2GaveUp *FileErrorEvent
	var unreachable *EndpointUnreachableEvent
	for _, e := range events {
		switch ev := e.(type) {
		case FileErrorEvent:
			if ev.File == "f2.txt" && strings.Contains(ev.Reason, "giving up on this file") {
				file2GaveUp = &ev
			}
		case EndpointUnreachableEvent:
			unreachable = &ev
		}
	}

	if file2GaveUp == nil {
		t.Fatal("expected a FileErrorEvent explaining why file2 was abandoned mid-retry")
	}
	if file2GaveUp.Attempt != 2 {
		t.Errorf("expected the give-up message to reference file2's actual last attempt (2), got %d",
			file2GaveUp.Attempt)
	}

	if unreachable == nil {
		t.Fatal("expected a single EndpointUnreachableEvent bundling the never-attempted files")
	}
	if len(unreachable.SkippedFiles) != 2 ||
		unreachable.SkippedFiles[0] != "f3.txt" || unreachable.SkippedFiles[1] != "f4.txt" {
		t.Errorf("expected f3.txt and f4.txt bundled together, got %v", unreachable.SkippedFiles)
	}

	counts := countByType(events)
	if counts["unreachable"] != 1 {
		t.Errorf("expected exactly 1 unreachable event, not one per skipped file, got %d",
			counts["unreachable"])
	}
}

func TestDispatchPermanentErrorAbandonsBatchWithoutRetry(t *testing.T) {
	f := &fakeUploader{failUploadN: 999, uploadErr: os.ErrPermission}
	withFakeUploader(t, f)

	ep := testEndpoint("ep1")
	ep.Attempts = 5
	files := []string{"a.jpg", "b.jpg", "c.jpg"}
	events := collectEvents(Upload(context.Background(), files, []Endpoint{ep}, Options{}))

	if f.uploadCalls != 1 {
		t.Errorf("expected exactly 1 upload call (no retry on a permanent error), got %d", f.uploadCalls)
	}

	counts := countByType(events)
	if counts["error"] != 1 {
		t.Errorf("expected exactly 1 error event (no retries), got %d", counts["error"])
	}
	if counts["given_up"] != 1 {
		t.Errorf("expected exactly 1 given_up event bundling the rest of the batch, got %d",
			counts["given_up"])
	}

	var givenUp *EndpointGivenUpEvent
	for _, e := range events {
		if ge, ok := e.(EndpointGivenUpEvent); ok {
			givenUp = &ge
		}
	}
	if givenUp == nil {
		t.Fatal("expected an EndpointGivenUpEvent")
	}
	if len(givenUp.SkippedFiles) != 2 ||
		givenUp.SkippedFiles[0] != "b.jpg" || givenUp.SkippedFiles[1] != "c.jpg" {
		t.Errorf("expected b.jpg and c.jpg bundled together, got %v", givenUp.SkippedFiles)
	}

	var done EndpointDoneEvent
	for _, e := range events {
		if d, ok := e.(EndpointDoneEvent); ok {
			done = d
		}
	}
	if done.Succeeded != 0 || done.Failed != len(files) {
		t.Errorf("expected EndpointDoneEvent{Succeeded:0,Failed:%d}, got %+v", len(files), done)
	}
}

func TestDispatchNonPermanentErrorStillRetriesNormally(t *testing.T) {
	// A plain error (as FTP/FTPS transports produce, never os.ErrPermission)
	// must keep retrying instead of tripping the new fail-fast path.
	f := &fakeUploader{failUploadN: 1}
	withFakeUploader(t, f)

	ep := testEndpoint("ep1")
	ep.Attempts = 3
	events := collectEvents(Upload(context.Background(), []string{"a.jpg"}, []Endpoint{ep}, Options{}))

	counts := countByType(events)
	if counts["given_up"] != 0 {
		t.Errorf("expected no given_up event for a non-permanent error, got %d", counts["given_up"])
	}
	if counts["success"] != 1 {
		t.Errorf("expected the retry to succeed, got counts=%+v", counts)
	}
}

func TestDispatchCircuitBreakerResetsAfterSuccessfulConnect(t *testing.T) {
	// Only the very first connect call fails; every later one (including
	// the reconnects for files b and c) succeeds, so the streak should
	// reset to zero and every file after the first should be attempted
	// normally rather than skipped.
	f := &fakeUploader{failConnectN: 1, failUploadN: 1}
	withFakeUploader(t, f)

	ep := testEndpoint("ep1")
	ep.Attempts = 3
	files := []string{"a.jpg", "b.jpg", "c.jpg"}
	events := collectEvents(Upload(context.Background(), files, []Endpoint{ep}, Options{}))

	counts := countByType(events)
	if counts["success"] != len(files) {
		t.Errorf("expected all %d files to eventually succeed, got %d (%+v)",
			len(files), counts["success"], events)
	}
	for _, e := range events {
		if fe, ok := e.(FileErrorEvent); ok && strings.Contains(fe.Reason, "endpoint unreachable") {
			t.Errorf("did not expect the circuit breaker to trip, got %+v", fe)
		}
	}
}

func TestDispatchContextCanceledBeforeStart(t *testing.T) {
	f := &fakeUploader{}
	withFakeUploader(t, f)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	events := collectEvents(Upload(
		ctx, []string{"a.jpg", "b.jpg"}, []Endpoint{testEndpoint("ep1")}, Options{},
	))

	var done EndpointDoneEvent
	for _, e := range events {
		if d, ok := e.(EndpointDoneEvent); ok {
			done = d
		}
	}
	if done.Succeeded != 0 || done.Failed != 2 {
		t.Errorf("expected every file counted as failed once canceled, got %+v", done)
	}
}

func TestDispatchCancelDuringRetryEmitsNoSpuriousErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	// Every upload fails, so each attempt disconnects and reconnects -
	// the retry path. Cancel while the first attempt is mid-upload.
	f := &fakeUploader{failUploadN: 1000}
	f.beforeUpload = func(call int) {
		if call == 1 {
			cancel()
		}
	}
	withFakeUploader(t, f)

	ep := testEndpoint("ep1")
	ep.Attempts = 5
	events := collectEvents(Upload(ctx, []string{"a.jpg"}, []Endpoint{ep}, Options{NoVerify: true}))

	counts := countByType(events)
	// Only the first attempt's genuine upload failure should be reported;
	// the remaining attempts must not run once canceled.
	if counts["error"] > 1 {
		t.Errorf("expected at most one error event after cancellation, got %d", counts["error"])
	}
	for _, e := range events {
		if fe, ok := e.(FileErrorEvent); ok && strings.HasPrefix(fe.Reason, "connect: ") {
			t.Errorf("unexpected spurious connect error after cancellation: %+v", fe)
		}
	}

	var done EndpointDoneEvent
	for _, e := range events {
		if d, ok := e.(EndpointDoneEvent); ok {
			done = d
		}
	}
	if done.Succeeded != 0 || done.Failed != 1 {
		t.Errorf("expected the canceled file counted as failed, got %+v", done)
	}
}

func TestDispatchDryRunSuccess(t *testing.T) {
	f := &fakeUploader{listResult: []string{"existing1.jpg", "existing2.jpg"}}
	withFakeUploader(t, f)

	events := collectEvents(Upload(
		context.Background(), []string{"a.jpg"}, []Endpoint{testEndpoint("ep1")}, Options{DryRun: true},
	))

	if len(events) != 1 {
		t.Fatalf("expected exactly 1 event for a dry run, got %d: %+v", len(events), events)
	}
	dr, ok := events[0].(DryRunEvent)
	if !ok {
		t.Fatalf("expected a DryRunEvent, got %T", events[0])
	}
	if dr.Err != nil {
		t.Errorf("expected no error, got %v", dr.Err)
	}
	if len(dr.Entries) != 2 {
		t.Errorf("expected 2 entries, got %v", dr.Entries)
	}

	if f.uploadCalls != 0 || f.deleteCalls != 0 {
		t.Errorf("expected no upload/delete calls during a dry run, got uploadCalls=%d deleteCalls=%d",
			f.uploadCalls, f.deleteCalls)
	}
	if f.listCalls != 1 {
		t.Errorf("expected exactly 1 List call, got %d", f.listCalls)
	}
	if f.disconnectCalls != 1 {
		t.Errorf("expected exactly 1 disconnect, got %d", f.disconnectCalls)
	}
}

func TestDispatchDryRunConnectFailure(t *testing.T) {
	f := &fakeUploader{failConnectN: 999}
	withFakeUploader(t, f)

	events := collectEvents(Upload(
		context.Background(), []string{"a.jpg"}, []Endpoint{testEndpoint("ep1")}, Options{DryRun: true},
	))

	if len(events) != 1 {
		t.Fatalf("expected exactly 1 event, got %d: %+v", len(events), events)
	}
	dr, ok := events[0].(DryRunEvent)
	if !ok {
		t.Fatalf("expected a DryRunEvent, got %T", events[0])
	}
	if dr.Err == nil {
		t.Error("expected a connect error")
	}
	if f.listCalls != 0 {
		t.Errorf("expected List not to be called after a connect failure, got %d calls", f.listCalls)
	}
}

func TestDispatchDryRunListFailure(t *testing.T) {
	f := &fakeUploader{listErr: errors.New("list failed")}
	withFakeUploader(t, f)

	events := collectEvents(Upload(
		context.Background(), []string{"a.jpg"}, []Endpoint{testEndpoint("ep1")}, Options{DryRun: true},
	))

	dr, ok := events[0].(DryRunEvent)
	if !ok {
		t.Fatalf("expected a DryRunEvent, got %T", events[0])
	}
	if dr.Err == nil {
		t.Error("expected a list error")
	}
	if f.disconnectCalls != 1 {
		t.Errorf("expected disconnect even after a list failure, got %d", f.disconnectCalls)
	}
}
