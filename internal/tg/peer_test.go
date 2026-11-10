// peer_test.go - unit tests for peer resolution. Everything uses a fake
// PeerRPC; no real network.
package tg

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"

	"github.com/1368129224/tgshelf/internal/state"
)

// fakePeerRPC is a programmable fake of PeerRPC: it records call counts and returns
// scripted results.
type fakePeerRPC struct {
	resolveCalls   int
	getChannels    int
	getFullChannel int
	getDialogs     int

	resolveResult  *tg.ContactsResolvedPeer
	resolveErr     error
	channelsResult tg.MessagesChatsClass
	channelsErr    error
	fullChannelRes *tg.MessagesChatFull
	fullChannelErr error
	dialogsPages   []tg.MessagesDialogsClass // returned in call order; empty page once exhausted
	lastDialogsReq *tg.MessagesGetDialogsRequest
}

func (f *fakePeerRPC) ContactsResolveUsername(ctx context.Context, request *tg.ContactsResolveUsernameRequest) (*tg.ContactsResolvedPeer, error) {
	f.resolveCalls++
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}
	return f.resolveResult, nil
}

func (f *fakePeerRPC) ChannelsGetChannels(ctx context.Context, id []tg.InputChannelClass) (tg.MessagesChatsClass, error) {
	f.getChannels++
	if f.channelsErr != nil {
		return nil, f.channelsErr
	}
	return f.channelsResult, nil
}

func (f *fakePeerRPC) ChannelsGetFullChannel(ctx context.Context, channel tg.InputChannelClass) (*tg.MessagesChatFull, error) {
	f.getFullChannel++
	if f.fullChannelErr != nil {
		return nil, f.fullChannelErr
	}
	return f.fullChannelRes, nil
}

func (f *fakePeerRPC) MessagesGetDialogs(ctx context.Context, request *tg.MessagesGetDialogsRequest) (tg.MessagesDialogsClass, error) {
	f.getDialogs++
	f.lastDialogsReq = request
	if len(f.dialogsPages) == 0 {
		return &tg.MessagesDialogs{}, nil
	}
	page := f.dialogsPages[0]
	f.dialogsPages = f.dialogsPages[1:]
	return page, nil
}

