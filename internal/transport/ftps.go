package transport

import (
	"context"
	"crypto/tls"
	"time"
)

// FTPSDialOptions configures an explicit-AUTH-TLS FTP connection (the
// ftps protocol). The control connection starts on the same port as
// plain FTP (21 by default) and is upgraded to TLS via the AUTH TLS
// command, rather than dialing straight into TLS on a separate port
// (implicit FTPS, not supported here).
type FTPSDialOptions struct {
	Host           string
	Port           int // 0 means defaultFTPPort
	Username       string
	Password       string
	ConnectTimeout time.Duration
	// TLSConfig overrides the default. Optional; any of ServerName or
	// ClientSessionCache left unset are filled in automatically (see
	// resolveTLSConfig) — both are required for many servers' data
	// connections to work at all, not just cosmetic defaults.
	TLSConfig *tls.Config
}

// DialFTPS connects and authenticates over FTP with explicit AUTH TLS.
// It returns the same FTPClient as DialFTP — once connected, FTP and
// FTPS sessions behave identically for upload/delete/verify/list.
func DialFTPS(ctx context.Context, opts FTPSDialOptions) (*FTPClient, error) {
	return dial(ctx, dialConfig{
		Host:           opts.Host,
		Port:           opts.Port,
		Username:       opts.Username,
		Password:       opts.Password,
		ConnectTimeout: opts.ConnectTimeout,
		explicitTLS:    true,
		tlsConfig:      opts.TLSConfig,
	})
}
