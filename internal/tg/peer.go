// peer.go - resolve a parsed MessageRef into a usable tg.InputPeerClass.
//
// Responsibilities:
//   - PeerIDFromChannelID / ChannelIDFromPeerID: the -100 prefix conversion;
//   - PeerResolver.Resolve: RefPublic (ContactsResolveUsername),
//     RefPrivate (cache -> ChannelsGetChannels), RefComment (resolve the main
//     channel first, then ChannelsGetFullChannel to find the linked discussion group);
//   - access_hash cache: reuses internal/state.Store's UpsertEntity/LookupEntity,
//     Entity.Peer always stores the decimal string of the peer id, with a TTL
//     (6 hours by default);
//   - SyncDialogs: pages through messages.getDialogs and batch-writes the dialog
//     entities into the cache.
//
// Note: callers use ref.CommentID as the message ID to fetch (comments live in the
// discussion group), and Resolve returns the discussion group's InputPeerChannel.
package tg

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/1368129224/tgshelf/internal/state"
	"github.com/gotd/td/tg"
)

// PeerRPC is the minimal RPC subset peer resolution needs. *tg.Client satisfies it
// out of the box.
type PeerRPC interface {
	ContactsResolveUsername(ctx context.Context, request *tg.ContactsResolveUsernameRequest) (*tg.ContactsResolvedPeer, error)
	ChannelsGetChannels(ctx context.Context, id []tg.InputChannelClass) (tg.MessagesChatsClass, error)
	ChannelsGetFullChannel(ctx context.Context, channel tg.InputChannelClass) (*tg.MessagesChatFull, error)
	MessagesGetDialogs(ctx context.Context, request *tg.MessagesGetDialogsRequest) (tg.MessagesDialogsClass, error)
}

// Compile-time assertion: *tg.Client satisfies PeerRPC (gotd v0.163.0's
// *tg.Client forwards each of the four methods above, see tg/tl_client_gen.go and
// the individual *_gen.go files).
var _ PeerRPC = (*tg.Client)(nil)

// ErrChannelInaccessible is the sentinel returned when a private channel misses the
// cache and GetChannels fails. The caller decides on it with errors.Is.
var ErrChannelInaccessible = errors.New("cannot access the channel: join it first, then send /sync")

// peerIDChannelPrefix is the "-100" prefix base of a private channel peer id: -1e12.
// Concatenating "-100" and the channelID string corresponds to the arithmetic
// -(1e12 + channelID), i.e. peerID = -1_000_000_000_000 - channelID (contract
// correction: the original brief's 1e11 was one digit short).
const peerIDChannelPrefix = -1_000_000_000_000

// Entity kind constants private to this package (internal/state only has the
// EntityKind type, no constants).
const (
	peerKindUser    = state.EntityKind("user")
	peerKindChat    = state.EntityKind("chat")
	peerKindChannel = state.EntityKind("channel")
)

// Default cache TTL: 6 hours.
const peerDefaultTTL = 6 * time.Hour

// PeerIDFromChannelID converts the raw channel id of t.me/c/<id> into a peer id.
func PeerIDFromChannelID(channelID int64) int64 {
	return peerIDChannelPrefix - channelID
}

// ChannelIDFromPeerID is the inverse of PeerIDFromChannelID;
// a peer id without the "-100" prefix (a regular group's negative id, a user's
// positive id) yields a result <= 0, returning (0, false).
func ChannelIDFromPeerID(peerID int64) (int64, bool) {
	channelID := peerIDChannelPrefix - peerID
	if channelID <= 0 {
		return 0, false
	}
	return channelID, true
}

// peerResolverTimeNow is the injectable time source (for tests); time.Now by default.
type peerResolverTimeNow func() time.Time

// PeerResolver resolves MessageRef into an InputPeer and maintains the access_hash
// cache.
type PeerResolver struct {
	rpc PeerRPC
	st  *state.Store // nil means no-cache mode
	ttl time.Duration
	now peerResolverTimeNow
}

// NewPeerResolver builds a PeerResolver; st == nil means no-cache mode;
// ttl <= 0 falls back to the default 6 hours.
func NewPeerResolver(rpc PeerRPC, st *state.Store, ttl time.Duration) (*PeerResolver, error) {
	if rpc == nil {
		return nil, errors.New("tg: PeerRPC must not be nil")
	}
	if ttl <= 0 {
		ttl = peerDefaultTTL
	}
	return &PeerResolver{
		rpc: rpc,
		st:  st,
		ttl: ttl,
		now: time.Now,
	}, nil
}

