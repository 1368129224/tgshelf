// pipeline.go - the resolution pipeline: walks a t.me link string all the way down to
// a MediaItem list.
//
// Steps (ResolveLink, in implementation order):
//  1. ParseLink, a pure function;
//  2. PeerResolver.Resolve to get the InputPeer;
//  3. Pick the message ID: RefComment uses CommentID (the comment lives in the discussion
//     group, ref.MsgID is only the ID of the post in the main channel), everything else uses MsgID;
//  4. MessagesGetHistory with OffsetID=msgID+1 to look a single message up backwards: the
//     message may have been deleted and the server then returns an earlier one, so msg.ID
//     must equal msgID, otherwise ErrMessageDeleted;
//  5. MediaFromMessage extracts the media, ok=false -> ErrNoMedia;
//  6. When GroupedID != 0, ExpandAlbum expands the album and the fetcher calls getHistory
//     again (OffsetID = msgID+11, Limit = 20, both fixed by ExpandAlbum); the ok=true media
//     are appended to Items in ascending ID order, and none at all -> ErrNoMedia;
//  7. Assemble Resolved (MsgID holds the ID of the message actually used).
package tg

import (
	"context"
	"errors"
	"fmt"

	"github.com/gotd/td/tg"
)

// PipelineRPC is the minimal RPC subset the pipeline needs. *tg.Client satisfies it
// naturally (see the compile-time assertion).
type PipelineRPC interface {
	PeerRPC
	MessagesGetHistory(ctx context.Context, request *tg.MessagesGetHistoryRequest) (tg.MessagesMessagesClass, error)
}

// Compile-time assertion: *tg.Client satisfies PipelineRPC.
var _ PipelineRPC = (*tg.Client)(nil)

var (
	// ErrMessageDeleted: the message fetched backwards by ID does not match the requested
	// ID (the message was deleted and the server returned an earlier one), or the server
	// returned an empty result / MessageEmpty.
	ErrMessageDeleted = errors.New("message does not exist or was deleted")
	// ErrNoMedia: the message exists but has no downloadable media.
	ErrNoMedia = errors.New("this message has no downloadable media")
)

// Resolved is the full resolution result of ResolveLink.
type Resolved struct {
	Ref      MessageRef
	Peer     tg.InputPeerClass
	MsgID    int // message ID actually used (CommentID for RefComment)
	Messages []*tg.Message
	Items    []MediaItem
}

// Pipeline ties the parse/peer/message/media steps together: link -> peer -> message -> media list.
type Pipeline struct {
	rpc      PipelineRPC
	resolver *PeerResolver
}

// NewPipeline builds a Pipeline; neither rpc nor resolver may be nil.
func NewPipeline(rpc PipelineRPC, resolver *PeerResolver) (*Pipeline, error) {
	if rpc == nil {
		return nil, errors.New("tg: PipelineRPC must not be nil")
	}
	if resolver == nil {
		return nil, errors.New("tg: PeerResolver must not be nil")
	}
	return &Pipeline{rpc: rpc, resolver: resolver}, nil
}

// pipelineTargetMsgID picks the message ID actually to fetch from ref:
// a comment link uses the comment ID inside the discussion group (CommentID), while
// ref.MsgID is only the ID of the post in the main channel.
func pipelineTargetMsgID(ref MessageRef) int {
	if ref.Kind == RefComment {
		return ref.CommentID
	}
	return ref.MsgID
}

// pipelineMsgFromBatch picks the last *tg.Message out of a getHistory result.
// An empty result, or a last element that is not a *tg.Message (such as *tg.MessageEmpty),
// yields nil. Why the last one: under the offset semantics results come back in descending
// ID order, so the last element has the smallest ID and is closest to the requested
// OffsetID; on a hit its ID is exactly msgID.
func pipelineMsgFromBatch(resp tg.MessagesMessagesClass) *tg.Message {
	msgs := pipelineAllMessages(resp)
	if len(msgs) == 0 {
		return nil
	}
	msg, _ := msgs[len(msgs)-1].(*tg.Message)
	return msg
}

// pipelineAllMessages flattens the MessageClass elements of a getHistory result into a
// slice; elements that are not *tg.Message (MessageEmpty / MessageService) are dropped.
func pipelineAllMessages(resp tg.MessagesMessagesClass) []tg.MessageClass {
	var msgs []tg.MessageClass
	switch m := resp.(type) {
	case *tg.MessagesMessages:
		msgs = m.Messages
	case *tg.MessagesMessagesSlice:
		msgs = m.Messages
	case *tg.MessagesChannelMessages:
		msgs = m.Messages
	default:
		return nil
	}
	out := make([]tg.MessageClass, 0, len(msgs))
	for _, mc := range msgs {
		if _, ok := mc.(*tg.Message); ok {
			out = append(out, mc)
		}
	}
	return out
}

// pipelineHistory issues one messages.getHistory call with the given peer/offsetID/limit.
func (p *Pipeline) pipelineHistory(ctx context.Context, peer tg.InputPeerClass, offsetID, limit int) (tg.MessagesMessagesClass, error) {
	return p.rpc.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
		Peer:      peer,
		OffsetID:  offsetID,
		AddOffset: 0,
		Limit:     limit,
	})
}

