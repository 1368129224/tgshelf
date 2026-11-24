package tg

import (
	"context"
	"errors"
	"testing"

	gotdtg "github.com/gotd/td/tg"
)

// peerVideoMsg is videoMsg with the channel peer recorded, which is what a real message carries and
// what RefreshItemLocation needs to find the dialog again.
func peerVideoMsg(id int, channelID int64) *gotdtg.Message {
	msg := videoMsg(id, 0)
	msg.PeerID = &gotdtg.PeerChannel{ChannelID: channelID}
	return msg
}

func TestRefreshItemLocationReturnsFreshLocation(t *testing.T) {
	const (
		channelID = int64(1000)
		msgID     = 42
	)
	rpc := &fakePipelineRPC{historyPages: []gotdtg.MessagesMessagesClass{
		oneMsgPage(peerVideoMsg(msgID, channelID)),
	}}
	p := setupPrivateCache(t, rpc, channelID, 700)

	loc, err := p.RefreshItemLocation(context.Background(), PeerIDFromChannelID(channelID), msgID, int64(msgID)*10)
	if err != nil {
		t.Fatalf("RefreshItemLocation: %v", err)
	}
	doc, ok := loc.(*gotdtg.InputDocumentFileLocation)
	if !ok {
		t.Fatalf("location type = %T, want *tg.InputDocumentFileLocation", loc)
	}
	if doc.ID != int64(msgID)*10 {
		t.Fatalf("doc id = %d, want %d", doc.ID, int64(msgID)*10)
	}

	// The item must be looked up backwards from id+1, like ResolveLink does.
	if len(rpc.historyReqs) != 1 {
		t.Fatalf("history requests = %d, want 1", len(rpc.historyReqs))
	}
	if got := rpc.historyReqs[0].OffsetID; got != msgID+1 {
		t.Fatalf("offset id = %d, want %d", got, msgID+1)
	}
}

func TestRefreshItemLocationRejectsChangedMedia(t *testing.T) {
	const (
		channelID = int64(1000)
		msgID     = 42
	)
	rpc := &fakePipelineRPC{historyPages: []gotdtg.MessagesMessagesClass{
		oneMsgPage(peerVideoMsg(msgID, channelID)),
	}}
	p := setupPrivateCache(t, rpc, channelID, 700)

	// A different document id means the message now holds another file; handing back its location
	// would download the wrong content.
	_, err := p.RefreshItemLocation(context.Background(), PeerIDFromChannelID(channelID), msgID, 999)
	if !errors.Is(err, ErrNoMedia) {
		t.Fatalf("err = %v, want ErrNoMedia", err)
	}
}

func TestRefreshItemLocationRejectsNonChannelDialog(t *testing.T) {
	p := newPipelineForTest(t, &fakePipelineRPC{})

	// A non-negative / plain negative dialog id is not a channel, so the peer cannot be rebuilt.
	_, err := p.RefreshItemLocation(context.Background(), 5, 42, 0)
	if !errors.Is(err, ErrChannelInaccessible) {
		t.Fatalf("err = %v, want ErrChannelInaccessible", err)
	}
}

func TestRefreshItemLocationReportsDeletedMessage(t *testing.T) {
	const (
		channelID = int64(1000)
		msgID     = 42
	)
	// The server answers with an older message, which fetchMessage treats as deleted.
	rpc := &fakePipelineRPC{historyPages: []gotdtg.MessagesMessagesClass{
		oneMsgPage(peerVideoMsg(msgID-1, channelID)),
	}}
	p := setupPrivateCache(t, rpc, channelID, 700)

	_, err := p.RefreshItemLocation(context.Background(), PeerIDFromChannelID(channelID), msgID, 0)
	if !errors.Is(err, ErrMessageDeleted) {
		t.Fatalf("err = %v, want ErrMessageDeleted", err)
	}
}
