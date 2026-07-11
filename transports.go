package uploadfun

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/alexeyu/uploadfun/internal/transport"
)

func init() {
	newUploader = newRealUploader
}

// remoteClient is the common surface every internal/transport client
// (FTPClient, SFTPClient) exposes; transportUploader adapts it to the
// Uploader interface without either side depending on the other's
// package.
type remoteClient interface {
	Close() error
	Delete(remoteName string) error
	Upload(remoteName string, r io.Reader, size int64, progress func(sent, total int64)) error
	Verify(localPath, remoteName string) (method string, err error)
	List() ([]string, error)
}

// transportUploader adapts one internal/transport client to the Uploader
// interface dispatch.go drives. None of the underlying transport clients
// support per-operation cancellation once connected (see
// ARCHITECTURE.md "Internal Uploader interface") — ctx is only honored
// at Connect time; an in-flight upload/delete/verify/list runs to
// completion (or its own network-level timeout) even if ctx is canceled
// mid-call. Cancellation still takes effect promptly between attempts
// and between files.
type transportUploader struct {
	dial   func(ctx context.Context, ep Endpoint) (remoteClient, error)
	client remoteClient
}

func (u *transportUploader) Connect(ctx context.Context, ep Endpoint) error {
	client, err := u.dial(ctx, ep)
	if err != nil {
		return err
	}
	u.client = client
	return nil
}

func (u *transportUploader) Disconnect(ctx context.Context) error {
	return u.client.Close()
}

func (u *transportUploader) Delete(ctx context.Context, remoteName string) error {
	return u.client.Delete(remoteName)
}

func (u *transportUploader) Upload(
	ctx context.Context, localPath, remoteName string, progress func(sent, total int64),
) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	return u.client.Upload(remoteName, f, info.Size(), progress)
}

func (u *transportUploader) Verify(
	ctx context.Context, localPath, remoteName string,
) (string, error) {
	return u.client.Verify(localPath, remoteName)
}

func (u *transportUploader) List(ctx context.Context) ([]string, error) {
	return u.client.List()
}

func newRealUploader(protocol Protocol) (Uploader, error) {
	switch protocol {
	case ProtocolFTP:
		return &transportUploader{dial: dialFTPClient}, nil
	case ProtocolFTPS:
		return &transportUploader{dial: dialFTPSClient}, nil
	case ProtocolSFTP:
		return &transportUploader{dial: dialSFTPClient}, nil
	default:
		return nil, fmt.Errorf("no transport registered for protocol %q", protocol)
	}
}

func dialFTPClient(ctx context.Context, ep Endpoint) (remoteClient, error) {
	return transport.DialFTP(ctx, transport.FTPDialOptions{
		Host:           ep.Host,
		Port:           ep.Port,
		Username:       ep.Username,
		Password:       ep.Password,
		ConnectTimeout: ep.ConnectTimeout,
		StallTimeout:   ep.StallTimeout,
	})
}

func dialFTPSClient(ctx context.Context, ep Endpoint) (remoteClient, error) {
	return transport.DialFTPS(ctx, transport.FTPSDialOptions{
		Host:           ep.Host,
		Port:           ep.Port,
		Username:       ep.Username,
		Password:       ep.Password,
		ConnectTimeout: ep.ConnectTimeout,
		StallTimeout:   ep.StallTimeout,
	})
}

func dialSFTPClient(ctx context.Context, ep Endpoint) (remoteClient, error) {
	return transport.DialSFTP(ctx, transport.SFTPDialOptions{
		Host:           ep.Host,
		Port:           ep.Port,
		Username:       ep.Username,
		Password:       ep.Password,
		PrivateKeyPath: ep.PrivateKey,
		ConnectTimeout: ep.ConnectTimeout,
		StallTimeout:   ep.StallTimeout,
	})
}
