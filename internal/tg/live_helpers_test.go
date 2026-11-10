//go:build live

// live_helpers_test.go - small helpers for live acceptance (compiled only with -tags live).
package tg

import (
	"context"
	"strings"
	"testing"

	"github.com/gotd/td/tg"
)

// liveMessageIDs pulls the *tg.Message IDs out of a getHistory result container, keeping
// their order.
func liveMessageIDs(resp tg.MessagesMessagesClass) []int {
	var ids []int
	for _, m := range liveMessages(resp) {
		ids = append(ids, m.ID)
	}
	return ids
}

// liveMessages flattens a result container into a []*tg.Message slice.
func liveMessages(resp tg.MessagesMessagesClass) []*tg.Message {
	var raw []tg.MessageClass
	switch h := resp.(type) {
	case *tg.MessagesMessages:
		raw = h.Messages
	case *tg.MessagesMessagesSlice:
		raw = h.Messages
	case *tg.MessagesChannelMessages:
		raw = h.Messages
	default:
		return nil
	}
	out := make([]*tg.Message, 0, len(raw))
	for _, mc := range raw {
		if m, ok := mc.(*tg.Message); ok {
			out = append(out, m)
		}
	}
	return out
}

// liveHasServiceMessage reports whether the result contains a MessageService (used as the
// counter-example for the "service message" sample).
func liveHasServiceMessage(resp tg.MessagesMessagesClass) bool {
	var raw []tg.MessageClass
	switch h := resp.(type) {
	case *tg.MessagesMessages:
		raw = h.Messages
	case *tg.MessagesMessagesSlice:
		raw = h.Messages
	case *tg.MessagesChannelMessages:
		raw = h.Messages
	}
	for _, mc := range raw {
		if _, ok := mc.(*tg.MessageService); ok {
			return true
		}
	}
	return false
}

// liveChannelAccess fetches a channel's real access_hash (by looking it up in the dialog
// list).
func liveChannelAccess(t *testing.T, ctx context.Context, env *liveEnv, channelID int64) int64 {
	t.Helper()
	dialogs, err := env.api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
		OffsetPeer: &tg.InputPeerEmpty{}, Limit: 100,
	})
	if err != nil {
		t.Fatalf("getDialogs: %v", err)
	}
	var chats []tg.ChatClass
	switch d := dialogs.(type) {
	case *tg.MessagesDialogs:
		chats = d.Chats
	case *tg.MessagesDialogsSlice:
		chats = d.Chats
	}
	for _, ch := range chats {
		if c, ok := ch.(*tg.Channel); ok && c.ID == channelID {
			return c.AccessHash
		}
	}
	t.Fatalf("channel %d is not in the dialog list (the account may have been logged out)", channelID)
	return 0
}

// liveCommentAccess fetches the access_hash of the discussion main channel.
func liveCommentAccess(t *testing.T, ctx context.Context, env *liveEnv) int64 {
	t.Helper()
	return liveChannelAccess(t, ctx, env, liveCommentMain)
}

// containsAny reports whether s contains any of the substrings.
func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
