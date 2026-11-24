// takeout.go - takeout session wrapper.
//
// Telegram's takeout session is meant for bulk-exporting your own data: the server counts
// FLOOD_WAIT for takeout connections separately from normal connections, and exported data is
// not subject to the usual rate limit.
//
// Flow:
//  1. account.initTakeoutSession{files:true, file_max_size:...} returns the takeout id;
//  2. every subsequent request is wrapped in invokeWithTakeout{takeout_id};
//  3. when the task ends, call account.finishTakeoutSession{success:true}.
//
// This implementation wraps any Invoker as a middleware, so it can be layered on every
// connection of the dcpool.
package tg

import (
	"context"
	"fmt"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// InitTakeoutSession starts a takeout session and returns the takeout id.
// fileMaxSizeMB is the per-file size limit in MiB (Telegram caps it at 2000 MiB; this passes
// 4000 as a fallback so the server applies its own cap).
func InitTakeoutSession(ctx context.Context, api *tg.Client, fileMaxSizeMB int32) (int64, error) {
	var fileMaxSize int64
	if fileMaxSizeMB > 0 {
		fileMaxSize = int64(fileMaxSizeMB) << 20
	}
	req := &tg.AccountInitTakeoutSessionRequest{
		Contacts:          true,
		MessageUsers:      true,
		MessageChats:      true,
		MessageMegagroups: true,
		MessageChannels:   true,
		Files:             true,
		FileMaxSize:       fileMaxSize,
	}
	req.SetFlags()

	session, err := api.AccountInitTakeoutSession(ctx, req)
	if err != nil {
		return 0, fmt.Errorf("tg: init takeout session: %w", err)
	}
	return session.ID, nil
}

// FinishTakeoutSession ends a takeout session (success=true means it completed normally).
// Per Telegram's MTProto specification, account.finishTakeoutSession must be invoked inside
// invokeWithTakeout{takeout_id}.
func FinishTakeoutSession(ctx context.Context, api *tg.Client, takeoutID int64) error {
	if api == nil {
		return fmt.Errorf("tg: finish takeout session: api client is nil")
	}
	req := &tg.AccountFinishTakeoutSessionRequest{Success: true}
	req.SetFlags()
	wrapped := tg.NewClient(NewTakeoutInvoker(api.Invoker(), takeoutID))
	if _, err := wrapped.AccountFinishTakeoutSession(ctx, req); err != nil {
		// Errors such as SESSION_PASSWORD_NEEDED must not be swallowed, but "takeout is
		// already gone" style errors can be ignored.
		if tgerr.Is(err, "TAKEOUT_INVALID", "TAKEOUT_NULL") {
			return nil
		}
		return fmt.Errorf("tg: finish takeout session: %w", err)
	}
	return nil
}

// takeoutInvoker is the middleware that wraps every request in invokeWithTakeout.
type takeoutInvoker struct {
	next    tg.Invoker
	takeout int64
}

// NewTakeoutInvoker wraps any invoker with a takeout id.
func NewTakeoutInvoker(next tg.Invoker, takeoutID int64) tg.Invoker {
	return &takeoutInvoker{next: next, takeout: takeoutID}
}

func (t *takeoutInvoker) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	// input is a bin.Encoder; the Query of invokeWithTakeout needs a bin.Object.
	// Every gotd request type implements both, so assert it back out here.
	obj, ok := input.(bin.Object)
	if !ok {
		return fmt.Errorf("tg: takeout wrapper needs a bin.Object input, got %T", input)
	}
	req := &tg.InvokeWithTakeoutRequest{
		TakeoutID: t.takeout,
		Query:     obj,
	}

	// The response type of invokeWithTakeout matches the inner query; Invoke unpacks it
	// automatically.
	return t.next.Invoke(ctx, req, output)
}
