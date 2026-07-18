package transport

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/textproto"
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
	StallTimeout   time.Duration // 0 disables idle-stall protection
}

// FTPClient wraps a connected FTP (or FTPS, via explicit AUTH TLS)
// session. Not safe for concurrent use, matching the "one reused
// connection per endpoint worker" model the engine drives it with.
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
	StallTimeout   time.Duration
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
		StallTimeout:   opts.StallTimeout,
	})
}

func dial(ctx context.Context, cfg dialConfig) (*FTPClient, error) {
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(resolvePort(cfg.Port, defaultFTPPort)))

	var tlsConfig *tls.Config
	if cfg.explicitTLS {
		tlsConfig = resolveTLSConfig(cfg.Host, cfg.tlsConfig)
	}

	guard := &stallGuard{stallTimeout: cfg.StallTimeout}
	d := &ftpDialer{ctx: ctx, timeout: cfg.ConnectTimeout, guard: guard, tlsConfig: tlsConfig}

	conn, err := ftpDialAndLogin(addr, cfg, d)
	if err != nil {
		return nil, err
	}

	// Session is up: clear the connect deadline and switch to the idle
	// stall timeout, and detach d from the connect-scoped ctx - later
	// data-connection dials must not inherit it.
	if d.ctrlConn != nil {
		clearDeadline(d.ctrlConn)
	}
	d.ctx = context.Background()
	guard.markEstablished()
	return &FTPClient{conn: conn}, nil
}

// ftpDialer builds the custom net dial func jlaffaye/ftp uses for both the
// control and data connections, capturing the control conn so its
// connect-phase deadline can be cleared once the session is established.
type ftpDialer struct {
	ctx       context.Context
	timeout   time.Duration
	guard     *stallGuard
	tlsConfig *tls.Config
	ctrlConn  net.Conn
}

// dial is the func handed to jlaffaye/ftp. Every conn is wrapped in a
// guardedConn so that, once the session is established, an idle transfer
// trips the stall timeout instead of hanging.
func (d *ftpDialer) dial(network, address string) (net.Conn, error) {
	conn, err := (&net.Dialer{Timeout: d.timeout}).DialContext(d.ctx, network, address)
	if err != nil {
		return nil, err
	}
	// The first dial is the control connection: hand back the guarded
	// conn (library does its own AUTH TLS upgrade) with a deadline
	// covering banner/AUTH-TLS/login; later data dials must not inherit it.
	if d.ctrlConn == nil {
		armConnectDeadline(conn, d.timeout)
		d.ctrlConn = conn
		return d.guard.wrap(conn), nil
	}
	// Data connection: a custom dial func skips the library's own TLS
	// wrapping, so wrap it here with the same config (shared session
	// cache + ServerName) so pure-ftpd et al. allow the session resume.
	if d.tlsConfig != nil {
		return tls.Client(d.guard.wrap(conn), d.tlsConfig), nil
	}
	return d.guard.wrap(conn), nil
}

// ftpDialAndLogin opens the control connection through d's custom dial
// func and authenticates, bounding the whole connect+login sequence -
// not just the TCP dial - against a server that stalls mid-handshake.
func ftpDialAndLogin(addr string, cfg dialConfig, d *ftpDialer) (*ftp.ServerConn, error) {
	dialOpts := []ftp.DialOption{ftp.DialWithDialFunc(d.dial)}
	if cfg.explicitTLS {
		dialOpts = append(dialOpts, ftp.DialWithExplicitTLS(d.tlsConfig))
	}

	conn, err := ftp.Dial(addr, dialOpts...)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	if err := conn.Login(cfg.Username, cfg.Password); err != nil {
		_ = conn.Quit()
		return nil, fmt.Errorf("login: %w", err)
	}
	return conn, nil
}

// resolveTLSConfig fills in ServerName and ClientSessionCache when
// missing, on a clone so the caller's config isn't mutated - servers
// enforcing data-connection TLS resumption (e.g. pure-ftpd) need both set.
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

// Upload streams r (size bytes total) to remoteName via STOR, reporting
// cumulative bytes through progress.
func (c *FTPClient) Upload(
	remoteName string, r io.Reader, size int64, progress func(sent, total int64),
) error {
	pr := &progressReader{r: r, total: size, onProgress: progress}
	return c.conn.Stor(remoteName, pr)
}

// Size returns the remote file's size in bytes via SIZE.
func (c *FTPClient) Size(remoteName string) (int64, error) {
	return c.conn.FileSize(remoteName)
}

// Verify checks localPath's size against the remote file's. jlaffaye/ftp
// exposes no FEAT/XCRC/XMD5/XSHA1 access, so FTP/FTPS verification is
// size-only.
func (c *FTPClient) Verify(localPath, remoteName string) (method string, err error) {
	return verifyBySize(localPath, remoteName, c.Size)
}

// List returns the names of entries in the current remote directory, used
// only for --dry-run.
func (c *FTPClient) List() ([]string, error) {
	entries, err := c.conn.List("")
	if err != nil {
		return nil, err
	}
	return dirEntryNames(entries, func(e *ftp.Entry) string { return e.Name }), nil
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
