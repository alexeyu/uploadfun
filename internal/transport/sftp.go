package transport

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const defaultSFTPPort = 22

// SFTPDialOptions configures an SSH/SFTP connection. If both Password
// and PrivateKeyPath are set, PrivateKeyPath takes priority and Password
// is tried as the key's passphrase; otherwise whichever one is set is
// used on its own.
type SFTPDialOptions struct {
	Host           string
	Port           int // 0 means defaultSFTPPort
	Username       string
	Password       string
	PrivateKeyPath string
	ConnectTimeout time.Duration
	StallTimeout   time.Duration // 0 disables idle-stall protection
}

// SFTPClient wraps a connected SSH+SFTP session. Not safe for concurrent
// use.
type SFTPClient struct {
	sshConn *ssh.Client
	sftp    *sftp.Client
}

// DialSFTP connects over SSH and opens an SFTP session. The remote host
// key is verified against the current user's ~/.ssh/known_hosts — there
// is no insecure-by-default fallback; an unrecognized host fails with a
// clear error, matching OpenSSH's own trust-on-first-use flow (`ssh` (or
// `ssh-keyscan`) into the host once to populate known_hosts, then retry).
func DialSFTP(ctx context.Context, opts SFTPDialOptions) (*SFTPClient, error) {
	cfg, err := sshClientConfig(opts)
	if err != nil {
		return nil, err
	}

	addr := net.JoinHostPort(opts.Host, strconv.Itoa(resolvePort(opts.Port, defaultSFTPPort)))
	conn, err := (&net.Dialer{Timeout: opts.ConnectTimeout}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	armConnectDeadline(conn, opts.ConnectTimeout)

	// Wrap the conn so that, once established, an idle SFTP transfer trips
	// the stall timeout. It stays passive until then, leaving the connect
	// deadline in force through the handshake and subsystem open.
	guard := &stallGuard{stallTimeout: opts.StallTimeout}
	client, err := openSFTP(guard.wrap(conn), addr, cfg)
	if err != nil {
		return nil, err
	}

	// Session is up; clear the connect deadline and hand transfers over to
	// the idle stall timeout.
	clearDeadline(conn)
	guard.markEstablished()
	return client, nil
}

// sshClientConfig assembles the SSH client config: the auth methods from
// the endpoint's password/key and host-key verification against the user's
// known_hosts.
func sshClientConfig(opts SFTPDialOptions) (*ssh.ClientConfig, error) {
	auth, err := sftpAuthMethods(opts)
	if err != nil {
		return nil, err
	}
	hostKeyCallback, err := knownHostsCallback()
	if err != nil {
		return nil, fmt.Errorf("known_hosts: %w", err)
	}
	return &ssh.ClientConfig{
		User:            opts.Username,
		Auth:            auth,
		HostKeyCallback: hostKeyCallback,
		Timeout:         opts.ConnectTimeout,
	}, nil
}

// openSFTP performs the SSH handshake over conn and opens an SFTP subsystem
// on top, returning a ready client. conn is closed on any failure.
func openSFTP(conn net.Conn, addr string, cfg *ssh.ClientConfig) (*SFTPClient, error) {
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ssh handshake: %w", err)
	}

	client := ssh.NewClient(sshConn, chans, reqs)
	sftpClient, err := sftp.NewClient(client)
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("open sftp session: %w", err)
	}
	return &SFTPClient{sshConn: client, sftp: sftpClient}, nil
}

func sftpAuthMethods(opts SFTPDialOptions) ([]ssh.AuthMethod, error) {
	if opts.PrivateKeyPath != "" {
		signer, err := loadPrivateKey(opts.PrivateKeyPath, opts.Password)
		if err != nil {
			return nil, fmt.Errorf("private key: %w", err)
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	}
	if opts.Password != "" {
		return []ssh.AuthMethod{ssh.Password(opts.Password)}, nil
	}
	return nil, errors.New("sftp requires a password or private key")
}

// loadPrivateKey tries parsing path as an unencrypted key first — a key
// isn't necessarily protected just because Password is also set (Password
// may be present for some other endpoint reason) — and only falls back
// to passphrase-protected parsing if the key itself demands it.
func loadPrivateKey(path, passphrase string) (ssh.Signer, error) {
	keyBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err == nil {
		return signer, nil
	}
	var passErr *ssh.PassphraseMissingError
	if !errors.As(err, &passErr) {
		return nil, err
	}
	if passphrase == "" {
		return nil, fmt.Errorf(
			"key is passphrase-protected but no password was provided to use as its passphrase: %w", err)
	}
	return ssh.ParsePrivateKeyWithPassphrase(keyBytes, []byte(passphrase))
}

func knownHostsCallback() (ssh.HostKeyCallback, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(home, ".ssh", "known_hosts")
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf(
			"%s not found or unreadable — connect once with `ssh` to add the host, then retry: %w",
			path, err)
	}
	return knownhosts.New(path)
}

// Close ends the SFTP session and the underlying SSH connection.
func (c *SFTPClient) Close() error {
	return errors.Join(c.sftp.Close(), c.sshConn.Close())
}

// Delete removes remoteName, treating "file doesn't exist" as success
// since it's called unconditionally before every upload under the
// delete-first overwrite mode.
func (c *SFTPClient) Delete(remoteName string) error {
	err := c.sftp.Remove(remoteName)
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// Upload streams r (size bytes total) to remoteName, invoking progress
// with cumulative bytes sent as it reads.
func (c *SFTPClient) Upload(
	remoteName string, r io.Reader, size int64, progress func(sent, total int64),
) error {
	f, err := c.sftp.Create(remoteName)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()

	pr := &progressReader{r: r, total: size, onProgress: progress}
	_, err = io.Copy(f, pr)
	return err
}

// Size returns the remote file's size in bytes.
func (c *SFTPClient) Size(remoteName string) (int64, error) {
	info, err := c.sftp.Stat(remoteName)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// Verify compares localPath's size against remoteName's size on the
// server. Plain SFTP has no standard hash-comparison equivalent to FTP's
// XCRC/XMD5/XSHA1 extensions, so this is size-only, same as the FTP/FTPS
// transports for v1 (see ARCHITECTURE.md "Verification").
func (c *SFTPClient) Verify(localPath, remoteName string) (method string, err error) {
	return verifyBySize(localPath, remoteName, c.Size)
}

// List returns the names of entries in the current remote directory, used
// only for --dry-run.
func (c *SFTPClient) List() ([]string, error) {
	cwd, err := c.sftp.Getwd()
	if err != nil {
		return nil, err
	}
	entries, err := c.sftp.ReadDir(cwd)
	if err != nil {
		return nil, err
	}
	return dirEntryNames(entries, os.FileInfo.Name), nil
}
