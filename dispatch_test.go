package uploadfun

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeUploader is the in-memory Uploader used to test the fan-out/retry
// engine without any network activity.
type fakeUploader struct {
	mu sync.Mutex

	failConnectN int
	connectCalls int

	failUploadN int
	uploadCalls int

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
	newUploader = func(protocol Protocol) (Uploader, error) {
		return f, nil
	}
	t.Cleanup(func() { newUploader = prev })
}

func testEndpoint(name string) Endpoint {
	return Endpoint{
		Name:           name,
		Protocol:       ProtocolFTP,
		Host:           "ftp.example.com",
		Username:       "u",
		Password:       "p",
		Overwrite:      OverwriteDeleteFirst,
		Attempts:       3,
		RetryDelay:     time.Millisecond,
		ConnectTimeout: time.Second,
		StallTimeout:   time.Second,
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
		if fe, ok := e.(FileErrorEvent); ok && fe.Reason == "connect" {
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