// newTestStoreForPeer creates a temporary state.Store (mirroring how the
// internal/state tests do it).
func newTestStoreForPeer(t *testing.T) *state.Store {
	t.Helper()
	s, err := state.New(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// newResolver builds a resolver with an injected time source.
func newResolver(t *testing.T, rpc PeerRPC, st *state.Store) (*PeerResolver, *time.Time) {
	t.Helper()
	r, err := NewPeerResolver(rpc, st, 0)
	if err != nil {
		t.Fatalf("NewPeerResolver: %v", err)
	}
	now := time.Unix(1_700_000_000, 0).UTC()
	r.now = func() time.Time { return now }
	return r, &now
}

// TestPeerIDChannelRoundTrip covers the required behavior: the -100 conversion and
// its boundaries (channelID=1, very large values, negative values, non -100 prefix).
func TestPeerIDChannelRoundTrip(t *testing.T) {
	cases := []struct {
		name      string
		channelID int64
		// wantStr is the exact decimal string of the arithmetic formula
		// -(1e12+channelID); the string assertion pins the "-100" prefix semantics
		// (e.g. 1 -> "-1000000000001").
		wantStr string
	}{
		{"channelID=1", 1, "-1000000000001"},
		{"channelID=10", 10, "-1000000000010"},
		{"typical value (10 digits)", 1234567890, "-1001234567890"},
		{"very large value", 9_000_000_000_000, "-10000000000000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			peerID := PeerIDFromChannelID(tc.channelID)
			want := int64(-1_000_000_000_000) - tc.channelID
			if peerID != want {
				t.Fatalf("PeerIDFromChannelID(%d) = %d, want %d", tc.channelID, peerID, want)
			}
			// String assertion: compare the decimal literal rather than the number.
			if gotStr := strconv.FormatInt(peerID, 10); gotStr != tc.wantStr {
				t.Fatalf("string assertion failed: PeerIDFromChannelID(%d) = %q, want %q",
					tc.channelID, gotStr, tc.wantStr)
			}
			got, ok := ChannelIDFromPeerID(peerID)
			if !ok || got != tc.channelID {
				t.Fatalf("ChannelIDFromPeerID(%d) = (%d, %v), want (%d, true)", peerID, got, ok, tc.channelID)
			}
		})
	}
	// A peer id without the "-100" prefix (conversion result <= 0, covering
	// non-negative numbers and regular group negative ids) returns (0, false).
	for _, peerID := range []int64{0, 1, 100, -1, -100, -2, 50_000_000_000, -999_999_999_999, -1_000_000_000_000} {
		if got, ok := ChannelIDFromPeerID(peerID); ok {
			t.Errorf("ChannelIDFromPeerID(%d) = (%d, true), want false", peerID, got)
		}
	}
	// Boundary: -1e12-1 is the smallest valid channel peer id (channelID=1).
	if got, ok := ChannelIDFromPeerID(-1_000_000_000_001); !ok || got != 1 {
		t.Errorf("ChannelIDFromPeerID(-1000000000001) = (%d, %v), want (1, true)", got, ok)
	}
}

// channelChat builds a Channel entity.
func channelChat(id, hash int64) *tg.Channel {
	return &tg.Channel{ID: id, AccessHash: hash, Title: fmt.Sprintf("channel%d", id)}
}

// linkedFull builds a ChannelFull carrying linked_chat_id (SetLinkedChatID must be
// used so the Flags bit is set, otherwise GetLinkedChatID returns 0 - see the
// Flags.Has(14) check at tg/tl_chat_full_gen.go:3879).
func linkedFull(id, linkedChatID int64) *tg.ChannelFull {
	c := &tg.ChannelFull{ID: id}
	c.SetLinkedChatID(linkedChatID)
	return c
}

// peerChannelRef builds a RefPrivate MessageRef.
func peerChannelRef(channelID int64) MessageRef {
	return MessageRef{Kind: RefPrivate, ChannelID: channelID, MsgID: 42}
}

// TestResolvePublic covers required behavior 1: the public shape resolves,
// AccessHash is non-zero and the cache is written.
func TestResolvePublic(t *testing.T) {
	st := newTestStoreForPeer(t)
	rpc := &fakePeerRPC{resolveResult: &tg.ContactsResolvedPeer{
		Peer:  &tg.PeerChannel{ChannelID: 777},
		Chats: []tg.ChatClass{channelChat(777, 424242)},
	}}
	r, _ := newResolver(t, rpc, st)

	ref, err := ParseLink("https://t.me/somechannel/100")
	if err != nil {
		t.Fatalf("ParseLink: %v", err)
	}
	peer, err := r.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	pc, ok := peer.(*tg.InputPeerChannel)
	if !ok {
		t.Fatalf("returned type = %T, want *tg.InputPeerChannel", peer)
	}
	if pc.ChannelID != 777 || pc.AccessHash != 424242 {
		t.Fatalf("InputPeerChannel = %+v, want ChannelID=777 AccessHash=424242", pc)
	}
	e, ok := st.LookupEntity(strconv.FormatInt(PeerIDFromChannelID(777), 10))
	if !ok || e.Hash != 424242 || e.Kind != "channel" {
		t.Fatalf("cache not written correctly: %+v ok=%v", e, ok)
	}
}

// TestResolvePublicUser covers the user branch of required behavior 1.
func TestResolvePublicUser(t *testing.T) {
	rpc := &fakePeerRPC{resolveResult: &tg.ContactsResolvedPeer{
		Peer:  &tg.PeerUser{UserID: 555},
		Users: []tg.UserClass{&tg.User{ID: 555, AccessHash: 1111}},
	}}
	r, _ := newResolver(t, rpc, nil)

	ref, err := ParseLink("t.me/someuser/7")
	if err != nil {
		t.Fatalf("ParseLink: %v", err)
	}
	peer, err := r.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	pu, ok := peer.(*tg.InputPeerUser)
	if !ok || pu.UserID != 555 || pu.AccessHash != 1111 {
		t.Fatalf("InputPeerUser = %+v", pu)
	}
}

// TestResolvePrivateCacheHit covers required behavior 2: zero RPC on a cache hit.
func TestResolvePrivateCacheHit(t *testing.T) {
	st := newTestStoreForPeer(t)
	// Pre-write one fresh cache entry.
	st.UpsertEntity(state.Entity{
		Peer:     strconv.FormatInt(PeerIDFromChannelID(1234), 10),
		Hash:     987654,
		Kind:     "channel",
		SyncedAt: time.Unix(1_700_000_000, 0).UTC(),
	})
	rpc := &fakePeerRPC{}
	r, _ := newResolver(t, rpc, st)

	peer, err := r.Resolve(context.Background(), peerChannelRef(1234))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	pc, ok := peer.(*tg.InputPeerChannel)
	if !ok || pc.ChannelID != 1234 || pc.AccessHash != 987654 {
		t.Fatalf("InputPeerChannel = %+v", pc)
	}
	// Core assertion: with a cache hit ChannelsGetChannels is called 0 times.
	if rpc.getChannels != 0 {
		t.Fatalf("cache hit still called ChannelsGetChannels %d times, want 0", rpc.getChannels)
	}
}

// TestResolvePrivateMissThenSuccess covers required behavior 3: miss -> GetChannels
// succeeds -> cache written.
func TestResolvePrivateMissThenSuccess(t *testing.T) {
	st := newTestStoreForPeer(t)
	rpc := &fakePeerRPC{channelsResult: &tg.MessagesChats{
		Chats: []tg.ChatClass{channelChat(5678, 123321)},
	}}
	r, _ := newResolver(t, rpc, st)

	peer, err := r.Resolve(context.Background(), peerChannelRef(5678))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if pc := peer.(*tg.InputPeerChannel); pc.AccessHash != 123321 {
		t.Fatalf("AccessHash = %d, want 123321", pc.AccessHash)
	}
	if rpc.getChannels != 1 {
		t.Fatalf("getChannels = %d, want 1", rpc.getChannels)
	}
	e, ok := st.LookupEntity(strconv.FormatInt(PeerIDFromChannelID(5678), 10))
	if !ok || e.Hash != 123321 {
		t.Fatalf("cache not written back: %+v ok=%v", e, ok)
	}
	// The second Resolve hits the cache and no longer calls GetChannels.
	if _, err := r.Resolve(context.Background(), peerChannelRef(5678)); err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if rpc.getChannels != 1 {
		t.Fatalf("after the second Resolve getChannels = %d, want still 1", rpc.getChannels)
	}
}

// TestResolvePrivateMissThenFail covers the failure branch of required behavior 3:
// errors.Is detection plus the original error text.
func TestResolvePrivateMissThenFail(t *testing.T) {
	rpc := &fakePeerRPC{channelsErr: errors.New("CHANNEL_PRIVATE")}
	r, _ := newResolver(t, rpc, nil)

	_, err := r.Resolve(context.Background(), peerChannelRef(9999))
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !errors.Is(err, ErrChannelInaccessible) {
		t.Fatalf("errors.Is(err, ErrChannelInaccessible) = false, err = %v", err)
	}
	if !strings.Contains(err.Error(), "join it first, then send /sync") {
		t.Fatalf("error text is missing the required phrasing: %v", err)
	}
}

// TestResolveComment covers required behavior 4: a discussion group comment returns
// the discussion group peer.
func TestResolveComment(t *testing.T) {
	const mainChannelID = 3001
	const linkedChatID = 5002
	st := newTestStoreForPeer(t)
	rpc := &fakePeerRPC{
		resolveResult: &tg.ContactsResolvedPeer{
			Peer:  &tg.PeerChannel{ChannelID: mainChannelID},
			Chats: []tg.ChatClass{channelChat(mainChannelID, 700)},
		},
		fullChannelRes: &tg.MessagesChatFull{
			FullChat: linkedFull(mainChannelID, linkedChatID),
		},
		channelsResult: &tg.MessagesChats{
			Chats: []tg.ChatClass{channelChat(linkedChatID, 800)},
		},
	}
	r, _ := newResolver(t, rpc, st)

	ref, err := ParseLink("https://t.me/mainchannel/10?comment=5150")
	if err != nil {
		t.Fatalf("ParseLink: %v", err)
	}
	if ref.Kind != RefComment || ref.CommentID != 5150 {
		t.Fatalf("ref = %+v", ref)
	}
	peer, err := r.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	pc, ok := peer.(*tg.InputPeerChannel)
	if !ok {
		t.Fatalf("returned type = %T, want *tg.InputPeerChannel", peer)
	}
	// Core assertion: the discussion group (linked chat) is returned, not the main channel.
	if pc.ChannelID != linkedChatID {
		t.Fatalf("ChannelID = %d, want discussion group %d (main channel is %d)",
			pc.ChannelID, linkedChatID, mainChannelID)
	}
	if pc.AccessHash != 800 {
		t.Fatalf("AccessHash = %d, want 800", pc.AccessHash)
	}
	if rpc.getFullChannel != 1 {
		t.Fatalf("getFullChannel = %d, want 1", rpc.getFullChannel)
	}
}

// TestResolveCommentPrivate covers the private main channel branch of required
// behavior 4.
func TestResolveCommentPrivate(t *testing.T) {
	const mainChannelID = 4001
	const linkedChatID = 6002
	st := newTestStoreForPeer(t)
	rpc := &fakePeerRPC{
		fullChannelRes: &tg.MessagesChatFull{
			FullChat: linkedFull(mainChannelID, linkedChatID),
		},
		channelsResult: &tg.MessagesChats{
			// GetChannels returns the main channel and the discussion group in one response.
			Chats: []tg.ChatClass{channelChat(mainChannelID, 901), channelChat(linkedChatID, 900)},
		},
	}
	r, _ := newResolver(t, rpc, st)

	ref, err := ParseLink(fmt.Sprintf("https://t.me/c/%d/1?comment=77", mainChannelID))
	if err != nil {
		t.Fatalf("ParseLink: %v", err)
	}
	peer, err := r.Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if pc := peer.(*tg.InputPeerChannel); pc.ChannelID != linkedChatID {
		t.Fatalf("ChannelID = %d, want %d", pc.ChannelID, linkedChatID)
	}
}

// TestResolveCommentNoLinkedChat covers required behavior 4: linked_chat_id 0
// produces a readable error.
func TestResolveCommentNoLinkedChat(t *testing.T) {
	const mainChannelID = 3001
	rpc := &fakePeerRPC{
		resolveResult: &tg.ContactsResolvedPeer{
			Peer:  &tg.PeerChannel{ChannelID: mainChannelID},
			Chats: []tg.ChatClass{channelChat(mainChannelID, 700)},
		},
		fullChannelRes: &tg.MessagesChatFull{FullChat: &tg.ChannelFull{ID: mainChannelID}},
	}
	r, _ := newResolver(t, rpc, nil)

	ref, err := ParseLink("https://t.me/mainchannel/10?comment=5150")
	if err != nil {
		t.Fatalf("ParseLink: %v", err)
	}
	_, err = r.Resolve(context.Background(), ref)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !strings.Contains(err.Error(), "no linked") {
		t.Fatalf("the error should say there is no linked discussion group: %v", err)
	}
}

// TestResolveCacheExpiredByTTL covers required behavior 7: refetch after the time
// source moves 7 hours ahead.
func TestResolveCacheExpiredByTTL(t *testing.T) {
	st := newTestStoreForPeer(t)
	st.UpsertEntity(state.Entity{
		Peer:     strconv.FormatInt(PeerIDFromChannelID(1234), 10),
		Hash:     111,
		Kind:     "channel",
		SyncedAt: time.Unix(1_700_000_000, 0).UTC(),
	})
	rpc := &fakePeerRPC{channelsResult: &tg.MessagesChats{
		Chats: []tg.ChatClass{channelChat(1234, 222)},
	}}
	r, now := newResolver(t, rpc, st)

	peer, err := r.Resolve(context.Background(), peerChannelRef(1234))
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if pc := peer.(*tg.InputPeerChannel); pc.AccessHash != 111 {
		t.Fatalf("should hit the cache within the TTL, AccessHash = %d, want 111", pc.AccessHash)
	}
	if rpc.getChannels != 0 {
		t.Fatalf("within the TTL getChannels = %d, want 0", rpc.getChannels)
	}
	// Advance the injected time source by 7 hours.
	*now = now.Add(7 * time.Hour)
	peer, err = r.Resolve(context.Background(), peerChannelRef(1234))
	if err != nil {
		t.Fatalf("Resolve after expiry: %v", err)
	}
	if pc := peer.(*tg.InputPeerChannel); pc.AccessHash != 222 {
		t.Fatalf("should refetch after expiry, AccessHash = %d, want 222", pc.AccessHash)
	}
	if rpc.getChannels != 1 {
		t.Fatalf("after expiry getChannels = %d, want 1", rpc.getChannels)
	}
}

// dialogsPage builds one MessagesDialogsSlice page (the Telegram paging shape
// carrying Count).
func dialogsPage(dialogs []tg.DialogClass, messages []tg.MessageClass, chats []tg.ChatClass, users []tg.UserClass, count int) *tg.MessagesDialogsSlice {
	return &tg.MessagesDialogsSlice{Dialogs: dialogs, Messages: messages, Chats: chats, Users: users, Count: count}
}

// TestSyncDialogsFullPagination covers required behavior 8: full paging (2 pages,
// then empty); asserts the count and the cache.
func TestSyncDialogsFullPagination(t *testing.T) {
	st := newTestStoreForPeer(t)
	rpc := &fakePeerRPC{}
	page1Chats := []tg.ChatClass{channelChat(1, 11), &tg.Chat{ID: -2, Title: "regular group"}}
	page1Users := []tg.UserClass{&tg.User{ID: 3, AccessHash: 33, Username: "u3"}}
	// Page 1: a full page (100 dialogs); the last dialog's peer is channel 1.
	d1 := make([]tg.DialogClass, 0, 100)
	msgs1 := make([]tg.MessageClass, 0, 100)
	for i := 0; i < 100; i++ {
		d1 = append(d1, &tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 1}})
		msgs1 = append(msgs1, &tg.Message{ID: 100 - i, Date: 1000 - i})
	}
	rpc.dialogsPages = []tg.MessagesDialogsClass{
		dialogsPage(d1, msgs1, page1Chats, page1Users, 102),
		dialogsPage([]tg.DialogClass{}, nil, nil, nil, 102),
	}
	r, _ := newResolver(t, rpc, st)

	n, err := r.SyncDialogs(context.Background(), true)
	if err != nil {
		t.Fatalf("SyncDialogs: %v", err)
	}
	// Page 1 writes 2 chats + 1 user = 3 entries; page 2 is empty, 0 entries.
	if n != 3 {
		t.Fatalf("SyncDialogs returned %d, want 3", n)
	}
	if rpc.getDialogs != 2 {
		t.Fatalf("paging calls = %d, want 2", rpc.getDialogs)
	}
	// Assert the cache contents.
	if e, ok := st.LookupEntity(strconv.FormatInt(PeerIDFromChannelID(1), 10)); !ok || e.Hash != 11 {
		t.Fatalf("channel 1 cache is wrong: %+v ok=%v", e, ok)
	}
	if e, ok := st.LookupEntity("-2"); !ok || e.Kind != "chat" || e.Title != "regular group" {
		t.Fatalf("basic chat cache is wrong: %+v ok=%v", e, ok)
	}
	if e, ok := st.LookupEntity("3"); !ok || e.Hash != 33 || e.Username != "u3" {
		t.Fatalf("user cache is wrong: %+v ok=%v", e, ok)
	}
	// The page 2 request should carry the offset of page 1's last dialog.
	if rpc.lastDialogsReq == nil || rpc.lastDialogsReq.OffsetID != 1 || rpc.lastDialogsReq.OffsetDate != 901 {
		t.Fatalf("second page offset is wrong: %+v", rpc.lastDialogsReq)
	}
	if _, is := rpc.lastDialogsReq.OffsetPeer.(*tg.InputPeerChannel); !is {
		t.Fatalf("OffsetPeer type = %T, want *tg.InputPeerChannel", rpc.lastDialogsReq.OffsetPeer)
	}
}