// peerFresh reports whether a cached entity is still fresh.
func (r *PeerResolver) peerFresh(e state.Entity) bool {
	if r.st == nil || e.SyncedAt.IsZero() {
		return false
	}
	return r.peerNow().Sub(e.SyncedAt) < r.ttl
}

// peerLookup queries the cache (no-cache mode always misses).
func (r *PeerResolver) peerLookup(peerID int64) (state.Entity, bool) {
	if r.st == nil {
		return state.Entity{}, false
	}
	e, ok := r.st.LookupEntity(strconv.FormatInt(peerID, 10))
	if !ok || !r.peerFresh(e) {
		return state.Entity{}, false
	}
	return e, true
}

// peerSave writes the cache (no-cache mode ignores it).
func (r *PeerResolver) peerSave(peerID int64, kind state.EntityKind, hash int64, username, title string) {
	if r.st == nil {
		return
	}
	r.st.UpsertEntity(state.Entity{
		Peer:     strconv.FormatInt(peerID, 10),
		Hash:     hash,
		Kind:     kind,
		Username: username,
		Title:    title,
	})
}

// peerNow exists so tests can replace the time source.
func (r *PeerResolver) peerNow() time.Time {
	if r.now == nil {
		return time.Now()
	}
	return r.now()
}

// peerIDsFromResolved pulls (channelID, accessHash), (userID, accessHash) or
// (chatID) out of a ResolvedPeer's Peer + Chats + Users.
func peerIDsFromResolved(resolved *tg.ContactsResolvedPeer) (peerID int64, hash int64, ok bool) {
	if resolved == nil || resolved.Peer == nil {
		return 0, 0, false
	}
	switch p := resolved.Peer.(type) {
	case *tg.PeerChannel:
		peerID = PeerIDFromChannelID(p.ChannelID)
		for _, ch := range resolved.Chats {
			if c, is := ch.(*tg.Channel); is && c.ID == p.ChannelID {
				return peerID, c.AccessHash, true
			}
		}
		return peerID, 0, false
	case *tg.PeerUser:
		peerID = p.UserID
		for _, u := range resolved.Users {
			if usr, is := u.(*tg.User); is && usr.ID == p.UserID {
				return peerID, usr.AccessHash, true
			}
		}
		return peerID, 0, false
	case *tg.PeerChat:
		return p.ChatID, 0, true
	default:
		return 0, 0, false
	}
}

// Resolve turns a MessageRef into an InputPeer; the returned peer is the dialog the
// message must be fetched from (for RefComment it is the discussion group's
// InputPeerChannel, and callers use ref.CommentID as the message ID).
func (r *PeerResolver) Resolve(ctx context.Context, ref MessageRef) (tg.InputPeerClass, error) {
	switch ref.Kind {
	case RefPublic:
		return r.resolvePublic(ctx, ref)
	case RefPrivate:
		return r.resolveChannelByPeerID(ctx, ref.ChannelID)
	case RefComment:
		return r.resolveComment(ctx, ref)
	default:
		return nil, fmt.Errorf("tg: unsupported link shape Kind=%d", int(ref.Kind))
	}
}

// resolvePublic handles RefPublic: contacts.resolveUsername -> InputPeer plus a
// cache write.
func (r *PeerResolver) resolvePublic(ctx context.Context, ref MessageRef) (tg.InputPeerClass, error) {
	resolved, err := r.rpc.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: ref.Username})
	if err != nil {
		return nil, fmt.Errorf("tg: resolve username %q: %w", ref.Username, err)
	}
	peerID, hash, ok := peerIDsFromResolved(resolved)
	if !ok {
		return nil, fmt.Errorf("tg: resolved username %q carries no entity information", ref.Username)
	}
	switch p := resolved.Peer.(type) {
	case *tg.PeerChannel:
		r.peerSave(peerID, peerKindChannel, hash, ref.Username, peerChannelTitle(resolved))
		return &tg.InputPeerChannel{ChannelID: p.ChannelID, AccessHash: hash}, nil
	case *tg.PeerUser:
		r.peerSave(peerID, peerKindUser, hash, ref.Username, "")
		return &tg.InputPeerUser{UserID: p.UserID, AccessHash: hash}, nil
	case *tg.PeerChat:
		r.peerSave(peerID, peerKindChat, 0, "", peerChannelTitle(resolved))
		return &tg.InputPeerChat{ChatID: p.ChatID}, nil
	default:
		return nil, fmt.Errorf("tg: username %q resolved to an unknown peer type %T", ref.Username, resolved.Peer)
	}
}

