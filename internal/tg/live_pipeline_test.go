//go:build live

// live_pipeline_test.go - real-path tests against the live API.
//
// This file is **compiled only when -tags live is given explicitly**; the default
// `go test ./...` never sees it, so it can never issue a network request in CI or in an
// ordinary local run.
//
// How to run it (requires a logged-in data/session.json):
//
//	export PATH="/usr/local/go/bin:/usr/bin:/bin:$PATH"
//	export GOPATH=$PWD/.gohome GOMODCACHE=$PWD/.gohome/pkg/mod GOCACHE=$PWD/.gohome/cache
//	go test ./internal/tg/ -tags live -run TestLive -count=1 -v
//
// Design principles:
//  1. Every subtest corresponds to one real-path behavior (or one unmeasured item) and its
//     name carries the number, so the output summary can be copied into the docs;
//  2. Assertions go through the **production code path** (Pipeline.ResolveLink) rather
//     than duplicating the resolution logic; only samples that cannot be produced
//     artificially in a live environment (such as a deleted message) call the RPC directly
//     as a counter-check;
//  3. Any failure says outright in its output that "the real path did not pass" and never
//     substitutes an offline conclusion;
//  4. The sample IDs are a snapshot picked from the user's real account dialog list on
//     2026-11-02 and verified one by one. If the account leaves those channels/groups the
//     corresponding subtest fails and asks for fresh samples instead of silently skipping.
package tg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"github.com/1368129224/tgshelf/internal/config"
	"github.com/1368129224/tgshelf/internal/state"
)

// ---- sample snapshot (verified to exist on 2026-11-02) ----

const (
	// livePublicChannel: the official @telegram channel, publicly visible, used for a
	// single message / a topic / no media.
	livePublicChannel = int64(1005640892)

	// livePrivateChannel: a private supergroup the user has joined (no username), holding
	// 3 albums (grouped) and several plain-text messages, used for private channels and
	// the cache tests.
	livePrivateChannel = int64(3479480016)

	// liveAlbumChannel: the public @music_tag_web channel, holding one 9-item album
	// (44..52), used for a stable reproduction of album expansion (a public channel does
	// not depend on whether the user left the group).
	liveAlbumChannel = int64(1863711228)

	// liveCommentMain: @CMLiussss_channel, which has a real linked discussion group
	// (linked_chat).
	liveCommentMain = int64(2022954205)
	// liveCommentPost: a post **with media** in that channel, corresponding to discussion
	// group root message 2692922.
	liveCommentPost = 250
	// liveCommentMsgID: the message ID inside the discussion group (i.e. the value of
	// ?comment=).
	liveCommentMsgID = 2692922

	// liveForumChat: a private forum the user has joined (forum=true megagroup), used for
	// topic links.
	liveForumChat = int64(1256010988)
	// liveForumTopicID / liveForumMsgID: a message in the forum known to belong to some
	// topic.
	liveForumTopicID = 250026
	liveForumMsgID   = 250293

	// liveDeletedByOffset: 38660 is already deleted in that channel, so the server returns
	// the earlier 38659.
	liveDeletedByOffset = 38660
	// liveServiceMsg: 38667 is a MessageService (a join notification), not a regular
	// message.
	liveServiceMsg = 38667

	// livePlainTextChannel / livePlainTextMsg: a plain-text message in @LSPosed.
	livePlainTextChannel = int64(1499638348)
	livePlainTextMsg     = 326

	// liveInaccessibleChannel: a channel the user has not joined, for which the server
	// returns CHANNEL_INVALID.
	liveInaccessibleChannel = int64(4711000000)
)

// ---- live environment setup ----

// liveRepoRoot walks up from the current directory looking for go.mod to locate the
// repository root (both the session and the config live there).
func liveRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd failed: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("cannot find the repository root (go.mod)")
		}
		dir = parent
	}
}

// liveEnv is the shared environment of one live test.
type liveEnv struct {
	api       *tg.Client // raw client (for counter-check scenarios that must issue RPCs directly)
	counting  *countingRPC
	pipeline  *Pipeline
	resolver  *PeerResolver
	store     *state.Store
	closeFn   func()
	sessionOK bool
}