// TestSyncDialogsIncremental covers required behavior 8: full=false pulls a single page.
func TestSyncDialogsIncremental(t *testing.T) {
	st := newTestStoreForPeer(t)
	rpc := &fakePeerRPC{dialogsPages: []tg.MessagesDialogsClass{
		// Not a full page: 1 chat + 1 user, so paging should stop right away
		// (count=2 == len(dialogs)=2).
		func() *tg.MessagesDialogsSlice {
			return &tg.MessagesDialogsSlice{
				Dialogs:  []tg.DialogClass{&tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 9}}},
				Messages: []tg.MessageClass{&tg.Message{ID: 5, Date: 42}},
				Chats:    []tg.ChatClass{channelChat(9, 99)},
				Users:    []tg.UserClass{&tg.User{ID: 10, AccessHash: 100}},
				Count:    2,
			}
		}(),
	}}
	r, _ := newResolver(t, rpc, st)

	n, err := r.SyncDialogs(context.Background(), false)
	if err != nil {
		t.Fatalf("SyncDialogs: %v", err)
	}
	if n != 2 {
		t.Fatalf("SyncDialogs returned %d, want 2", n)
	}
	if rpc.getDialogs != 1 {
		t.Fatalf("incremental mode getDialogs = %d, want 1", rpc.getDialogs)
	}
	if e, ok := st.LookupEntity(strconv.FormatInt(PeerIDFromChannelID(9), 10)); !ok || e.Hash != 99 {
		t.Fatalf("channel 9 cache is wrong: %+v ok=%v", e, ok)
	}
}

