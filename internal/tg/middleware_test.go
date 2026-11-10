package tg

import (
	"context"
	"errors"
	"testing"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// nextFunc is the simplified test-double signature for tg.Invoker.
type nextFunc func(ctx context.Context, input bin.Encoder, output bin.Decoder) error

// asInvoker wraps nextFunc as a tg.Invoker.
type testInvoker nextFunc

func (f testInvoker) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	return f(ctx, input, output)
}

func asInvoker(fn nextFunc) tg.Invoker {
	return testInvoker(fn)
}

// TestFloodWaitMiddlewareRetry verifies that the first call hits FLOOD_WAIT_1 and the second
// one succeeds.
func TestFloodWaitMiddlewareRetry(t *testing.T) {
	calls := 0
	next := asInvoker(func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		calls++
		if calls == 1 {
			return tgerr.New(420, "FLOOD_WAIT_1")
		}
		return nil
	})

	mw := newFloodWait(context.Background())
	if err := mw.Handle(next)(context.Background(), nil, nil); err != nil {
		t.Fatalf("floodWait should retry and succeed, got %v", err)
	}
	if calls != 2 {
		t.Fatalf("calls = %d, want 2", calls)
	}
}

// TestFloodWaitMiddlewarePassthrough verifies that non-flood errors are returned unchanged.
func TestFloodWaitMiddlewarePassthrough(t *testing.T) {
	wantErr := errors.New("other error")
	next := asInvoker(func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		return wantErr
	})

	mw := newFloodWait(context.Background())
	err := mw.Handle(next)(context.Background(), nil, nil)
	if !errors.Is(err, wantErr) {
		t.Fatalf("non-flood error should pass through, got %v", err)
	}
}

// TestFloodWaitContextCancel verifies that cancelling ctx interrupts a long wait.
func TestFloodWaitContextCancel(t *testing.T) {
	next := asInvoker(func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		return tgerr.New(420, "FLOOD_WAIT_3600")
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately
	mw := newFloodWait(ctx)
	if err := mw.Handle(next)(context.Background(), nil, nil); err == nil {
		t.Fatalf("cancelled ctx should abort flood wait")
	}
}

// Anchor check: the defaultMiddlewares return value can be assigned directly to
// telegram.Options.Middlewares.
func TestDefaultMiddlewaresShape(t *testing.T) {
	ms := defaultMiddlewares(context.Background(), 0)
	if len(ms) == 0 {
		t.Fatalf("defaultMiddlewares should not be empty")
	}
	var opts telegram.Options
	opts.Middlewares = ms
}
