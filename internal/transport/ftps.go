package transport

import (
	"context"
	"crypto/tls"
	"time"
)

// FTPSDialOptions configures an explicit-AUTH-TLS FTP connection (the
// ftps protocol): the control connection starts on port 21 like plain
// FTP and upgrades via AUTH TLS, not implicit FTPS on a separate port.
type FTPSDialOptions struct {
	Host           string
	Port           int // 0 means defaultFTPPort
	Username       string
	Password       string
	ConnectTimeout time.Duration
	StallTimeout   time.Duration // 0 disables idle-stall protection
	// TLSConfig overrides the default. Optional; ServerName or
	// ClientSessionCache left unset are filled in automatically (see
	// resolveTLSConfig) - required for many servers' data connections.
	TLSConfig *tls.Config
}

// DialFTPS connects and authenticates over FTP with explicit AUTH TLS.
// It returns the same FTPClient as DialFTP - once connected, FTP and
// FTPS sessions behave identically for upload/delete/verify/list.
func DialFTPS(ctx context.Context, opts FTPSDialOptions) (*FTPClient, error) {
	return dial(ctx, dialConfig{
		Host:           opts.Host,
		Port:           opts.Port,
		Username:       opts.Username,
		Password:       opts.Password,
		ConnectTimeout: opts.ConnectTimeout,
		StallTimeout:   opts.StallTimeout,
		explicitTLS:    true,
		tlsConfig:      opts.TLSConfig,
	})
}
