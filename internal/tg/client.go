// client.go - the project's own gotd client wrapper (implemented against the real API of
// gotd v0.163).
//
// Middleware choices (all measured against the live API):
//   - gotd v0.163's main repository has no middleware subpackage (confirmed against the source tree).
//   - retry / recovery reuse the implementation from github.com/iyear/tdl/core (a direct
//     dependency of this project, with thanks);
//   - floodwait lives in github.com/gotd/contrib (not a dependency), so this package
//     implements it in middleware.go.
package tg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tgerr"
	"github.com/iyear/tdl/core/dcpool"

	"github.com/1368129224/tgshelf/internal/config"
)

// Default runtime parameters.
const (
	defaultMaxRetries       = 5                // telegram.Options.MaxRetries
	defaultRetryInterval    = 5 * time.Second  // telegram.Options.RetryInterval
	defaultDialTimeout      = 10 * time.Second // telegram.Options.DialTimeout
	defaultReconnectTimeout = 10 * time.Minute // total budget for reconnect/recovery
)

// Client wraps gotd's telegram.Client together with the lifecycle of the session storage.
type Client struct {
	*telegram.Client
	storage *SessionStorage
}

// New builds a client from the configuration. It only parses arguments and assembles objects
// and makes no network connection (gotd connects when Run is called), so it is safe to call
// from tests.
//
// When storage is nil, a single-file storage is created from the cfg.Telegram.Session path
// (including the process lock).
func New(cfg *config.Config, storage *SessionStorage) (*Client, error) {
	if cfg == nil {
		return nil, fmt.Errorf("tg: config is nil")
	}
	// Credentials are decided by the identity: the tdesktop identity uses the official TD
	// credentials, the custom identity uses the configured ones.
	creds, err := ResolveCredentials(cfg.Telegram)
	if err != nil {
		return nil, err
	}

	if storage == nil {
		sessionPath := strings.TrimSpace(cfg.Telegram.Session)
		if sessionPath == "" {
			return nil, fmt.Errorf("tg: telegram.session path is empty")
		}
		var err error
		storage, err = OpenSession(sessionPath)
		if err != nil {
			return nil, fmt.Errorf("tg: open session: %w", err)
		}
	}

	resolver, err := BuildResolver(cfg.ProxyURL)
	if err != nil {
		_ = storage.Close()
		return nil, fmt.Errorf("tg: build proxy: %w", err)
	}

	// Logging uses the Nop implementation of gotd's own log.Logger interface to avoid a type
	// mismatch with *zap.Logger (which is exactly why core/tclient is disabled). The real log
	// adapter is injected separately by the caller after it implements log.Logger.
	opts := telegram.Options{
		SessionStorage:      storage,
		Device:              DeviceConfigFor(cfg.Telegram),
		Resolver:            resolver,
		ReconnectionBackoff: func() backoff.BackOff { return newPermanentBackoff() },
		Middlewares:         defaultMiddlewares(context.Background(), defaultReconnectTimeout),
		RetryInterval:       defaultRetryInterval,
		MaxRetries:          defaultMaxRetries,
		DialTimeout:         defaultDialTimeout,
	}

	return &Client{
		Client:  telegram.NewClient(creds.AppID, creds.AppHash, opts),
		storage: storage,
	}, nil
}

// Run starts the gotd client, executes fn while it is alive, and closes the connection when
// it returns. The reconnect strategy is handled by telegram.Options.ReconnectionBackoff and
// the middleware chain, so this layer adds no outer reconnect loop. The return value of fn is
// passed through unchanged.
func (c *Client) Run(ctx context.Context, fn func(ctx context.Context) error) error {
	if c == nil || c.Client == nil {
		return errors.New("tg: client is not initialized")
	}
	return c.Client.Run(ctx, fn)
}

// IsAuthorized reports whether the current session is logged in.
//
// It returns false only when Telegram answers with a 401-class error (unauthorized:
// AUTH_KEY_UNREGISTERED, SESSION_EXPIRED, SESSION_REVOKED and so on); other errors (network
// and so on) are returned as is and never swallowed.
func (c *Client) IsAuthorized(ctx context.Context) (bool, error) {
	if c == nil || c.Client == nil {
		return false, errors.New("tg: client is not initialized")
	}
	_, err := c.Client.Self(ctx)
	if err == nil {
		return true, nil
	}
	if tgerr.IsCode(err, 401) {
		return false, nil
	}
	return false, fmt.Errorf("tg: query login status: %w", err)
}

// Close shuts down gracefully: it releases the session file lock. The lifetime of the gotd
// connection is controlled by the ctx of Run, so this only performs idempotent extra cleanup.
func (c *Client) Close() error {
	if c == nil {
		return nil
	}
	if c.storage != nil {
		return c.storage.Close()
	}
	return nil
}

// newPermanentBackoff builds an exponential backoff that retries forever (matching gotd's
// official default behaviour). For a long-lived session MaxElapsedTime must be 0: the
// underlying layer retries automatically on network hiccups and never times out after ten
// minutes.
func newPermanentBackoff() backoff.BackOff {
	b := backoff.NewExponentialBackOff()
	b.Multiplier = 1.1
	b.MaxElapsedTime = 0 // reconnect forever
	b.MaxInterval = 5 * time.Second
	b.InitialInterval = 100 * time.Millisecond
	return b
}

// NewPool creates a multi-DC connection pool on top of the current session (supporting
// concurrent TCP connections to a single DC and direct cross-DC connections, reusing tdl
// core/dcpool). size is the maximum number of underlying connections kept per DC (defaults to
// 8 when <=0).
func (c *Client) NewPool(size int64) dcpool.Pool {
	if c == nil || c.Client == nil {
		return nil
	}
	if size <= 0 {
		size = 8
	}
	return dcpool.NewPool(c.Client, size, defaultMiddlewares(context.Background(), defaultReconnectTimeout)...)
}

// tgerrIsUnauthorized decides whether an error is a Telegram 401 unauthorized error
// (AUTH_KEY_UNREGISTERED, SESSION_EXPIRED, SESSION_REVOKED and so on), which is equivalent to
// "not logged in".
func tgerrIsUnauthorized(err error) bool {
	return tgerr.IsCode(err, 401)
}