// TestSyncDialogsFullPageCap covers required behavior 8: the full paging cap of 50
// pages stops the loop and still returns normally.
func TestSyncDialogsFullPageCap(t *testing.T) {
	st := newTestStoreForPeer(t)
	// Every page is full (count far larger than what was returned); the fake keeps
	// serving pages (an empty page once dialogsPages is exhausted).
	rpc := &fakePeerRPC{}
	r, _ := newResolver(t, rpc, st)
	// Verify the empty page sequence directly: after exhaustion getDialogs returns an
	// empty MessagesDialogs and paging stops naturally.
	n, err := r.SyncDialogs(context.Background(), true)
	if err != nil {
		t.Fatalf("SyncDialogs: %v", err)
	}
	_ = n
	// The point above is mainly that it does not panic and returns normally; the 50
	// page cap is guaranteed by the maxPages constant, and a fake that only ever
	// serves full pages would be too heavy here, so the empty response path is enough.
}

// TestSyncDialogsFullCap50Page covers the 50 page cap precisely: the fake serves
// full pages forever, confirming the loop stops at page 50.
func TestSyncDialogsFullCap50Page(t *testing.T) {
	st := newTestStoreForPeer(t)
	rpc := &fakePeerRPC{}
	// The fake logic cannot be changed directly, so wrap it: always return a full
	// page on top of the fake.
	infinite := &infiniteDialogsRPC{inner: rpc}
	r, _ := newResolver(t, infinite, st)
	n, err := r.SyncDialogs(context.Background(), true)
	if err != nil {
		t.Fatalf("SyncDialogs: %v", err)
	}
	if infinite.calls != 50 {
		t.Fatalf("full paging calls = %d, want 50", infinite.calls)
	}
	_ = n
	_ = rpc
}