// peerChannelTitle takes the channel/group title from ResolvedPeer.Chats (best effort).
func peerChannelTitle(resolved *tg.ContactsResolvedPeer) string {
	for _, ch := range resolved.Chats {
		switch c := ch.(type) {
		case *tg.Channel:
			return c.Title
		case *tg.Chat:
			return c.Title
		}
	}
	return ""
}

// resolveChannelByPeerID handles RefPrivate: a cache hit returns immediately (zero
// RPC); a miss tries ChannelsGetChannels once and wraps failures in
// ErrChannelInaccessible.
func (r *PeerResolver) resolveChannelByPeerID(ctx context.Context, channelID int64) (tg.InputPeerClass, error) {
	peerID := PeerIDFromChannelID(channelID)
	if e, ok := r.peerLookup(peerID); ok {
		// Cache hit: no RPC at all.
		return &tg.InputPeerChannel{ChannelID: channelID, AccessHash: e.Hash}, nil
	}
	chats, err := r.rpc.ChannelsGetChannels(ctx, []tg.InputChannelClass{&tg.InputChannel{ChannelID: channelID, AccessHash: 0}})
	if err != nil {
		return nil, fmt.Errorf("tg: cannot get access_hash of channel %d: %w: %w", channelID, ErrChannelInaccessible, err)
	}
	// Find the channel in the successful response and cache its access_hash.
	hash, found := peerHashFromChats(chats, channelID)
	if !found {
		return nil, fmt.Errorf("tg: GetChannels response is missing channel %d: %w", channelID, ErrChannelInaccessible)
	}
	r.peerSave(peerID, peerKindChannel, hash, "", "")
	return &tg.InputPeerChannel{ChannelID: channelID, AccessHash: hash}, nil
}

// peerHashFromChats finds the access_hash of the given channel in a
// ChannelsGetChannels response. The response type is MessagesChatsClass (both chats
// and chatsSlice).
func peerHashFromChats(chats tg.MessagesChatsClass, channelID int64) (int64, bool) {
	var list []tg.ChatClass
	switch c := chats.(type) {
	case *tg.MessagesChats:
		list = c.Chats
	case *tg.MessagesChatsSlice:
		list = c.Chats
	default:
		return 0, false
	}
	for _, ch := range list {
		if channel, is := ch.(*tg.Channel); is && channel.ID == channelID {
			return channel.AccessHash, true
		}
	}
	return 0, false
}

// resolveComment handles RefComment: resolve the main channel first, then fetch the
// linked discussion group.
func (r *PeerResolver) resolveComment(ctx context.Context, ref MessageRef) (tg.InputPeerClass, error) {
	// Step one: resolve the main channel. RefComment keeps the main channel in
	// Username/ChannelID.
	var mainPeer tg.InputPeerClass
	var err error
	if ref.Username != "" {
		mainPeer, err = r.resolvePublic(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("tg: resolve comment main channel: %w", err)
		}
	} else {
		mainPeer, err = r.resolveChannelByPeerID(ctx, ref.ChannelID)
		if err != nil {
			return nil, fmt.Errorf("tg: resolve comment main channel: %w", err)
		}
	}
	mainChannel, is := mainPeer.(*tg.InputPeerChannel)
	if !is {
		return nil, errors.New("tg: the main channel of a comment link is not a channel, cannot resolve the discussion group")
	}

	// Step two: channels.getFullChannel to read LinkedChatID.
	full, err := r.rpc.ChannelsGetFullChannel(ctx, &tg.InputChannel{ChannelID: mainChannel.ChannelID, AccessHash: mainChannel.AccessHash})
	if err != nil {
		return nil, fmt.Errorf("tg: get full info of the main channel: %w", err)
	}
	// channels.getFullChannel actually returns channelFull (tg.ChannelFull), and
	// LinkedChatID is one of its conditional fields (verified: tg/tl_chat_full_gen.go:1326-1333, 3879).
	channelFull, is := full.FullChat.(*tg.ChannelFull)
	if !is {
		return nil, errors.New("tg: getFullChannel response has an unexpected type")
	}
	linkedChatID, _ := channelFull.GetLinkedChatID()
	if linkedChatID == 0 {
		return nil, errors.New("tg: this channel has no linked discussion group, cannot resolve the comment link")
	}

	// Step three: take the linked chat id down the private channel path to get its
	// access_hash and return the discussion group peer.
	return r.resolveChannelByPeerID(ctx, linkedChatID)
}

