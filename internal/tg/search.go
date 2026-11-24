// search.go - keyword search over a channel's media messages (messages.search + photo/video
// filter).
//
// Scope: resolve the channel argument -> server-side search with InputMessagesFilterPhotoVideo ->
// paginate with OffsetID -> expand albums -> normalize media items. Downloading is not this file's
// concern; the caller feeds the returned MediaItems into the existing TaskRunner path.
package tg

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gotd/td/tg"
)

// SearchRPC is the RPC surface the searcher needs: the peer/history methods shared with the
// resolve pipeline (album expansion reuses MessagesGetHistory) plus messages.search.
type SearchRPC interface {
	PipelineRPC
	MessagesSearch(ctx context.Context, request *tg.MessagesSearchRequest) (tg.MessagesMessagesClass, error)
}

// Compile-time assertion: the real client satisfies the search surface.
var _ SearchRPC = (*tg.Client)(nil)

const (
	// searchPageLimit is the page size of messages.search (the server caps it at 100).
	searchPageLimit = 100
	// searchDefaultMaxCount caps one search: a common keyword must not drag thousands of
	// messages (and tens of GB) into a single confirmation panel.
	searchDefaultMaxCount = 200
	// searchPageDelay sleeps between pages to stay far away from FLOOD_WAIT.
	searchPageDelay = 500 * time.Millisecond
)

// ErrBadChannelArg is returned when the /search channel argument is neither @username, a bare
// username, a t.me link, nor a numeric channel id.
var ErrBadChannelArg = errors.New("bad channel argument: use @username, t.me link or numeric channel id")

// SearchStats summarizes one search run for the confirmation panel.
type SearchStats struct {
	// MatchedMessages is the number of distinct messages that matched the query.
	MatchedMessages int
	// MediaCount is the number of downloadable media items (albums included).
	MediaCount int
	// TotalBytes is the summed size of all media items.
	TotalBytes int64
	// Truncated is true when the search stopped early because MaxCount was reached.
	Truncated bool
	// HitMessages holds the id of every message that matched the query and yielded media, in the
	// order the server returned them (newest first). An album appears once: its members share one
	// message link, so listing the members separately would only duplicate lines.
	HitMessages []int
}

// SearchOptions configures one SearchMedia call.
type SearchOptions struct {
	// Peer is the resolved channel peer.
	Peer tg.InputPeerClass
	// Query is the keyword (Telegram searches message text/captions server-side).
	Query string
	// MaxCount caps the number of matched messages; <=0 falls back to searchDefaultMaxCount.
	MaxCount int
	// PageDelay sleeps between pages; <=0 falls back to searchPageDelay.
	PageDelay time.Duration
}

// Searcher performs keyword searches against one channel.
type Searcher struct {
	rpc      SearchRPC
	resolver *PeerResolver
}

// NewSearcher wires the searcher to an RPC client and the shared peer resolver.
func NewSearcher(rpc SearchRPC, resolver *PeerResolver) (*Searcher, error) {
	if rpc == nil {
		return nil, errors.New("tg: nil search rpc")
	}
	if resolver == nil {
		return nil, errors.New("tg: nil peer resolver")
	}
	return &Searcher{rpc: rpc, resolver: resolver}, nil
}