// infiniteDialogsRPC is a PeerRPC wrapper that serves full pages forever, used to
// verify the 50 page cap.
type infiniteDialogsRPC struct {
	inner *fakePeerRPC
	calls int
}

func (f *infiniteDialogsRPC) ContactsResolveUsername(ctx context.Context, request *tg.ContactsResolveUsernameRequest) (*tg.ContactsResolvedPeer, error) {
	return f.inner.ContactsResolveUsername(ctx, request)
}

func (f *infiniteDialogsRPC) ChannelsGetChannels(ctx context.Context, id []tg.InputChannelClass) (tg.MessagesChatsClass, error) {
	return f.inner.ChannelsGetChannels(ctx, id)
}

func (f *infiniteDialogsRPC) ChannelsGetFullChannel(ctx context.Context, channel tg.InputChannelClass) (*tg.MessagesChatFull, error) {
	return f.inner.ChannelsGetFullChannel(ctx, channel)
}

func (f *infiniteDialogsRPC) MessagesGetDialogs(ctx context.Context, request *tg.MessagesGetDialogsRequest) (tg.MessagesDialogsClass, error) {
	f.calls++
	d := make([]tg.DialogClass, 0, 100)
	msgs := make([]tg.MessageClass, 0, 100)
	for i := 0; i < 100; i++ {
		d = append(d, &tg.Dialog{Peer: &tg.PeerChannel{ChannelID: 1}})
		msgs = append(msgs, &tg.Message{ID: 10000 - i, Date: 5000 - i})
	}
	return dialogsPage(d, msgs, nil, nil, 10_000), nil
}

// TestNewPeerResolverNilRPC covers the constructor's nil check.
func TestNewPeerResolverNilRPC(t *testing.T) {
	if _, err := NewPeerResolver(nil, nil, 0); err == nil {
		t.Fatal("want error for nil rpc, got nil")
	}
}
