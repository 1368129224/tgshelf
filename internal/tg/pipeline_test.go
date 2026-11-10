// pipeline_test.go - the 8-row matrix plus boundary tests for the resolution pipeline.
// Everything uses a fake PipelineRPC (embedding fakePeerRPC and adding MessagesGetHistory);
// real network access is forbidden. The fake follows the same pattern as the resolver tests
// in peer_test.go.
package tg

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/1368129224/tgshelf/internal/state"
	"github.com/gotd/td/tg"
)

// fakePipelineRPC adds MessagesGetHistory on top of fakePeerRPC and records the full
// parameters of every request so the matrix can assert OffsetID/Limit.
type fakePipelineRPC struct {
	fakePeerRPC

	getHistory  int
	historyReqs []*tg.MessagesGetHistoryRequest
	// historyPages are returned in call order; an empty result is returned once exhausted.
	historyPages []tg.MessagesMessagesClass
}

func (f *fakePipelineRPC) MessagesGetHistory(ctx context.Context, request *tg.MessagesGetHistoryRequest) (tg.MessagesMessagesClass, error) {
	f.getHistory++
	f.historyReqs = append(f.historyReqs, request)
	if len(f.historyPages) > 0 {
		// The i-th call returns the i-th page; once exhausted the last page is returned.
		idx := f.getHistory - 1
		if idx >= len(f.historyPages) {
			idx = len(f.historyPages) - 1
		}
		return f.historyPages[idx], nil
	}
	return &tg.MessagesMessages{}, nil
}

// Compile-time assertion: the fake satisfies PipelineRPC.
var _ PipelineRPC = (*fakePipelineRPC)(nil)

