// refresh.go - the serialized refresher for expired FILE_REFERENCE.
//
// # Background
//
// Telegram file references are short-lived credentials: a long download is bound to
// hit FILE_REFERENCE_EXPIRED / FILE_REFERENCE_INVALID / FILEREF_UPGRADE_NEEDED
// halfway through. The right fix is to refetch the message for a fresh Location and
// retry only the current chunk, instead of failing the whole file and starting over.
//
// # Design
//
//   - Serialization (singleflight semantics): when N workers hit expiry at the same
//     time only 1 real RefreshFunc call is allowed; the rest wait and reuse the same
//     result.
//   - Generation (gen) short-circuit: after a worker's request fails it first checks
//     whether gen has already advanced; if another worker refreshed successfully
//     after this request was issued, the new Location is reused without triggering a
//     redundant refresh.
//   - Rate limit and cap: once the cumulative refresh count reaches maxRefreshes
//     (3 by default) a clear error is returned - "file reference keeps expiring,
//     retry later" (wrapping both ErrFileRefExpired and ErrRefreshTooFrequent, both
//     detectable with errors.Is), so a failure never loops forever.
//   - Same-reference detection: when the refreshed Location is equivalent to the old
//     one (same concrete type and a byte-identical FileReference), wait staleWait
//     (5s by default) and try once more; if it is still the same, the refresh counts
//     as a failure and consumes the attempt budget.
//   - Logs record only the refresh count and the elapsed time, never the raw
//     FileReference bytes.
//
// cooldown/staleWait/maxRefreshes are injectable fields; tests may set them to 0 or a
// tiny value to run faster.
package tg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// RefreshFunc refetches the message and returns the new file location (wired from
// Pipeline.ResolveLink).
type RefreshFunc func(ctx context.Context) (tg.InputFileLocationClass, error)

// ErrRefreshTooFrequent: the file reference keeps expiring (the cumulative refresh
// count reached the cap, or the last successful refresh is younger than the cooldown
// window). Decide with errors.Is; the returned error also wraps ErrFileRefExpired, so
// errors.Is matches both.
var ErrRefreshTooFrequent = errors.New("file reference keeps expiring, retry later")

// Default parameters of the refresh rate limit: maxAttempts is a constant;
// cooldown/staleWait are package-level variables that tests in this package may
// override (set to 0 for speed), while the production path always uses the defaults.
var (
	// refreshCooldown is the minimum interval between two successful refreshes.
	refreshCooldown = 30 * time.Second
	// refreshStaleWait is how long to wait when a refresh returns the same reference
	// (followed by one retry).
	refreshStaleWait = 5 * time.Second
)

// refreshMaxAttempts is the cumulative refresh cap for a single file.
const refreshMaxAttempts = 3

// fileRefErrors lists the RPC error codes of the file reference error class.
var fileRefErrors = []string{"FILE_REFERENCE_EXPIRED", "FILE_REFERENCE_INVALID", "FILEREF_UPGRADE_NEEDED"}

// isFileRefErr decides the file reference error class by error code (tgerr.Is, never
// English string containment).
func isFileRefErr(err error) bool {
	return tgerr.Is(err, fileRefErrors...)
}

// flight is one refresh in progress: followers wait for done to close and then read
// the result (the fields are written before the close, so readers need no extra lock).
type flight struct {
	done chan struct{}
	loc  tg.InputFileLocationClass
	err  error
}

// Refresher is the serialized file reference refresher: concurrent expiry triggers a
// single real refetch, and it maintains the "currently effective Location" for the
// remaining chunks of a Download.
// An instance is bound to the Download lifetime of a single file (Download creates a
// new one each time), so the refresh counter is naturally isolated per file.
type Refresher struct {
	fn     RefreshFunc
	logger *zap.Logger

	mu           sync.Mutex
	inflight     *flight
	current      tg.InputFileLocationClass // currently effective Location (seed or last successful refresh)
	gen          int                       // generation of current: +1 per successful refresh
	count        int                       // cumulative RefreshFunc calls (including failures)
	last         time.Time                 // time of the last successful refresh
	cooldown     time.Duration
	staleWait    time.Duration
	maxRefreshes int
}

// NewRefresher builds the serialized refresher (singleflight semantics internally:
// concurrent expiry triggers one refetch).
// fn is called only on Refresh; logger may be nil (falls back to Nop).
func NewRefresher(fn RefreshFunc, logger *zap.Logger) *Refresher {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Refresher{
		fn:           fn,
		logger:       logger,
		cooldown:     refreshCooldown,
		staleWait:    refreshStaleWait,
		maxRefreshes: refreshMaxAttempts,
	}
}

// seed presets the currently effective Location: the original (soon to expire)
// reference serves as the baseline for the same-reference comparison.
func (r *Refresher) seed(loc tg.InputFileLocationClass) {
	r.mu.Lock()
	r.current = loc
	r.mu.Unlock()
}

// Current returns the currently effective Location (concurrency safe).
func (r *Refresher) Current() tg.InputFileLocationClass {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.current
}

// Gen returns the generation of the current Location; +1 per successful refresh.
func (r *Refresher) Gen() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.gen
}

