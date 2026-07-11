package uploadfun

import "testing"

func TestNewRealUploaderSelectsProtocol(t *testing.T) {
	for _, protocol := range []Protocol{ProtocolFTP, ProtocolFTPS, ProtocolSFTP} {
		t.Run(string(protocol), func(t *testing.T) {
			up, err := newRealUploader(protocol)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if up == nil {
				t.Fatal("expected a non-nil Uploader")
			}
		})
	}
}

func TestNewRealUploaderUnknownProtocol(t *testing.T) {
	if _, err := newRealUploader(Protocol("bogus")); err == nil {
		t.Error("expected an error for an unregistered protocol")
	}
}
