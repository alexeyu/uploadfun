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
	Endpoint   string `json:"endpoint"`
	File       string `json:"file"`
	BytesSent  int64  `json:"bytesSent"`
	TotalBytes int64  `json:"totalBytes"`
}

func (ProgressEvent) uploadEvent() {}

// FileSuccessEvent reports that a file was uploaded (and, unless
// NoVerify, verified) successfully on one endpoint.
type FileSuccessEvent struct {
	Endpoint string `json:"endpoint"`
	File     string `json:"file"`
	// VerifyMethod describes what verification was performed ("size",
	// "size+hash"), or "" if verification was disabled (NoVerify). Lets
	// a caller surface the weaker size-only guarantee distinctly rather
	// than silently.
	VerifyMethod string `json:"verifyMethod,omitempty"`
}

func (FileSuccessEvent) uploadEvent() {}

// FileErrorEvent reports a single failed attempt (upload or verification)
// for a file on one endpoint. Attempt is 1-based; further attempts follow
// up to the endpoint's Attempts budget before the file is given up on.
type FileErrorEvent struct {
	Endpoint string `json:"endpoint"`
	File     string `json:"file"`
	Attempt  int    `json:"attempt"`
	Reason   string `json:"reason"`
	// Err is the underlying error; excluded from JSON output since the
	// error interface carries no exported fields worth serializing (its
	// message is already captured in Reason).
	Err error `json:"-"`
}

func (FileErrorEvent) uploadEvent() {}

// EndpointDoneEvent reports that one endpoint's worker has finished
// (uploaded or given up on) every file and disconnected.
type EndpointDoneEvent struct {
	Endpoint  string `json:"endpoint"`
	Succeeded int    `json:"succeeded"`
	Failed    int    `json:"failed"`
}

func (EndpointDoneEvent) uploadEvent() {}

// DryRunEvent reports the outcome of a --dry-run connectivity check for
// one endpoint: connect, authenticate, list the remote directory,
// disconnect — no transfer, no delete, no writes. Exactly one is sent
// per endpoint when Options.DryRun is set, replacing the normal
// per-file event sequence entirely.
type DryRunEvent struct {
	Endpoint string   `json:"endpoint"`
	Entries  []string `json:"entries,omitempty"`
	// Err is set if connecting, authenticating, or listing failed; nil
	// means Entries reflects a successful listing.
	Err error `json:"-"`
}

func (DryRunEvent) uploadEvent() {}

// Upload fans out files to endpoints: one goroutine per endpoint, each
// uploading files sequentially over a single reused connection, retrying
// per Endpoint.Attempts/RetryDelay. Every worker's events land on the
// returned channel, which is closed once every endpoint worker is done or
// ctx is canceled.
func Upload(
	ctx context.Context, files []string, endpoints []Endpoint, opts Options,
) <-chan UploadEvent {
	events := make(chan UploadEvent)
	go func() {
		defer close(events)
		dispatch(ctx, files, endpoints, opts, events)
	}()
	return events
}