// ParseChannelArg accepts the /search channel argument: @username, bare username, a t.me link
// (public t.me/user or private t.me/c/123) or a numeric channel id. It produces the MessageRef
// the peer resolver understands; message ids are irrelevant for search and left zero.
func ParseChannelArg(arg string) (MessageRef, error) {
	s := strings.TrimSpace(arg)
	if s == "" {
		return MessageRef{}, ErrBadChannelArg
	}
	// Strip a leading @ for the @username form.
	s = strings.TrimPrefix(s, "@")

	// t.me links (with or without scheme): reuse the link shapes but ignore the message id.
	if rest, ok := trimTmePrefix(s); ok {
		seg := strings.Split(rest, "/")
		switch {
		case len(seg) >= 2 && seg[0] == "c":
			// Private channel: t.me/c/<channelID>[/<msgID>].
			cid, err := parsePosInt64(seg[1])
			if err != nil {
				return MessageRef{}, fmt.Errorf("%w: bad private channel id %q", ErrBadChannelArg, seg[1])
			}
			return MessageRef{Kind: RefPrivate, ChannelID: cid}, nil
		case len(seg) >= 1 && seg[0] != "":
			u := seg[0]
			if i := strings.Index(u, "?"); i >= 0 {
				u = u[:i]
			}
			if !validUsername(u) {
				return MessageRef{}, fmt.Errorf("%w: bad username %q", ErrBadChannelArg, u)
			}
			return MessageRef{Kind: RefPublic, Username: u}, nil
		}
		return MessageRef{}, ErrBadChannelArg
	}

	// Numeric form first: Telegram usernames never consist of digits only, so "1234567890" and
	// "-1001234567890" (private supergroup/channel ids) are unambiguous here.
	if cid, err := parseChannelIDArg(s); err == nil {
		return MessageRef{Kind: RefPrivate, ChannelID: cid}, nil
	}

	// Bare username form.
	if validUsername(s) {
		return MessageRef{Kind: RefPublic, Username: s}, nil
	}
	return MessageRef{}, ErrBadChannelArg
}

func trimTmePrefix(s string) (string, bool) {
	lower := strings.ToLower(s)
	for _, prefix := range []string{"https://t.me/", "http://t.me/", "https://www.t.me/", "http://www.t.me/", "www.t.me/", "t.me/"} {
		if strings.HasPrefix(lower, prefix) {
			return s[len(prefix):], true
		}
	}
	return "", false
}

func parsePosInt64(s string) (int64, error) {
	if s == "" {
		return 0, errors.New("empty integer")
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("not a positive int64: %q", s)
	}
	return v, nil
}

// parseChannelIDArg parses "-1001234567890" or "1234567890" into a raw channel id. The
// negative form is the raw peer id of a private channel/supergroup and is the natural way to
// paste one, so it is accepted and normalized to the positive channel id.
func parseChannelIDArg(s string) (int64, error) {
	neg := false
	body := s
	if strings.HasPrefix(body, "-") {
		neg = true
		body = body[1:]
	}
	v, err := parsePosInt64(body)
	if err != nil {
		return 0, err
	}
	if neg {
		// "-1001234567890" is the peer id form (PeerIDFromChannelID output): recover the raw
		// channel id. A bare negative id (a regular group) is not searchable here — reject it.
		if cid, ok := ChannelIDFromPeerID(-v); ok {
			return cid, nil
		}
		return 0, fmt.Errorf("bad channel id %q: not a channel/supergroup peer id", s)
	}
	return v, nil
}

// SearchChannel resolves the channel argument then runs the search. It is the entry point the
// bot wiring layer calls. maxCount<=0 uses the default cap; progress (optional) is called with
// the running media count after every processed message.
func (s *Searcher) SearchChannel(ctx context.Context, channelArg, query string, maxCount int, progress func(done int)) ([]MediaItem, SearchStats, error) {
	ref, err := ParseChannelArg(channelArg)
	if err != nil {
		return nil, SearchStats{}, err
	}
	peer, err := s.resolver.Resolve(ctx, ref)
	if err != nil {
		return nil, SearchStats{}, fmt.Errorf("tg: resolve search channel: %w", err)
	}
	return s.SearchMedia(ctx, SearchOptions{
		Peer:     peer,
		Query:    query,
		MaxCount: maxCount,
	}, progress)
}