// newPipelineForTest builds a Pipeline with the matrix's shared configuration:
// username "telegram" resolves to channel 100 (hash 700).
func newPipelineForTest(t *testing.T, rpc *fakePipelineRPC) *Pipeline {
	t.Helper()
	st := newTestStoreForPeer(t)
	r, _ := newResolver(t, rpc, st)
	p, err := NewPipeline(rpc, r)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

// defaultResolve configures the fixed answer for the username path (matrix rows 1/4/5/6/7/8
// and the boundary tests).
func (f *fakePipelineRPC) defaultResolve() {
	f.resolveResult = &tg.ContactsResolvedPeer{
		Peer:  &tg.PeerChannel{ChannelID: 100},
		Chats: []tg.ChatClass{channelChat(100, 700)},
	}
}

// photoMsg builds a message with downloadable media (sizes use PhotoSizeProgressive).
func photoMsg(id int) *tg.Message {
	m := &tg.Message{
		ID: id,
		Media: &tg.MessageMediaPhoto{
			Photo: &tg.Photo{
				ID:            int64(id) * 100,
				AccessHash:    2,
				FileReference: []byte("ref"),
				DCID:          2,
				Sizes: []tg.PhotoSizeClass{
					&tg.PhotoSizeProgressive{Type: "x", Sizes: []int{100, 500}},
				},
			},
		},
	}
	return m
}

// plainMsg builds a text-only message without media.
func plainMsg(id int) *tg.Message {
	return &tg.Message{ID: id, Message: "hello"}
}

// oneMsgPage wraps a few *tg.Message into a MessagesMessages page.
func oneMsgPage(msgs ...*tg.Message) tg.MessagesMessagesClass {
	cls := make([]tg.MessageClass, 0, len(msgs))
	for _, m := range msgs {
		cls = append(cls, m)
	}
	return &tg.MessagesMessages{Messages: cls}
}

// setupPrivateCache pre-writes the access_hash cache of channelID into its own store and
// returns a Pipeline bound to that cache (matrix row 2).
func setupPrivateCache(t *testing.T, rpc *fakePipelineRPC, channelID, hash int64) *Pipeline {
	t.Helper()
	st := newTestStoreForPeer(t)
	st.UpsertEntity(state.Entity{
		Peer:     strconv.FormatInt(PeerIDFromChannelID(channelID), 10),
		Hash:     hash,
		Kind:     "channel",
		SyncedAt: time.Unix(1_700_000_000, 0).UTC(),
	})
	r, _ := newResolver(t, rpc, st)
	p, err := NewPipeline(rpc, r)
	if err != nil {
		t.Fatalf("NewPipeline: %v", err)
	}
	return p
}

// matrixRowID gives every matrix row a stable subtest name prefix.
func matrixRowID(n int, name string) string {
	return fmt.Sprintf("matrix%d_%s", n, name)
}

func TestPipelineMatrix(t *testing.T) {
	ctx := context.Background()

	// Row 1: https://t.me/telegram/193 -> resolution succeeds, len(Items) == 1, Kind is right.
	t.Run(matrixRowID(1, "public_channel_single_message"), func(t *testing.T) {
		rpc := &fakePipelineRPC{}
		rpc.defaultResolve()
		rpc.historyPages = []tg.MessagesMessagesClass{oneMsgPage(photoMsg(193))}
		p := newPipelineForTest(t, rpc)

		res, err := p.ResolveLink(ctx, "https://t.me/telegram/193")
		if err != nil {
			t.Fatalf("ResolveLink: %v", err)
		}
		if res.MsgID != 193 || res.Ref.Kind != RefPublic {
			t.Fatalf("MsgID/Kind = %d/%d, want 193/%d", res.MsgID, res.Ref.Kind, RefPublic)
		}
		if len(res.Items) != 1 {
			t.Fatalf("len(Items) = %d, want 1", len(res.Items))
		}
		if res.Items[0].Kind != MediaPhoto {
			t.Fatalf("Kind = %v, want MediaPhoto", res.Items[0].Kind)
		}
		if len(rpc.historyReqs) != 1 || rpc.historyReqs[0].OffsetID != 194 || rpc.historyReqs[0].Limit != 1 {
			t.Fatalf("getHistory request = %+v", rpc.historyReqs[0])
		}
	})

	// Row 2: https://t.me/c/1234567890/45 -> a cache hit succeeds, GetChannels is called 0 times.
	t.Run(matrixRowID(2, "private_channel_cache_hit"), func(t *testing.T) {
		rpc := &fakePipelineRPC{}
		rpc.historyPages = []tg.MessagesMessagesClass{oneMsgPage(photoMsg(45))}
		p := setupPrivateCache(t, rpc, 1234567890, 987654)

		res, err := p.ResolveLink(ctx, "https://t.me/c/1234567890/45")
		if err != nil {
			t.Fatalf("ResolveLink: %v", err)
		}
		if len(res.Items) != 1 || res.MsgID != 45 {
			t.Fatalf("Items/MsgID = %d/%d, want 1/45", len(res.Items), res.MsgID)
		}
		if rpc.getChannels != 0 {
			t.Fatalf("cache hit still called GetChannels %d times, want 0", rpc.getChannels)
		}
	})

	// Row 3: https://t.me/c/999/45 -> cache miss and GetChannels fails -> ErrChannelInaccessible.
	t.Run(matrixRowID(3, "private_channel_unreachable"), func(t *testing.T) {
		rpc := &fakePipelineRPC{}
		rpc.channelsErr = errors.New("CHANNEL_PRIVATE")
		p := newPipelineForTest(t, rpc)

		_, err := p.ResolveLink(ctx, "https://t.me/c/999/45")
		if !errors.Is(err, ErrChannelInaccessible) {
			t.Fatalf("err = %v, want ErrChannelInaccessible", err)
		}
	})

	// Row 4: https://t.me/telegram/200 -> grouped album of 3 out of order, expanded to 3 ascending.
	t.Run(matrixRowID(4, "album_expands_ascending"), func(t *testing.T) {
		rpc := &fakePipelineRPC{}
		rpc.defaultResolve()
		// Target message 200 plus the two others of the group, 198/199, delivered out of order.
		album := []*tg.Message{photoMsg(198), photoMsg(200), photoMsg(199)}
		for _, m := range album {
			m.GroupedID = 777
		}
		rpc.historyPages = []tg.MessagesMessagesClass{
			oneMsgPage(album[1]),
			oneMsgPage(album[0], album[2], album[1]),
		}
		p := newPipelineForTest(t, rpc)

		res, err := p.ResolveLink(ctx, "https://t.me/telegram/200")
		if err != nil {
			t.Fatalf("ResolveLink: %v", err)
		}
		if len(res.Items) != 3 {
			t.Fatalf("len(Items) = %d, want 3", len(res.Items))
		}
		for i, want := range []int{198, 199, 200} {
			if res.Items[i].MessageID != want {
				t.Fatalf("Items[%d].MessageID = %d, want %d (ascending)", i, res.Items[i].MessageID, want)
			}
		}
		// The fetcher's OffsetID must be 200+11=211.
		if len(rpc.historyReqs) != 2 || rpc.historyReqs[1].OffsetID != 211 {
			t.Fatalf("second getHistory = %+v", rpc.historyReqs)
		}
	})

	// Row 5: https://t.me/telegram/300?comment=360409 -> the actual request uses the comment ID.
	t.Run(matrixRowID(5, "comment_link_uses_comment_id"), func(t *testing.T) {
		rpc := &fakePipelineRPC{}
		rpc.defaultResolve()
		// RefComment: resolve the main channel + getFullChannel for the linked chat + GetChannels
		// for the discussion group hash.
		rpc.fullChannelRes = &tg.MessagesChatFull{FullChat: linkedFull(100, 200)}
		rpc.channelsResult = &tg.MessagesChats{Chats: []tg.ChatClass{channelChat(200, 800)}}
		rpc.historyPages = []tg.MessagesMessagesClass{oneMsgPage(photoMsg(360409))}
		p := newPipelineForTest(t, rpc)

		res, err := p.ResolveLink(ctx, "https://t.me/telegram/300?comment=360409")
		if err != nil {
			t.Fatalf("ResolveLink: %v", err)
		}
		if res.MsgID != 360409 {
			t.Fatalf("MsgID = %d, want 360409 (comment ID)", res.MsgID)
		}
		if len(rpc.historyReqs) != 1 || rpc.historyReqs[0].OffsetID != 360410 {
			t.Fatalf("getHistory request OffsetID = %+v, want 360410", rpc.historyReqs)
		}
		if len(res.Items) != 1 {
			t.Fatalf("len(Items) = %d, want 1", len(res.Items))
		}
	})

	// Row 6: https://t.me/telegram/5/1930 -> the topic segment is dropped, OffsetID=1931.
	t.Run(matrixRowID(6, "topic_segment_dropped"), func(t *testing.T) {
		rpc := &fakePipelineRPC{}
		rpc.defaultResolve()
		rpc.historyPages = []tg.MessagesMessagesClass{oneMsgPage(photoMsg(1930))}
		p := newPipelineForTest(t, rpc)

		res, err := p.ResolveLink(ctx, "https://t.me/telegram/5/1930")
		if err != nil {
			t.Fatalf("ResolveLink: %v", err)
		}
		if res.MsgID != 1930 {
			t.Fatalf("MsgID = %d, want 1930", res.MsgID)
		}
		if len(rpc.historyReqs) != 1 || rpc.historyReqs[0].OffsetID != 1931 {
			t.Fatalf("getHistory OffsetID = %+v, want 1931", rpc.historyReqs)
		}
	})

	// Row 7: https://t.me/telegram/9999 -> the server returns an earlier message -> ErrMessageDeleted.
	t.Run(matrixRowID(7, "message_deleted_earlier_returned"), func(t *testing.T) {
		rpc := &fakePipelineRPC{}
		rpc.defaultResolve()
		rpc.historyPages = []tg.MessagesMessagesClass{oneMsgPage(photoMsg(9000))}
		p := newPipelineForTest(t, rpc)

		_, err := p.ResolveLink(ctx, "https://t.me/telegram/9999")
		if !errors.Is(err, ErrMessageDeleted) {
			t.Fatalf("err = %v, want ErrMessageDeleted", err)
		}
	})

	// Row 7b: the server returns an empty result -> ErrMessageDeleted as well.
	t.Run(matrixRowID(7, "message_deleted_empty_result"), func(t *testing.T) {
		rpc := &fakePipelineRPC{}
		rpc.defaultResolve()
		rpc.historyPages = []tg.MessagesMessagesClass{&tg.MessagesMessages{}}
		p := newPipelineForTest(t, rpc)

		_, err := p.ResolveLink(ctx, "https://t.me/telegram/9999")
		if !errors.Is(err, ErrMessageDeleted) {
			t.Fatalf("err = %v, want ErrMessageDeleted (empty result)", err)
		}
	})

	// Row 8: https://t.me/telegram/188 -> plain text -> ErrNoMedia.
	t.Run(matrixRowID(8, "plain_text_no_media"), func(t *testing.T) {
		rpc := &fakePipelineRPC{}
		rpc.defaultResolve()
		rpc.historyPages = []tg.MessagesMessagesClass{oneMsgPage(plainMsg(188))}
		p := newPipelineForTest(t, rpc)

		_, err := p.ResolveLink(ctx, "https://t.me/telegram/188")
		if !errors.Is(err, ErrNoMedia) {
			t.Fatalf("err = %v, want ErrNoMedia", err)
		}
	})
}

// TestPipelineInvalidLink boundary: an invalid link surfaces ErrNotTMeLink.
func TestPipelineInvalidLink(t *testing.T) {
	rpc := &fakePipelineRPC{}
	p := newPipelineForTest(t, rpc)

	_, err := p.ResolveLink(context.Background(), "not a link")
	if !errors.Is(err, ErrNotTMeLink) {
		t.Fatalf("err = %v, want ErrNotTMeLink", err)
	}
	// A parse failure must not trigger any RPC.
	if rpc.getHistory != 0 || rpc.resolveCalls != 0 {
		t.Fatalf("parse failure must not issue RPCs: getHistory=%d resolveCalls=%d", rpc.getHistory, rpc.resolveCalls)
	}
}

// TestPipelineNewPipelineNilArgs guards the constructor.
func TestPipelineNewPipelineNilArgs(t *testing.T) {
	rpc := &fakePipelineRPC{}
	if _, err := NewPipeline(nil, nil); err == nil {
		t.Fatal("all-nil arguments should fail")
	}
	if _, err := NewPipeline(rpc, nil); err == nil {
		t.Fatal("nil resolver should fail")
	}
}
