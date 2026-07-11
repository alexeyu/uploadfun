package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"os"
	"strconv"
	"time"

	"github.com/jlaffaye/ftp"
)

const defaultFTPPort = 21

// FTPDialOptions configures a plain-FTP connection.
type FTPDialOptions struct {
	Host           string
	Port           int // 0 means defaultFTPPort
	Username       string
	Password       string
	ConnectTimeout time.Duration
}

// FTPClient wraps a connected FTP (or FTPS, via explicit AUTH TLS)
// session. Not safe for concurrent use, matching the underlying library
// — this mirrors the "one reused connection per endpoint worker" model
// the engine drives it with.
type FTPClient struct {
	conn *ftp.ServerConn
}

// dialConfig is the union of what plain FTP and explicit-TLS FTPS need;
// ftp.go and ftps.go each expose a narrower, protocol-specific dial
// option type and funnel into this shared implementation.
type dialConfig struct {
	Host           string
	Port           int
	Username       string
	Password       string
	ConnectTimeout time.Duration
	explicitTLS    bool
	tlsConfig      *tls.Config
}

// DialFTP connects and authenticates over plain FTP.
func DialFTP(ctx context.Context, opts FTPDialOptions) (*FTPClient, error) {
	return dial(ctx, dialConfig{
		Host:           opts.Host,
		Port:           opts.Port,
		Username:       opts.Username,
		Password:       opts.Password,
		ConnectTimeout: opts.ConnectTimeout,
	})
}

func dial(ctx context.Context, cfg dialConfig) (*FTPClient, error) {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(resolvePort(cfg.Port)))

	dialOpts := []ftp.DialOption{
		ftp.DialWithContext(ctx),
		ftp.DialWithTimeout(cfg.ConnectTimeout),
	}
	if cfg.explicitTLS {
		dialOpts = append(dialOpts, ftp.DialWithExplicitTLS(resolveTLSConfig(cfg.Host, cfg.tlsConfig)))
	}

	conn, err := ftp.Dial(addr, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	if err := conn.Login(cfg.Username, cfg.Password); err != nil {
		_ = conn.Quit()
		return nil, fmt.Errorf("login: %w", err)
	}
	return &FTPClient{conn: conn}, nil
}

func resolvePort(port int) int {
	if port == 0 {
		return defaultFTPPort
	}
	return port
}

// resolveTLSConfig fills in ServerName and ClientSessionCache when
// missing, on a clone so a caller-supplied *tls.Config is never mutated.
// Both matter beyond cosmetics: Go's crypto/tls only attempts session
// resumption when ServerName is non-empty (it's part of the session
// cache key) and ClientSessionCache is set. Servers that enforce "the
// data connection must resume the control connection's TLS session" as
// an anti-hijacking measure — pure-ftpd among them — silently accept the
// data connection's TCP handshake and then drop it once the session
// fails to match, which surfaces to callers as a bare io.EOF on
// upload/download with no indication it was a TLS policy rejection.
func resolveTLSConfig(host string, cfg *tls.Config) *tls.Config {
	resolved := &tls.Config{}
	if cfg != nil {
		resolved = cfg.Clone()
	}
	if resolved.ServerName == "" {
		resolved.ServerName = host
	}
	if resolved.ClientSessionCache == nil {
		resolved.ClientSessionCache = tls.NewLRUClientSessionCache(4)
	}
	return resolved
}

// Close ends the session with a QUIT.
func (c *FTPClient) Close() error {
	return c.conn.Quit()
}

// Delete removes remoteName, treating "file doesn't exist" as success
// since it's called unconditionally before every upload under the
// delete-first overwrite mode.
func (c *FTPClient) Delete(remoteName string) error {
	err := c.conn.Delete(remoteName)
	if err == nil || isNotExist(err) {
		return nil
	}
	return err
}

// Upload streams r (size bytes total) to remoteName via STOR, invoking
// progress with cumulative bytes sent as it reads.
func (c *FTPClient) Upload(
	remoteName string, r io.Reader, size int64, progress func(sent, total int64),
) error {
	pr := &progressReader{r: r, total: size, onProgress: progress}
	return c.conn.Stor(remoteName, pr)
}

// Size issues SIZE and returns the remote file's size in bytes.
func (c *FTPClient) Size(remoteName string) (int64, error) {
	return c.conn.FileSize(remoteName)
}

// Verify compares localPath's size against remoteName's size on the
// server. jlaffaye/ftp exposes no public way to query FEAT or issue
// XCRC/XMD5/XSHA1 hash commands (see ARCHITECTURE.md "Verification"), so
// FTP/FTPS verification is size-only for v1; method is always "size" on
// success.
func (c *FTPClient) Verify(localPath, remoteName string) (method string, err error) {
	info, err := os.Stat(localPath)
	if err != nil {
		return "", fmt.Errorf("stat local file: %w", err)
	}
	remoteSize, err := c.Size(remoteName)
	if err != nil {
		return "", fmt.Errorf("remote size: %w", err)
	}
	if remoteSize != info.Size() {
		return "", fmt.Errorf("size mismatch: local %d bytes, remote %d bytes", info.Size(), remoteSize)
	}
	return "size", nil
}

// List returns the names of entries in the current remote directory,
// used only for --dry-run. Excludes "." and ".." — servers that answer
// via MLSD (RFC 3659), pure-ftpd among them, include those pseudo-entries
// and a caller displaying "N entries" shouldn't count them.
func (c *FTPClient) List() ([]string, error) {
	entries, err := c.conn.List("")
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.Name == "." || e.Name == ".." {
			continue
		}
		names = append(names, e.Name)
	}
	return names, nil
}

// isNotExist reports whether err is an FTP "file unavailable" response
// (RFC 959 code 550), the standard code servers use for DELE/RETR/etc.
// against a path that doesn't exist.
func isNotExist(err error) bool {
	var tpErr *textproto.Error
	if errors.As(err, &tpErr) {
		return tpErr.Code == ftp.StatusFileUnavailable
	}
	return false
}

type progressReader struct {
	r          io.Reader
	sent       int64
	total      int64
	onProgress func(sent, total int64)
}

func (p *progressReader) Read(buf []byte) (int, error) {
	n, err := p.r.Read(buf)
	if n > 0 {
		p.sent += int64(n)
		if p.onProgress != nil {
			p.onProgress(p.sent, p.total)
		}
	}
	return n, err
}