// countingRPC counts calls to specific RPCs, for assertions such as "a cache hit must mean
// zero RPCs". Every other method is forwarded unchanged to the embedded *tg.Client.
type countingRPC struct {
	*tg.Client
	getChannels atomic.Int64
	getFullChat atomic.Int64
	getHistory  atomic.Int64
}

func (c *countingRPC) ChannelsGetChannels(ctx context.Context, id []tg.InputChannelClass) (tg.MessagesChatsClass, error) {
	c.getChannels.Add(1)
	return c.Client.ChannelsGetChannels(ctx, id)
}

func (c *countingRPC) ChannelsGetFullChannel(ctx context.Context, channel tg.InputChannelClass) (*tg.MessagesChatFull, error) {
	c.getFullChat.Add(1)
	return c.Client.ChannelsGetFullChannel(ctx, channel)
}

func (c *countingRPC) MessagesGetHistory(ctx context.Context, request *tg.MessagesGetHistoryRequest) (tg.MessagesMessagesClass, error) {
	c.getHistory.Add(1)
	return c.Client.MessagesGetHistory(ctx, request)
}

// Compile-time assertion: countingRPC still satisfies PipelineRPC.
var _ PipelineRPC = (*countingRPC)(nil)

// withLive sets up the live environment and keeps the gotd connection alive while fn runs.
//
// Every subtest runs against its own state.Store (a temporary directory) so caches cannot
// pollute each other; the "cache hit" test therefore has to resolve twice inside the same
// env itself.
func withLive(t *testing.T, fn func(ctx context.Context, env *liveEnv)) {
	t.Helper()

	root := liveRepoRoot(t)
	cfgPath := os.Getenv("TDFB_LIVE_CONFIG")
	if cfgPath == "" {
		cfgPath = filepath.Join(root, "tgshelf_config.toml")
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("failed to load config (%s): %v", cfgPath, err)
	}
	sessionPath := filepath.Join(root, "data", "session.json")
	if _, statErr := os.Stat(sessionPath); statErr != nil {
		t.Skipf("%s not found: live acceptance requires tgshelf -login first (a SKIP is not a pass)", sessionPath)
	}

	storage, err := OpenSession(sessionPath)
	if err != nil {
		t.Fatalf("failed to open the session (is another tgshelf running and holding the lock?): %v", err)
	}
	client, err := New(cfg, storage)
	if err != nil {
		_ = storage.Close()
		t.Fatalf("failed to construct the client: %v", err)
	}

	store, err := state.New(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		_ = storage.Close()
		t.Fatalf("failed to construct state.Store: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)

	// Key point: the connection runs on its own goroutine while the assertions (including
	// t.Fatalf) stay on the **test goroutine**. Calling t.Fatalf inside the client.Run
	// callback would only end that callback goroutine via runtime.Goexit, Run would never
	// return, and the test would hang until the ctx timeout -- a pitfall we already hit.
	ready := make(chan *liveEnv, 1)
	runErr := make(chan error, 1)
	go func() {
		runErr <- client.Run(ctx, func(ctx context.Context) error {
			counting := &countingRPC{Client: client.API()}
			resolver, rerr := NewPeerResolver(counting, store, 6*time.Hour)
			if rerr != nil {
				return fmt.Errorf("construct PeerResolver: %w", rerr)
			}
			pipeline, perr := NewPipeline(counting, resolver)
			if perr != nil {
				return fmt.Errorf("construct Pipeline: %w", perr)
			}
			ready <- &liveEnv{
				api:      counting.Client,
				counting: counting,
				pipeline: pipeline,
				resolver: resolver,
				store:    store,
			}
			<-ctx.Done() // keep the connection until the test cancels it deliberately
			return nil
		})
	}()

	var env *liveEnv
	select {
	case env = <-ready:
	case err := <-runErr:
		cancel()
		_ = store.Close()
		_ = storage.Close()
		t.Fatalf("failed to establish the live session: %v", err)
	case <-time.After(90 * time.Second):
		cancel()
		_ = store.Close()
		_ = storage.Close()
		t.Fatal("timed out establishing the live session (90s)")
	}

	// Run the assertions on the test goroutine, then clean up uniformly.
	fn(ctx, env)

	cancel()
	select {
	case err := <-runErr:
		if err != nil {
			t.Errorf("the live session reported an error while shutting down: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Error("the live session did not shut down within 30s")
	}
	_ = store.Close()
	_ = storage.Close()
}

// livePrivateURL builds a private-channel link of the form t.me/c.
func livePrivateURL(channelID int64, msgID int) string {
	return fmt.Sprintf("https://t.me/c/%d/%d", channelID, msgID)
}

// assertResolved checks the basic invariants of a successful resolution and returns nothing
// so the caller can assert further.
func assertResolved(t *testing.T, res *Resolved, wantMsgID int) {
	t.Helper()
	if res == nil {
		t.Fatal("resolution result is nil")
	}
	if res.MsgID != wantMsgID {
		t.Fatalf("MsgID = %d, want %d", res.MsgID, wantMsgID)
	}
	if len(res.Items) == 0 {
		t.Fatal("Items is empty; a successful resolution must yield at least 1 item")
	}
	for i, it := range res.Items {
		if it.Size <= 0 {
			t.Errorf("Items[%d].Size = %d; real media must have a positive size", i, it.Size)
		}
		if it.DCID <= 0 {
			t.Errorf("Items[%d].DCID = %d; real media must have a valid DC", i, it.DCID)
		}
		if len(it.FileRef) == 0 {
			t.Errorf("Items[%d].FileRef is empty; a file_reference handed out for real must not be empty", i)
		}
		if it.Location == nil {
			t.Errorf("Items[%d].Location is nil; the download would fail", i)
		}
		if it.MessageID <= 0 {
			t.Errorf("Items[%d].MessageID = %d", i, it.MessageID)
		}
	}
}

// TestLivePipeline is the main entry point of the live tests; every subtest
// corresponds to one item in the docs.
func TestLivePipeline(t *testing.T) {
	t.Run("live-1a public channel single message", func(t *testing.T) {
		withLive(t, func(ctx context.Context, env *liveEnv) {
			url := fmt.Sprintf("https://t.me/telegram/193")
			res, err := env.pipeline.ResolveLink(ctx, url)
			if err != nil {
				t.Fatalf("ResolveLink(%s): %v", url, err)
			}
			assertResolved(t, res, 193)
			if peer, ok := res.Peer.(*tg.InputPeerChannel); !ok || peer.ChannelID != livePublicChannel {
				t.Fatalf("Peer = %T(%v), want channel %d", res.Peer, res.Peer, livePublicChannel)
			}
			if res.Items[0].Kind != MediaVideo {
				t.Errorf("Kind = %v, want MediaVideo (the message itself is a video)", res.Items[0].Kind)
			}
			t.Logf("real result: peer=channel:%d msg=%d items=%d kind=%s name=%q size=%d",
				livePublicChannel, res.MsgID, len(res.Items), res.Items[0].Kind, res.Items[0].Name, res.Items[0].Size)
		})
	})

	t.Run("live-2a private channel, cache miss, getChannels fills in access_hash", func(t *testing.T) {
		withLive(t, func(ctx context.Context, env *liveEnv) {
			url := livePrivateURL(livePrivateChannel, 38666)
			res, err := env.pipeline.ResolveLink(ctx, url)
			if err != nil {
				t.Fatalf("ResolveLink(%s): %v (you may need to join channel %d first)", url, err, livePrivateChannel)
			}
			assertResolved(t, res, 38666)
			if got := env.counting.getChannels.Load(); got != 1 {
				t.Fatalf("ChannelsGetChannels was called %d times, want 1 (the cache should be empty)", got)
			}
			t.Logf("real result: cache miss -> getChannels=1, resolved %d items", len(res.Items))
		})
	})

	t.Run("live-2b private channel, cache hit with zero RPCs", func(t *testing.T) {
		withLive(t, func(ctx context.Context, env *liveEnv) {
			url := livePrivateURL(livePrivateChannel, 38666)
			// First call: cache miss, which calls getChannels and writes the cache.
			if _, err := env.pipeline.ResolveLink(ctx, url); err != nil {
				t.Fatalf("first ResolveLink: %v", err)
			}
			first := env.counting.getChannels.Load()

			// Second call: same env/Store, so it must hit the cache and must not call
			// getChannels again.
			res, err := env.pipeline.ResolveLink(ctx, url)
			if err != nil {
				t.Fatalf("second ResolveLink: %v", err)
			}
			assertResolved(t, res, 38666)
			second := env.counting.getChannels.Load()
			if second != first {
				t.Fatalf("ChannelsGetChannels was called again after the cache hit: %d -> %d", first, second)
			}
			t.Logf("real result: getChannels=%d on call 1, getChannels=%d on call 2 (no new calls)", first, second)
		})
	})

	t.Run("live-3 private channel not joined, ErrChannelInaccessible", func(t *testing.T) {
		withLive(t, func(ctx context.Context, env *liveEnv) {
			url := livePrivateURL(liveInaccessibleChannel, 45)
			_, err := env.pipeline.ResolveLink(ctx, url)
			if err == nil {
				t.Fatalf("ResolveLink(%s) unexpectedly succeeded; the account must not be able to access that channel", url)
			}
			if !errors.Is(err, ErrChannelInaccessible) {
				t.Fatalf("err = %v, want ErrChannelInaccessible", err)
			}
			// The error must offer an executable next step.
			if !containsAny(err.Error(), "join", "/sync") {
				t.Errorf("the error text lacks an actionable hint (it should say \"join it first, then send /sync\"): %v", err)
			}
			t.Logf("real result: %v", err)
		})
	})

	t.Run("live-4 album expansion, real item count and ascending order", func(t *testing.T) {
		withLive(t, func(ctx context.Context, env *liveEnv) {
			// 44..52 in @music_tag_web form one 9-item album; resolving from the smallest
			// ID should return the whole group.
			const (
				albumChannel = liveAlbumChannel
				albumFirstID = 44
				wantCount    = 9
			)
			url := fmt.Sprintf("https://t.me/music_tag_web/%d", albumFirstID)
			res, err := env.pipeline.ResolveLink(ctx, url)
			if err != nil {
				t.Fatalf("ResolveLink(%s): %v", url, err)
			}
			assertResolved(t, res, albumFirstID)
			if len(res.Items) != wantCount {
				t.Fatalf("Items = %d, want %d (the real album item count)", len(res.Items), wantCount)
			}
			// Assert ascending order consistent with the real, contiguously increasing IDs.
			for i := 1; i < len(res.Items); i++ {
				if res.Items[i].MessageID <= res.Items[i-1].MessageID {
					t.Fatalf("the album is not ascending by ID: Items[%d]=%d <= Items[%d]=%d",
						i, res.Items[i].MessageID, i-1, res.Items[i-1].MessageID)
				}
			}
			// Cross-check against an independent RPC scan: the count and the set must match.
			peer := res.Peer
			resp, herr := env.api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
				Peer: peer, OffsetID: albumFirstID + 11, Limit: 20,
			})
			if herr != nil {
				t.Fatalf("cross-check getHistory: %v", herr)
			}
			realIDs := liveMessageIDs(resp)
			var albumIDs []int
			for _, id := range realIDs {
				if id >= albumFirstID && id < albumFirstID+wantCount {
					albumIDs = append(albumIDs, id)
				}
			}
			sort.Ints(albumIDs)
			if len(albumIDs) != len(res.Items) {
				t.Fatalf("cross-check: the server has %d media messages in that range, resolution produced %d", len(albumIDs), len(res.Items))
			}
			for i := range albumIDs {
				if res.Items[i].MessageID != albumIDs[i] {
					t.Fatalf("Items[%d].MessageID = %d, the server says %d", i, res.Items[i].MessageID, albumIDs[i])
				}
			}
			t.Logf("real result: %d album items, IDs (ascending) %v, channel=%d",
				len(res.Items), albumIDs, albumChannel)
		})
	})

	t.Run("live-5 comment link, real linked_chat resolution", func(t *testing.T) {
		withLive(t, func(ctx context.Context, env *liveEnv) {
			url := fmt.Sprintf("https://t.me/CMLiussss_channel/%d?comment=%d", liveCommentPost, liveCommentMsgID)
			res, err := env.pipeline.ResolveLink(ctx, url)
			if err != nil {
				t.Fatalf("ResolveLink(%s): %v", url, err)
			}
			// Key assertion: the message is actually fetched with CommentID, not the main
			// channel post ID.
			assertResolved(t, res, liveCommentMsgID)
			if res.Ref.Kind != RefComment {
				t.Fatalf("Ref.Kind = %v, want RefComment", res.Ref.Kind)
			}
			if res.Ref.MsgID != liveCommentPost {
				t.Fatalf("Ref.MsgID = %d, want %d (the main channel post)", res.Ref.MsgID, liveCommentPost)
			}
			// Counter-check: the peer located via linked_chat must be the discussion group
			// itself.
			full, ferr := env.resolver.rpc.ChannelsGetFullChannel(ctx, &tg.InputChannel{
				ChannelID: liveCommentMain, AccessHash: liveCommentAccess(t, ctx, env),
			})
			if ferr != nil {
				t.Fatalf("getFullChannel(%d): %v", liveCommentMain, ferr)
			}
			cf, ok := full.FullChat.(*tg.ChannelFull)
			if !ok {
				t.Fatalf("FullChat type %T", full.FullChat)
			}
			linked, hasLinked := cf.GetLinkedChatID()
			if !hasLinked || linked == 0 {
				t.Fatal("that channel has no linked_chat on the real server; the sample is stale")
			}
			peer, ok := res.Peer.(*tg.InputPeerChannel)
			if !ok {
				t.Fatalf("Peer type %T, want *tg.InputPeerChannel (the discussion group)", res.Peer)
			}
			if peer.ChannelID != linked {
				t.Fatalf("resolved peer = %d, real linked_chat_id = %d", peer.ChannelID, linked)
			}
			t.Logf("real result: main channel %d post %d -> discussion group %d, discussion message %d, %d media items",
				liveCommentMain, liveCommentPost, linked, res.MsgID, len(res.Items))
		})
	})

	t.Run("live-6 forum topic link, topic segment dropped", func(t *testing.T) {
		withLive(t, func(ctx context.Context, env *liveEnv) {
			// Shape t.me/c/<cid>/<topicID>/<msgID>: only the last segment may be taken as
			// the message ID.
			url := fmt.Sprintf("https://t.me/c/%d/%d/%d", liveForumChat, liveForumTopicID, liveForumMsgID)
			ref, err := ParseLink(url)
			if err != nil {
				t.Fatalf("ParseLink(%s): %v", url, err)
			}
			if ref.MsgID != liveForumMsgID {
				t.Fatalf("ParseLink MsgID = %d, want %d (the topic segment must be dropped)", ref.MsgID, liveForumMsgID)
			}
			if ref.Kind != RefPrivate {
				t.Fatalf("Kind = %v, want RefPrivate", ref.Kind)
			}
			// The message is plain text, so resolution should reach ErrNoMedia; as long as
			// it is not "message does not exist", the message ID was picked correctly (the
			// topic segment was not taken as the message ID).
			res, rerr := env.pipeline.ResolveLink(ctx, url)
			if rerr != nil {
				if errors.Is(rerr, ErrMessageDeleted) {
					t.Fatalf("wrong message ID (the topic segment was taken as the message): %v", rerr)
				}
				if !errors.Is(rerr, ErrNoMedia) {
					t.Fatalf("ResolveLink(%s): %v", url, rerr)
				}
				t.Logf("real result: MsgID=%d hits a real message, plain text -> %v", liveForumMsgID, rerr)
				return
			}
			assertResolved(t, res, liveForumMsgID)
			t.Logf("real result: MsgID=%d hits, %d media items", liveForumMsgID, len(res.Items))
		})
	})

	t.Run("live-7a deleted message, server returns an earlier one", func(t *testing.T) {
		withLive(t, func(ctx context.Context, env *liveEnv) {
			// Counter-check first: that ID really is deleted (the server returns an earlier
			// message).
			access := liveChannelAccess(t, ctx, env, livePrivateChannel)
			resp, herr := env.api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
				Peer:     &tg.InputPeerChannel{ChannelID: livePrivateChannel, AccessHash: access},
				OffsetID: liveDeletedByOffset + 1, Limit: 1,
			})
			if herr != nil {
				t.Fatalf("counter-check getHistory: %v", herr)
			}
			got := liveMessageIDs(resp)
			if len(got) == 0 || got[0] == liveDeletedByOffset {
				t.Fatalf("the sample is stale: expected the server to return an earlier message, got %v", got)
			}

			url := livePrivateURL(livePrivateChannel, liveDeletedByOffset)
			_, err := env.pipeline.ResolveLink(ctx, url)
			if err == nil {
				t.Fatalf("ResolveLink(%s) unexpectedly succeeded; a deleted message must not resolve", url)
			}
			if !errors.Is(err, ErrMessageDeleted) {
				t.Fatalf("err = %v, want ErrMessageDeleted", err)
			}
			t.Logf("real result: requested %d, the server returned %v -> %v", liveDeletedByOffset, got, err)
		})
	})

	t.Run("live-7b deleted message, server returns an empty result", func(t *testing.T) {
		withLive(t, func(ctx context.Context, env *liveEnv) {
			// Looking up msgID=1 (OffsetID=2) in a real channel almost certainly runs past
			// the earliest message, and the server returns an empty result instead of an
			// error -- that is the second branch of ErrMessageDeleted.
			const (
				emptyChannel = livePlainTextChannel
				emptyMsgID   = 1
			)
			access := liveChannelAccess(t, ctx, env, emptyChannel)
			resp, herr := env.api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
				Peer:     &tg.InputPeerChannel{ChannelID: emptyChannel, AccessHash: access},
				OffsetID: emptyMsgID + 1, Limit: 1,
			})
			if herr != nil {
				t.Fatalf("counter-check getHistory: %v", herr)
			}
			got := liveMessageIDs(resp)
			if len(got) != 0 {
				t.Fatalf("counter-check failed: expected an empty result, got %v (the channel's earliest message ID is already <= %d)", got, emptyMsgID)
			}

			url := fmt.Sprintf("https://t.me/LSPosed/%d", emptyMsgID)
			_, err := env.pipeline.ResolveLink(ctx, url)
			if err == nil {
				t.Fatalf("ResolveLink(%s) unexpectedly succeeded", url)
			}
			if !errors.Is(err, ErrMessageDeleted) {
				t.Fatalf("err = %v, want ErrMessageDeleted", err)
			}
			t.Logf("real result: requested %d, the server returned an empty result -> %v", emptyMsgID, err)
		})
	})

	t.Run("live-8 plain-text message, ErrNoMedia", func(t *testing.T) {
		withLive(t, func(ctx context.Context, env *liveEnv) {
			url := fmt.Sprintf("https://t.me/LSPosed/%d", livePlainTextMsg)
			_, err := env.pipeline.ResolveLink(ctx, url)
			if err == nil {
				t.Fatalf("ResolveLink(%s) unexpectedly succeeded; a plain-text message has no media", url)
			}
			if !errors.Is(err, ErrNoMedia) {
				t.Fatalf("err = %v, want ErrNoMedia (note: ErrMessageDeleted would mean the wrong message ID)", err)
			}
			t.Logf("real result: %v", err)
		})
	})

	t.Run("live-9 album boundary, maximum item count of a single album", func(t *testing.T) {
		withLive(t, func(ctx context.Context, env *liveEnv) {
			// Scan the dialog to find the longest album that exists in the real world and
			// verify the expansion limits are sufficient. Telegram's spec caps a single
			// album at 10 items; ExpandAlbum uses OffsetID=msgID+11, Limit=20.
			access := liveChannelAccess(t, ctx, env, liveAlbumChannel)
			resp, herr := env.api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
				Peer:  &tg.InputPeerChannel{ChannelID: liveAlbumChannel, AccessHash: access},
				Limit: 100,
			})
			if herr != nil {
				t.Fatalf("getHistory: %v", herr)
			}
			groups := map[int64][]int{}
			for _, m := range liveMessages(resp) {
				if m.GroupedID != 0 {
					groups[m.GroupedID] = append(groups[m.GroupedID], m.ID)
				}
			}
			if len(groups) == 0 {
				t.Skip("the channel's latest 100 messages contain no album sample (a SKIP is not a pass)")
			}
			// Take the longest group for the end-to-end verification.
			var bestGroup int64
			bestLen := 0
			for gid, ids := range groups {
				if len(ids) > bestLen {
					bestGroup, bestLen = gid, len(ids)
				}
			}
			ids := groups[bestGroup]
			sort.Ints(ids)
			anchor := ids[0]
			// The channel is public, so the username form is closer to a real paste.
			url := fmt.Sprintf("https://t.me/music_tag_web/%d", anchor)
			res, err := env.pipeline.ResolveLink(ctx, url)
			if err != nil {
				t.Fatalf("ResolveLink(%s): %v", url, err)
			}
			assertResolved(t, res, anchor)
			if len(res.Items) != bestLen {
				t.Fatalf("the longest album has %d items, resolution produced %d", bestLen, len(res.Items))
			}
			t.Logf("real result: longest album grouped=%d with %d items, anchor %d, expansion fully successful", bestGroup, bestLen, anchor)
		})
	})

	t.Run("live-10 service message, currently classified as ErrMessageDeleted (known limitation)", func(t *testing.T) {
		withLive(t, func(ctx context.Context, env *liveEnv) {
			// 38667 is a MessageService (a join notification), not "deleted".
			// The production code pipelineAllMessages drops non-*tg.Message entries, so
			// fetchMessage cannot find the message -> ErrMessageDeleted. This test pins the
			// **current real behavior** so that a future fix of the classification is
			// noticed (known limitation).
			access := liveChannelAccess(t, ctx, env, livePrivateChannel)
			resp, herr := env.api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
				Peer:     &tg.InputPeerChannel{ChannelID: livePrivateChannel, AccessHash: access},
				OffsetID: liveServiceMsg + 1, Limit: 1,
			})
			if herr != nil {
				t.Fatalf("counter-check getHistory: %v", herr)
			}
			if !liveHasServiceMessage(resp) {
				t.Skip("the sample is no longer a MessageService (a SKIP is not a pass)")
			}
			url := livePrivateURL(livePrivateChannel, liveServiceMsg)
			_, err := env.pipeline.ResolveLink(ctx, url)
			if !errors.Is(err, ErrMessageDeleted) {
				t.Fatalf("err = %v, want ErrMessageDeleted (the established behavior of the current implementation)", err)
			}
			t.Logf("real result: MessageService %d is reported as \"%v\" -- the message exists but is not a regular message, a known classification limitation",
				liveServiceMsg, err)
		})
	})

	t.Run("live-11 resolving the same message twice, stable file reference", func(t *testing.T) {
		withLive(t, func(ctx context.Context, env *liveEnv) {
			url := fmt.Sprintf("https://t.me/telegram/193")
			res1, err := env.pipeline.ResolveLink(ctx, url)
			if err != nil {
				t.Fatalf("first ResolveLink: %v", err)
			}
			res2, err := env.pipeline.ResolveLink(ctx, url)
			if err != nil {
				t.Fatalf("second ResolveLink: %v", err)
			}
			if len(res1.Items) != len(res2.Items) {
				t.Fatalf("the two resolutions produced different item counts: %d vs %d", len(res1.Items), len(res2.Items))
			}
			for i := range res1.Items {
				a, b := res1.Items[i], res2.Items[i]
				if a.MessageID != b.MessageID || a.Size != b.Size || a.Kind != b.Kind {
					t.Fatalf("Items[%d] differs between the two resolutions: %+v vs %+v", i, a, b)
				}
				if len(b.FileRef) == 0 {
					t.Fatalf("Items[%d].FileRef is empty on the second resolution", i)
				}
			}
			t.Logf("real result: both resolutions agree, %d items, every FileRef non-empty", len(res1.Items))
		})
	})
}