// Refresh returns the refreshed Location.
//
//   - if a refresh is already in progress, wait for it and reuse its result (success
//     or failure);
//   - otherwise act as leader and check the rate limit (cumulative count); past the
//     limit return a clear error wrapping ErrRefreshTooFrequent and ErrFileRefExpired;
//   - fn returning nil or an error counts as a failed refresh (wrapping
//     ErrFileRefExpired together with the refresh count);
//   - when fn returns a Location equivalent to the old reference, wait staleWait and
//     retry once; still equivalent then counts as a failed refresh (both fn calls
//     count against the cap).
//
// Cancellable through ctx; a waiting follower returns ctx.Err() when cancelled.
func (r *Refresher) Refresh(ctx context.Context) (tg.InputFileLocationClass, error) {
	r.mu.Lock()
	if r.inflight != nil {
		fl := r.inflight
		r.mu.Unlock()
		select {
		case <-fl.done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return fl.loc, fl.err
	}
	fl := &flight{done: make(chan struct{})}
	r.inflight = fl
	r.mu.Unlock()

	loc, err := r.run(ctx)

	r.mu.Lock()
	fl.loc, fl.err = loc, err
	r.inflight = nil
	r.mu.Unlock()
	close(fl.done)
	return loc, err
}

// run is the leader path: it actually calls RefreshFunc and does validation, the
// same-reference retry and counting.
func (r *Refresher) run(ctx context.Context) (tg.InputFileLocationClass, error) {
	staleRetried := false
	for {
		r.mu.Lock()
		n := r.count
		last := r.last
		cooldown := r.cooldown
		maxRefreshes := r.maxRefreshes
		r.mu.Unlock()

		if n >= maxRefreshes {
			err := fmt.Errorf("tg: %w (already refreshed %d times, limit %d): %w",
				ErrRefreshTooFrequent, n, maxRefreshes, ErrFileRefExpired)
			r.logger.Warn("tg: file reference refresh rate limited", zap.Int("count", n), zap.Error(err))
			return nil, err
		}
		if !staleRetried && cooldown > 0 && !last.IsZero() && time.Since(last) < cooldown {
			err := fmt.Errorf("tg: %w (last refresh less than %v ago, refreshed %d times): %w",
				ErrRefreshTooFrequent, cooldown, n, ErrFileRefExpired)
			r.logger.Warn("tg: file reference refresh rate limited", zap.Int("count", n), zap.Error(err))
			return nil, err
		}

		if staleRetried {
			// Same-reference detection hit: wait and retry once (immediate when injected
			// as 0).
			if err := sleepCtx(ctx, r.staleWait); err != nil {
				return nil, err
			}
		}

		start := time.Now()
		loc, err := r.fn(ctx)
		took := time.Since(start)

		r.mu.Lock()
		r.count++
		n = r.count
		if err != nil {
			r.mu.Unlock()
			r.logger.Warn("tg: file reference refresh failed",
				zap.Int("count", n), zap.Duration("elapsed", took), zap.Error(err))
			return nil, fmt.Errorf("tg: refresh %d of the file reference failed: %w: %w", n, ErrFileRefExpired, err)
		}
		if loc == nil {
			r.mu.Unlock()
			r.logger.Warn("tg: file reference refresh returned a nil location", zap.Int("count", n), zap.Duration("elapsed", took))
			return nil, fmt.Errorf("tg: refresh %d returned a nil location (refreshed %d times): %w", n, n, ErrFileRefExpired)
		}
		if r.current != nil && sameLocation(r.current, loc) {
			if !staleRetried {
				staleRetried = true
				r.mu.Unlock()
				r.logger.Warn("tg: refresh returned a Location equivalent to the old reference, retrying once after a wait", zap.Int("count", n))
				continue
			}
			r.mu.Unlock()
			r.logger.Warn("tg: refresh still returned the old reference after the retry, treating it as a failure", zap.Int("count", n), zap.Duration("elapsed", took))
			return nil, fmt.Errorf("tg: refreshed file reference equals the old one (refreshed %d times): %w", n, ErrFileRefExpired)
		}
		// Success: update the currently effective Location, bump the generation, record
		// the time.
		r.current = loc
		r.gen++
		r.last = time.Now()
		r.mu.Unlock()
		r.logger.Info("tg: file reference refresh succeeded", zap.Int("count", n), zap.Duration("elapsed", took))
		return loc, nil
	}
}

// sameLocation decides whether two Locations are equivalent: same concrete type and a
// byte-identical FileReference (covering the *tg.InputFileLocation named by the
// contract and the other common input location types).
func sameLocation(a, b tg.InputFileLocationClass) bool {
	if a == nil || b == nil {
		return false
	}
	if reflect.TypeOf(a) != reflect.TypeOf(b) {
		return false
	}
	ra, ok1 := fileRefBytes(a)
	rb, ok2 := fileRefBytes(b)
	return ok1 && ok2 && bytes.Equal(ra, rb)
}

// fileRefBytes extracts the FileReference bytes of a Location (only for the equality
// comparison, never logged).
func fileRefBytes(loc tg.InputFileLocationClass) ([]byte, bool) {
	switch l := loc.(type) {
	case *tg.InputFileLocation:
		return l.FileReference, true
	case *tg.InputDocumentFileLocation:
		return l.FileReference, true
	case *tg.InputPhotoFileLocation:
		return l.FileReference, true
	default:
		return nil, false
	}
}

// sleepCtx waits for d; d<=0 waits not at all (tests inject 0 for speed), and it always
// honors an already-cancelled ctx.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