// SyncDialogs syncs the dialog list into the entities cache and returns the number
// of entities written or updated.
// full=true pages through everything (at most 50 pages), false only pulls the most
// recent page as an increment.
func (r *PeerResolver) SyncDialogs(ctx context.Context, full bool) (int, error) {
	const (
		pageLimit = 100 // messages per page
		maxPages  = 50  // full paging cap; stop when reached
	)
	total := 0
	offsetDate := 0
	offsetID := 0
	var offsetPeer tg.InputPeerClass = &tg.InputPeerEmpty{}

	for page := 0; ; page++ {
		if full && page >= maxPages {
			break
		}
		if !full && page >= 1 {
			break
		}
		dialogs, err := r.rpc.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
			OffsetDate: offsetDate,
			OffsetID:   offsetID,
			OffsetPeer: offsetPeer,
			Limit:      pageLimit,
		})
		if err != nil {
			return total, fmt.Errorf("tg: messages.getDialogs: %w", err)
		}
		count := r.peerCacheDialogsPage(dialogs)
		total += count
		// Build the offset for the next page.
		nextDate, nextID, nextPeer, hasMore := peerNextOffset(dialogs)
		if !hasMore {
			break
		}
		offsetDate, offsetID, offsetPeer = nextDate, nextID, nextPeer
	}
	return total, nil
}

// peerCacheDialogsPage writes the chats/users of one dialogs page into the cache and
// returns the number written.
func (r *PeerResolver) peerCacheDialogsPage(dialogs tg.MessagesDialogsClass) int {
	var (
		chatsList []tg.ChatClass
		usersList []tg.UserClass
	)
	switch d := dialogs.(type) {
	case *tg.MessagesDialogs:
		chatsList, usersList = d.Chats, d.Users
	case *tg.MessagesDialogsSlice:
		chatsList, usersList = d.Chats, d.Users
	default:
		return 0
	}
	count := 0
	for _, ch := range chatsList {
		switch c := ch.(type) {
		case *tg.Channel:
			r.peerSave(PeerIDFromChannelID(c.ID), peerKindChannel, c.AccessHash, "", c.Title)
			count++
		case *tg.Chat:
			r.peerSave(c.ID, peerKindChat, 0, "", c.Title)
			count++
		}
	}
	for _, u := range usersList {
		if usr, is := u.(*tg.User); is {
			r.peerSave(usr.ID, peerKindUser, usr.AccessHash, usr.Username, "")
			count++
		}
	}
	return count
}

// peerNextOffset takes the last dialog of a page and builds the OffsetDate /
// OffsetID / OffsetPeer of the next page; it returns hasMore=false for an empty page
// or one that is not full.
func peerNextOffset(dialogs tg.MessagesDialogsClass) (date, id int, peer tg.InputPeerClass, hasMore bool) {
	var (
		dialogsList []tg.DialogClass
		messages    []tg.MessageClass
		count       int
	)
	switch d := dialogs.(type) {
	case *tg.MessagesDialogs:
		dialogsList, messages = d.Dialogs, d.Messages
	case *tg.MessagesDialogsSlice:
		dialogsList, messages, count = d.Dialogs, d.Messages, d.Count
	default:
		return 0, 0, nil, false
	}
	if len(dialogsList) == 0 {
		return 0, 0, nil, false
	}
	// Telegram convention: a page that is not full (< 100 entries) is the last one.
	if len(dialogsList) < 100 {
		return 0, 0, nil, false
	}
	_ = count // MessagesDialogs (the full form) has no count field; judge by page size
	last := dialogsList[len(dialogsList)-1]
	dlg, is := last.(*tg.Dialog)
	if !is || dlg.Peer == nil {
		return 0, 0, nil, false
	}
	// OffsetID / OffsetDate come from the newest message of the last dialog.
	for i := len(messages) - 1; i >= 0; i-- {
		if msg, isMsg := messages[i].(*tg.Message); isMsg {
			date = msg.Date
			id = msg.ID
			break
		}
	}
	peer = peerInputFromPeerClass(dlg.Peer)
	return date, id, peer, true
}

// peerInputFromPeerClass converts a PeerClass into the InputPeerClass used for paging.
func peerInputFromPeerClass(p tg.PeerClass) tg.InputPeerClass {
	switch pp := p.(type) {
	case *tg.PeerChannel:
		return &tg.InputPeerChannel{ChannelID: pp.ChannelID}
	case *tg.PeerUser:
		return &tg.InputPeerUser{UserID: pp.UserID}
	case *tg.PeerChat:
		return &tg.InputPeerChat{ChatID: pp.ChatID}
	default:
		return &tg.InputPeerEmpty{}
	}
}