// fetchMessage looks a single message up backwards by ID and checks that the ID matches;
// when the message was deleted it returns ErrMessageDeleted (an empty result, a
// MessageEmpty, or an earlier message returned by the server all count).
func (p *Pipeline) fetchMessage(ctx context.Context, peer tg.InputPeerClass, msgID int) (*tg.Message, error) {
	resp, err := p.pipelineHistory(ctx, peer, msgID+1, 1)
	if err != nil {
		return nil, fmt.Errorf("tg: get message %d: %w", msgID, err)
	}
	msg := pipelineMsgFromBatch(resp)
	if msg == nil {
		return nil, fmt.Errorf("tg: message %d: %w", msgID, ErrMessageDeleted)
	}
	if msg.ID != msgID {
		return nil, fmt.Errorf("tg: requested message %d but server returned %d: %w", msgID, msg.ID, ErrMessageDeleted)
	}
	return msg, nil
}

// ResolveLink resolves a link string into a MediaItem list; the steps are in the file
// header comment.
func (p *Pipeline) ResolveLink(ctx context.Context, raw string) (*Resolved, error) {
	// Step 1: parse the link with a pure function; errors pass through as is (the
	// ParseLink sentinels can be matched with errors.Is).
	ref, err := ParseLink(raw)
	if err != nil {
		return nil, err
	}

	// Step 2: resolve the peer (RefComment returns the peer of the discussion group);
	// errors are wrapped as is.
	peer, err := p.resolver.Resolve(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("tg: resolve peer: %w", err)
	}

	// Step 3: pick the message ID actually used.
	msgID := pipelineTargetMsgID(ref)

	// Step 4: look the single message up backwards by ID.
	msg, err := p.fetchMessage(ctx, peer, msgID)
	if err != nil {
		return nil, err
	}

	// Step 5: extract the media; plain text -> ErrNoMedia.
	item, ok := MediaFromMessage(msg)
	if !ok {
		return nil, fmt.Errorf("tg: message %d: %w", msgID, ErrNoMedia)
	}

	// Step 6: expand the album (GroupedID != 0). The fetcher issues getHistory on the same
	// peer; OffsetID = msg.ID+11 and Limit = 20 are passed in by ExpandAlbum.
	var (
		msgs  []*tg.Message
		items []MediaItem
	)
	if msg.GroupedID != 0 {
		fetcher := HistoryFetcherFunc(func(_ context.Context, offsetID, limit int) ([]tg.MessageClass, error) {
			resp, err := p.pipelineHistory(ctx, peer, offsetID, limit)
			if err != nil {
				return nil, err
			}
			return pipelineAllMessages(resp), nil
		})
		album, err := ExpandAlbum(ctx, msg, fetcher)
		if err != nil {
			return nil, fmt.Errorf("tg: %w", err)
		}
		for _, m := range album {
			if it, ok := MediaFromMessage(m); ok {
				items = append(items, it)
			}
		}
		if len(items) == 0 {
			return nil, fmt.Errorf("tg: album message %d: %w", msgID, ErrNoMedia)
		}
		msgs = album
	} else {
		items = append(items, item)
		msgs = []*tg.Message{msg}
	}

	// Step 7: assemble. MsgID holds the ID of the message actually used; Messages holds
	// the messages that took part in the resolution.
	return &Resolved{
		Ref:      ref,
		Peer:     peer,
		MsgID:    msgID,
		Messages: msgs,
		Items:    items,
	}, nil
}

// RefreshItemLocation re-fetches the message behind a media item and returns its current file
// location. A file reference embedded in a location stops working after roughly half an hour, so a
// batch that downloads for hours has to refresh each file when its reference expires; this is what
// RefreshFunc implementations call.
//
// docID is the document id the caller expects; when it does not match, the message was edited into
// a different file and the refresh reports ErrNoMedia instead of handing back a location for the
// wrong content.
func (p *Pipeline) RefreshItemLocation(ctx context.Context, dialogID int64, msgID int, docID int64) (tg.InputFileLocationClass, error) {
	channelID, ok := ChannelIDFromPeerID(dialogID)
	if !ok {
		return nil, fmt.Errorf("tg: refresh message %d: dialog %d is not a channel: %w", msgID, dialogID, ErrChannelInaccessible)
	}
	peer, err := p.resolver.Resolve(ctx, MessageRef{Kind: RefPrivate, ChannelID: channelID})
	if err != nil {
		return nil, fmt.Errorf("tg: refresh message %d: resolve peer: %w", msgID, err)
	}
	msg, err := p.fetchMessage(ctx, peer, msgID)
	if err != nil {
		return nil, err
	}
	item, ok := MediaFromMessage(msg)
	if !ok || item.Location == nil {
		return nil, fmt.Errorf("tg: refresh message %d: media is gone: %w", msgID, ErrNoMedia)
	}
	if docID != 0 && docIDOf(item) != docID {
		return nil, fmt.Errorf("tg: refresh message %d: media changed (want doc %d, got %d): %w",
			msgID, docID, docIDOf(item), ErrNoMedia)
	}
	return item.Location, nil
}
