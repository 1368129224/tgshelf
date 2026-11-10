package tg

import (
	"context"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"github.com/iyear/tdl/core/middlewares/recovery"
	"github.com/iyear/tdl/core/middlewares/retry"
)

// defaultMiddlewares assembles the default middleware chain used by this project:
//   - recovery: non-Telegram errors (network hiccups and so on) are retried with exponential backoff;
//   - floodWait: FLOOD_WAIT / FLOOD_PREMIUM_WAIT waits the number of seconds the server asks for, then retries;
//   - retry: Telegram internal transient errors (Timedout and so on) are retried a bounded number of times.
//
// The gotd v0.163 main repository has no separate floodwait package, so it is written here.
func defaultMiddlewares(ctx context.Context, reconnectTimeout time.Duration) []telegram.Middleware {
	return []telegram.Middleware{
		recovery.New(ctx, newReconnectBackoff(reconnectTimeout)),
		newFloodWait(ctx),
		retry.New(5),
	}
}

// newReconnectBackoff builds the exponential backoff policy.
func newReconnectBackoff(timeout time.Duration) backoff.BackOff {
	b := backoff.NewExponentialBackOff()
	b.Multiplier = 1.1
	b.MaxElapsedTime = timeout
	b.MaxInterval = 10 * time.Second
	return b
}

// floodWait is the hand-written FLOOD_WAIT middleware: on FLOOD_WAIT_x it sleeps x+1 seconds
// and retries the same call, until ctx is cancelled.
type floodWait struct {
	ctx context.Context
}

// newFloodWait creates the floodWait middleware. ctx is used to leave the wait early on shutdown.
func newFloodWait(ctx context.Context) telegram.Middleware {
	return floodWait{ctx: ctx}
}

func (f floodWait) Handle(next tg.Invoker) telegram.InvokeFunc {
	return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		for {
			err := next.Invoke(ctx, input, output)
			if err == nil {
				return nil
			}

			wait, ok := tgerr.AsFloodWait(err)
			if !ok {
				return err
			}

			timer := time.NewTimer(wait + time.Second)
			select {
			case <-f.ctx.Done():
				timer.Stop()
				return f.ctx.Err()
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
				// the wait is over, retry the same call
			}
		}
	}
}
