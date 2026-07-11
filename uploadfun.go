// Package uploadfun fans out local files to multiple FTP/FTPS/SFTP
// endpoints concurrently, with retry and an event stream, for unattended
// automation rather than interactive use.
package uploadfun

import (
	"context"
	"time"
)

// Defaults applied by LoadConfig to any endpoint (or the config file as a
// whole) that doesn't set its own value.
const (
	DefaultAttempts       = 3
	DefaultRetryDelay     = 2 * time.Second
	DefaultConnectTimeout = 30 * time.Second
	DefaultStallTimeout   = 5 * time.Minute
)

// Protocol identifies which transport an Endpoint uses.
type Protocol string

const (
	ProtocolFTP  Protocol = "ftp"
	ProtocolFTPS Protocol = "ftps"
	ProtocolSFTP Protocol = "sftp"
)

// OverwriteMode controls how an existing remote file with the same name is
// handled before upload.
type OverwriteMode string

const (
	// OverwriteDeleteFirst deletes any existing remote file before
	// uploading. It is the default: it matches the prior implementation's
	// proven behavior and avoids servers that reject a PUT over an
	// existing file.
	OverwriteDeleteFirst OverwriteMode = "delete-first"
	// OverwriteDirect uploads straight over any existing remote file,
	// avoiding the brief window where the remote file doesn't exist at
	// all between delete and re-upload.
	OverwriteDirect OverwriteMode = "direct"
)

// Endpoint is one remote destination to upload to, fully resolved (global
// config defaults already merged in) by LoadConfig.
type Endpoint struct {
	Name     string
	Protocol Protocol
	Host     string
	Port     int
	Username string
	Password string
	// PrivateKey is a path to an SSH private key file, used by the sftp
	// protocol as an alternative to Password.
	PrivateKey string
	Overwrite  OverwriteMode

	Attempts       int
	RetryDelay     time.Duration
	ConnectTimeout time.Duration
	StallTimeout   time.Duration
}

// Options controls behavior of Upload that isn't per-endpoint config.
type Options struct {
	// NoVerify disables the post-upload size/hash verification that's on
	// by default.
	NoVerify bool
	// DryRun connects, authenticates, and lists the remote directory per
	// endpoint, without transferring, deleting, or writing anything.
	DryRun bool
}

// UploadEvent is the vocabulary of events sent on the channel returned by
// Upload. Consumers type-switch on it to distinguish ProgressEvent,
// FileSuccessEvent, FileErrorEvent, and EndpointDoneEvent.
type UploadEvent interface {
	uploadEvent()
}

// ProgressEvent reports byte-level upload progress for one file on one
// endpoint. Only emitted in verbose mode's underlying event stream.
type ProgressEvent struct {
	Endpoint   string
	File       string
	BytesSent  int64
	TotalBytes int64
}

func (ProgressEvent) uploadEvent() {}

// FileSuccessEvent reports that a file was uploaded (and, unless
// NoVerify, verified) successfully on one endpoint.
type FileSuccessEvent struct {
	Endpoint string
	File     string
	// VerifyMethod describes what verification was performed ("size",
	// "size+hash"), or "" if verification was disabled (NoVerify). Lets
	// a caller surface the weaker size-only guarantee distinctly rather
	// than silently.
	VerifyMethod string
}

func (FileSuccessEvent) uploadEvent() {}

// FileErrorEvent reports a single failed attempt (upload or verification)
// for a file on one endpoint. Attempt is 1-based; further attempts follow
// up to the endpoint's Attempts budget before the file is given up on.
type FileErrorEvent struct {
	Endpoint string
	File     string
	Attempt  int
	Reason   string
	Err      error
}

func (FileErrorEvent) uploadEvent() {}

// EndpointDoneEvent reports that one endpoint's worker has finished
// (uploaded or given up on) every file and disconnected.
type EndpointDoneEvent struct {
	Endpoint  string
	Succeeded int
	Failed    int
}

func (EndpointDoneEvent) uploadEvent() {}

// Upload fans out files to endpoints: one goroutine per endpoint, each
// uploading files sequentially over a single reused connection, retrying
// per Endpoint.Attempts/RetryDelay. Every worker's events land on the
// returned channel, which is closed once every endpoint worker is done or
// ctx is canceled.
func Upload(ctx context.Context, files []string, endpoints []Endpoint, opts Options) <-chan UploadEvent {
	events := make(chan UploadEvent)
	go func() {
		defer close(events)
		dispatch(ctx, files, endpoints, opts, events)
	}()
	return events
}