// SearchMedia runs the paginated server-side search. Only messages that both match the query and
// contain a photo or video come back (the filter is applied server-side), so no client-side kind
// filtering is needed here. Albums are expanded: the search only hits the album message carrying
// the caption, and ExpandAlbum pulls in the rest of the group.
func (s *Searcher) SearchMedia(ctx context.Context, opts SearchOptions, progress func(done int)) ([]MediaItem, SearchStats, error) {
	if opts.Peer == nil {
		return nil, SearchStats{}, errors.New("tg: nil search peer")
	}
	if strings.TrimSpace(opts.Query) == "" {
		return nil, SearchStats{}, errors.New("tg: empty search query")
	}
	maxCount := opts.MaxCount
	if maxCount <= 0 {
		maxCount = searchDefaultMaxCount
	}
	pageDelay := opts.PageDelay
	if pageDelay <= 0 {
		pageDelay = searchPageDelay
	}

	var (
		items []MediaItem
		stats SearchStats
		seen  = make(map[int]struct{}) // message ids already consumed (album members included)
		last  = 0                      // OffsetID cursor: the lowest message id seen so far
	)
	fetcher := func(fctx context.Context, offsetID, limit int) ([]tg.MessageClass, error) {
		return s.historyPage(fctx, opts.Peer, offsetID, limit)
	}

	for {
		if err := ctx.Err(); err != nil {
			return nil, stats, err
		}
		req := &tg.MessagesSearchRequest{
			Peer:     opts.Peer,
			Q:        opts.Query,
			Filter:   &tg.InputMessagesFilterPhotoVideo{},
			Limit:    searchPageLimit,
			OffsetID: last,
		}
		resp, err := s.rpc.MessagesSearch(ctx, req)
		if err != nil {
			return nil, stats, fmt.Errorf("tg: messages search: %w", err)
		}
		msgs := pipelineAllMessages(resp)
		rawCount := len(msgs)
		if rawCount == 0 {
			break
		}
		prevLast := last

		for _, mc := range msgs {
			msg, ok := mc.(*tg.Message)
			if !ok {
				continue
			}
			// Advance the search cursor from search-hit message IDs (not expanded album members)
			// so interleaved non-album hits are never skipped.
			if msg.ID < last || last == 0 {
				last = msg.ID
			}
			if _, dup := seen[msg.ID]; dup {
				continue
			}
			if msg.GroupedID != 0 {
				// Album member: the search only returned the caption carrier, expand the whole
				// group (dedup is by message id) and consume every member.
				group, aerr := ExpandAlbum(ctx, msg, fetcher)
				if aerr != nil {
					return nil, stats, fmt.Errorf("tg: search album expand: %w", aerr)
				}
				albumHasMedia := false
				for _, am := range group {
					if _, dup := seen[am.ID]; dup {
						continue
					}
					seen[am.ID] = struct{}{}
					it, mok := MediaFromMessage(am)
					if !mok {
						continue
					}
					items = append(items, it)
					stats.MediaCount++
					stats.TotalBytes += it.Size
					albumHasMedia = true
				}
				if albumHasMedia {
					stats.MatchedMessages++
					stats.HitMessages = append(stats.HitMessages, msg.ID)
					if progress != nil {
						progress(stats.MediaCount)
					}
				}
			} else {
				seen[msg.ID] = struct{}{}
				it, mok := MediaFromMessage(msg)
				if mok {
					items = append(items, it)
					stats.MediaCount++
					stats.TotalBytes += it.Size
					stats.MatchedMessages++
					stats.HitMessages = append(stats.HitMessages, msg.ID)
					if progress != nil {
						progress(stats.MediaCount)
					}
				}
			}
			if stats.MatchedMessages >= maxCount {
				stats.Truncated = true
				return items, stats, nil
			}
		}

		if rawCount < searchPageLimit {
			break
		}
		// Page cursor: the search returns messages newest-first, so continue below the lowest id
		// already seen. Stop if the cursor did not advance to prevent an infinite loop.
		if last == 0 || (prevLast != 0 && last >= prevLast) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, stats, ctx.Err()
		case <-time.After(pageDelay):
		}
	}
	return items, stats, nil
}

// historyPage fetches one history page (the album-expansion fetcher), mirroring Pipeline's
// pipelineHistory.
func (s *Searcher) historyPage(ctx context.Context, peer tg.InputPeerClass, offsetID, limit int) ([]tg.MessageClass, error) {
	resp, err := s.rpc.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer:      peer,
		OffsetID:  offsetID,
		AddOffset: 0,
		Limit:     limit,
	})
	if err != nil {
		return nil, fmt.Errorf("tg: messages get history: %w", err)
	}
	return pipelineAllMessages(resp), nil
}
