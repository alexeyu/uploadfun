package uploadfun

import (
	"context"
	"testing"
)

func TestUploadEventVocabularyImplementsInterface(t *testing.T) {
	var events = []UploadEvent{
		ProgressEvent{Endpoint: "e", File: "f", BytesSent: 1, TotalBytes: 2},
		FileSuccessEvent{Endpoint: "e", File: "f"},
		FileErrorEvent{Endpoint: "e", File: "f", Attempt: 1, Reason: "boom"},
		EndpointDoneEvent{Endpoint: "e", Succeeded: 1, Failed: 0},
	}
	if len(events) != 4 {
		t.Fatalf("expected 4 events, got %d", len(events))
	}
}

func TestUploadClosesChannel(t *testing.T) {
	ch := Upload(context.Background(), nil, nil, Options{})
	for range ch {
		t.Fatal("expected no events when there are no endpoints")
	}
}
