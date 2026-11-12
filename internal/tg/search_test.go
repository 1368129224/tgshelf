// search_test.go - unit tests for the /search pipeline: channel-argument parsing, pagination,
// the message cap, album expansion and the stats. Everything runs against a fake SearchRPC
// (zero network), following the fakePipelineRPC pattern.
package tg

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

// fakeSearchRPC adds MessagesSearch on top of fakePipelineRPC and records every request.
type fakeSearchRPC struct {
	fakePipelineRPC

	searchCalls int
	searchReqs  []*tg.MessagesSearchRequest
	// searchPages are returned in call order; once exhausted the last page repeats (mirroring
	// historyPages) unless searchStopAtEnd is set, in which case an empty page is returned.
	searchPages     []tg.MessagesMessagesClass
	searchStopAtEnd bool
	searchErr       error
}

func (f *fakeSearchRPC) MessagesSearch(ctx context.Context, request *tg.MessagesSearchRequest) (tg.MessagesMessagesClass, error) {
	f.searchCalls++
	f.searchReqs = append(f.searchReqs, request)
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	if len(f.searchPages) == 0 {
		return &tg.MessagesMessages{}, nil
	}
	idx := f.searchCalls - 1
	if idx >= len(f.searchPages) {
		if f.searchStopAtEnd {
			return &tg.MessagesMessages{}, nil
		}
		idx = len(f.searchPages) - 1
	}
	return f.searchPages[idx], nil
}

// Compile-time assertion: the fake satisfies the search surface.
var _ SearchRPC = (*fakeSearchRPC)(nil)

// newSearcherForTest builds a Searcher over the fake with the shared default resolution
// (username "telegram" -> channel 100, hash 700).
func newSearcherForTest(t *testing.T, rpc *fakeSearchRPC) *Searcher {
	t.Helper()
	p := newPipelineForTest(t, &rpc.fakePipelineRPC)
	s, err := NewSearcher(rpc, p.resolver)
	if err != nil {
		t.Fatalf("NewSearcher: %v", err)
	}
	return s
}

// videoMsg builds a message carrying a video document (for album composition).
func videoMsg(id int, groupedID int64) *tg.Message {
	return &tg.Message{
		ID:        id,
		GroupedID: groupedID,
		Media: &tg.MessageMediaDocument{
			Document: &tg.Document{
				ID:            int64(id) * 10,
				AccessHash:    3,
				FileReference: []byte("vref"),
				MimeType:      "video/mp4",
				Size:          1000,
				DCID:          2,
				Attributes:    []tg.DocumentAttributeClass{&tg.DocumentAttributeVideo{}},
			},
		},
	}
}

func TestParseChannelArg(t *testing.T) {
	cases := []struct {
		name    string
		arg     string
		ref     MessageRef
		wantErr bool
	}{
		{"at_username", "@durov", MessageRef{Kind: RefPublic, Username: "durov"}, false},
		{"bare_username", "durov", MessageRef{Kind: RefPublic, Username: "durov"}, false},
		{"tme_public", "https://t.me/durov", MessageRef{Kind: RefPublic, Username: "durov"}, false},
		{"tme_public_noscheme", "t.me/durov/45", MessageRef{Kind: RefPublic, Username: "durov"}, false},
		{"tme_private", "https://t.me/c/1234567", MessageRef{Kind: RefPrivate, ChannelID: 1234567}, false},
		{"tme_private_noscheme", "t.me/c/1234567/9", MessageRef{Kind: RefPrivate, ChannelID: 1234567}, false},
		{"numeric", "1234567890", MessageRef{Kind: RefPrivate, ChannelID: 1234567890}, false},
		// The -100… peer-id form normalizes to the raw channel id (same number
		// t.me/c/<id> uses), matching ParseLink's semantics.
		{"numeric_neg100", "-1001234567890", MessageRef{Kind: RefPrivate, ChannelID: 1234567890}, false},
		{"empty", "", MessageRef{}, true},
		{"bad_username", "not valid!", MessageRef{}, true},
		{"bad_private", "t.me/c/abc", MessageRef{}, true},
		{"garbage", "hello world", MessageRef{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := ParseChannelArg(tc.arg)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseChannelArg(%q) = %+v, want error", tc.arg, ref)
				}
				if !errors.Is(err, ErrBadChannelArg) {
					t.Fatalf("ParseChannelArg(%q) err = %v, want ErrBadChannelArg", tc.arg, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseChannelArg(%q): %v", tc.arg, err)
			}
			if ref.Kind != tc.ref.Kind || ref.Username != tc.ref.Username || ref.ChannelID != tc.ref.ChannelID {
				t.Fatalf("ParseChannelArg(%q) = %+v, want %+v", tc.arg, ref, tc.ref)
			}
		})
	}
}

func TestSearcherDefaults(t *testing.T) {
	if _, err := NewSearcher(nil, nil); err == nil {
		t.Fatal("NewSearcher(nil, nil) should fail")
	}
}

func TestSearchMediaSinglePage(t *testing.T) {
	rpc := &fakeSearchRPC{}
	rpc.defaultResolve()
	rpc.searchPages = []tg.MessagesMessagesClass{
		oneMsgPage(photoMsg(10), plainMsg(11), photoMsg(12)),
	}
	s := newSearcherForTest(t, rpc)

	items, stats, err := s.SearchMedia(context.Background(), SearchOptions{
		Peer:  &tg.InputPeerChannel{ChannelID: 100, AccessHash: 700},
		Query: "cat",
	}, nil)
	if err != nil {
		t.Fatalf("SearchMedia: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2 (the plain text message must be skipped)", len(items))
	}
	if items[0].MessageID != 10 || items[1].MessageID != 12 {
		t.Fatalf("item message ids = %d,%d; want 10,12", items[0].MessageID, items[1].MessageID)
	}
	if stats.MatchedMessages != 2 || stats.MediaCount != 2 {
		t.Fatalf("stats = %+v, want 2 matched / 2 media", stats)
	}
	if stats.TotalBytes <= 0 {
		t.Fatalf("TotalBytes = %d, want > 0", stats.TotalBytes)
	}
	if stats.Truncated {
		t.Fatal("stats.Truncated = true, want false")
	}
	if rpc.searchCalls != 1 {
		t.Fatalf("searchCalls = %d, want 1 (len(page) < limit stops paging)", rpc.searchCalls)
	}
	req := rpc.searchReqs[0]
	if req.Q != "cat" || req.Limit != searchPageLimit || req.OffsetID != 0 {
		t.Fatalf("first request = %+v", req)
	}
	if _, ok := req.Filter.(*tg.InputMessagesFilterPhotoVideo); !ok {
		t.Fatalf("filter = %T, want *tg.InputMessagesFilterPhotoVideo", req.Filter)
	}
}

func TestSearchMediaPagination(t *testing.T) {
	rpc := &fakeSearchRPC{}
	rpc.defaultResolve()
	// Two full pages (searchPageLimit messages each) then a short page: the loop must issue
	// three calls, with OffsetID set to the lowest message id of the previous page.
	rpc.searchPages = []tg.MessagesMessagesClass{
		idRangePage(300, searchPageLimit), // ids 300..201
		idRangePage(200, searchPageLimit), // ids 200..101
		oneMsgPage(photoMsg(50)),
	}
	s := newSearcherForTest(t, rpc)

	items, stats, err := s.SearchMedia(context.Background(), SearchOptions{
		Peer:      &tg.InputPeerChannel{ChannelID: 100, AccessHash: 700},
		Query:     "cat",
		MaxCount:  500, // above the default 200 cap: this test must page past it
		PageDelay: time.Millisecond,
	}, nil)
	if err != nil {
		t.Fatalf("SearchMedia: %v", err)
	}
	if rpc.searchCalls != 3 {
		t.Fatalf("searchCalls = %d, want 3", rpc.searchCalls)
	}
	if rpc.searchReqs[1].OffsetID != 201 {
		t.Fatalf("second page OffsetID = %d, want 201 (lowest id of page 1)", rpc.searchReqs[1].OffsetID)
	}
	if rpc.searchReqs[2].OffsetID != 101 {
		t.Fatalf("third page OffsetID = %d, want 101 (lowest id of page 2)", rpc.searchReqs[2].OffsetID)
	}
	if len(items) != searchPageLimit+searchPageLimit+1 || stats.MediaCount != searchPageLimit*2+1 {
		t.Fatalf("items = %d, stats.MediaCount = %d, want %d", len(items), stats.MediaCount, searchPageLimit*2+1)
	}
}

// idRangePage builds a page of searchPageLimit photo messages with ids start, start-1, ...
// (newest-first, as the server returns them).
func idRangePage(start, n int) tg.MessagesMessagesClass {
	msgs := make([]*tg.Message, 0, n)
	for i := 0; i < n; i++ {
		msgs = append(msgs, photoMsg(start-i))
	}
	return oneMsgPage(msgs...)
}

func TestSearchMediaMaxCountTruncates(t *testing.T) {
	rpc := &fakeSearchRPC{}
	rpc.defaultResolve()
	// A full page always repeats (searchStopAtEnd=false): without the cap this would loop
	// forever on the last page.
	rpc.searchPages = []tg.MessagesMessagesClass{
		oneMsgPage(photoMsg(30), photoMsg(20), photoMsg(10)),
	}
	s := newSearcherForTest(t, rpc)

	_, stats, err := s.SearchMedia(context.Background(), SearchOptions{
		Peer:     &tg.InputPeerChannel{ChannelID: 100, AccessHash: 700},
		Query:    "cat",
		MaxCount: 2,
	}, nil)
	if err != nil {
		t.Fatalf("SearchMedia: %v", err)
	}
	if !stats.Truncated {
		t.Fatal("stats.Truncated = false, want true (cap reached)")
	}
	if stats.MatchedMessages != 2 {
		t.Fatalf("MatchedMessages = %d, want 2", stats.MatchedMessages)
	}
	if rpc.searchCalls != 1 {
		t.Fatalf("searchCalls = %d, want 1 (cap stops within the first page)", rpc.searchCalls)
	}
}

func TestSearchMediaEmptyResult(t *testing.T) {
	rpc := &fakeSearchRPC{}
	rpc.defaultResolve()
	s := newSearcherForTest(t, rpc)

	items, _, err := s.SearchMedia(context.Background(), SearchOptions{
		Peer:  &tg.InputPeerChannel{ChannelID: 100, AccessHash: 700},
		Query: "nothing",
	}, nil)
	if err != nil {
		t.Fatalf("SearchMedia: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("items = %d, want 0", len(items))
	}
}

func TestSearchMediaErrorPropagates(t *testing.T) {
	rpc := &fakeSearchRPC{}
	rpc.defaultResolve()
	rpc.searchErr = errors.New("FLOOD_WAIT_60")
	s := newSearcherForTest(t, rpc)

	_, _, err := s.SearchMedia(context.Background(), SearchOptions{
		Peer:  &tg.InputPeerChannel{ChannelID: 100, AccessHash: 700},
		Query: "cat",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "FLOOD_WAIT_60") {
		t.Fatalf("err = %v, want wrapped FLOOD_WAIT_60", err)
	}
}

func TestSearchMediaAlbumExpansion(t *testing.T) {
	rpc := &fakeSearchRPC{}
	rpc.defaultResolve()
	// The search only returns the album's caption carrier (id 200, GroupedID=7). ExpandAlbum
	// must pull in the siblings 199 and 201 via MessagesGetHistory.
	rpc.searchPages = []tg.MessagesMessagesClass{
		oneMsgPage(&tg.Message{
			ID:        200,
			GroupedID: 7,
			Message:   "album caption",
			Media: &tg.MessageMediaPhoto{
				Photo: &tg.Photo{
					ID:            20000,
					AccessHash:    2,
					FileReference: []byte("ref"),
					DCID:          2,
					Sizes: []tg.PhotoSizeClass{
						&tg.PhotoSizeProgressive{Type: "x", Sizes: []int{100, 500}},
					},
				},
			},
		}),
	}
	rpc.historyPages = []tg.MessagesMessagesClass{
		// ExpandAlbum fetches OffsetID=200+11: the page around the album (ids 199..201).
		oneMsgPage(photoMsg(201), photoMsg(199), videoMsg(201, 7), videoMsg(200, 7), videoMsg(199, 7), plainMsg(150)),
	}
	s := newSearcherForTest(t, rpc)

	items, stats, err := s.SearchMedia(context.Background(), SearchOptions{
		Peer:  &tg.InputPeerChannel{ChannelID: 100, AccessHash: 700},
		Query: "album",
	}, nil)
	if err != nil {
		t.Fatalf("SearchMedia: %v", err)
	}
	if rpc.getHistory == 0 {
		t.Fatal("ExpandAlbum did not call MessagesGetHistory")
	}
	if len(items) != 3 {
		t.Fatalf("items = %d, want 3 (one album member each for 199/200/201)", len(items))
	}
	if stats.MatchedMessages != 1 || stats.MediaCount != 3 {
		t.Fatalf("stats = %+v, want 1 matched / 3 media", stats)
	}
	seen := map[int]bool{}
	for _, it := range items {
		seen[it.MessageID] = true
	}
	for _, id := range []int{199, 200, 201} {
		if !seen[id] {
			t.Fatalf("album member %d missing from items %v", id, seen)
		}
	}
}

func TestSearchChannelEndToEnd(t *testing.T) {
	rpc := &fakeSearchRPC{}
	rpc.defaultResolve()
	rpc.searchPages = []tg.MessagesMessagesClass{
		oneMsgPage(photoMsg(5)),
	}
	s := newSearcherForTest(t, rpc)

	items, stats, err := s.SearchChannel(context.Background(), "@telegram", "hello", 0, nil)
	if err != nil {
		t.Fatalf("SearchChannel: %v", err)
	}
	if len(items) != 1 || stats.MediaCount != 1 {
		t.Fatalf("items = %d, stats = %+v, want 1 media", len(items), stats)
	}
	// The resolver must have been consulted for the public username.
	if rpc.resolveCalls == 0 {
		t.Fatal("SearchChannel did not resolve the channel argument")
	}
}

func TestSearchChannelBadArg(t *testing.T) {
	rpc := &fakeSearchRPC{}
	rpc.defaultResolve()
	s := newSearcherForTest(t, rpc)

	_, _, err := s.SearchChannel(context.Background(), "not a channel!", "hello", 0, nil)
	if !errors.Is(err, ErrBadChannelArg) {
		t.Fatalf("err = %v, want ErrBadChannelArg", err)
	}
}

func TestSearchChannelResolveError(t *testing.T) {
	rpc := &fakeSearchRPC{}
	rpc.resolveErr = errors.New("no such username")
	s := newSearcherForTest(t, rpc)

	_, _, err := s.SearchChannel(context.Background(), "@missing", "hello", 0, nil)
	if err == nil || !strings.Contains(err.Error(), "resolve search channel") {
		t.Fatalf("err = %v, want wrapped resolve error", err)
	}
}

func TestSearchMediaProgressCallback(t *testing.T) {
	rpc := &fakeSearchRPC{}
	rpc.defaultResolve()
	rpc.searchPages = []tg.MessagesMessagesClass{
		oneMsgPage(photoMsg(10), plainMsg(11), photoMsg(12)),
	}
	s := newSearcherForTest(t, rpc)

	var counts []int
	_, _, err := s.SearchMedia(context.Background(), SearchOptions{
		Peer:  &tg.InputPeerChannel{ChannelID: 100, AccessHash: 700},
		Query: "cat",
	}, func(done int) {
		counts = append(counts, done)
	})
	if err != nil {
		t.Fatalf("SearchMedia: %v", err)
	}
	if len(counts) != 2 || counts[0] != 1 || counts[1] != 2 {
		t.Fatalf("progress calls = %v, want [1 2]", counts)
	}
}

func TestMessageLink(t *testing.T) {
	cases := []struct {
		name    string
		ref     MessageRef
		msgID   int
		want    string
		wantErr bool
	}{
		{"public", MessageRef{Kind: RefPublic, Username: "durov"}, 42, "https://t.me/durov/42", false},
		{"private", MessageRef{Kind: RefPrivate, ChannelID: 1234567890}, 42, "https://t.me/c/1234567890/42", false},
		{"bad_username", MessageRef{Kind: RefPublic, Username: "no pe"}, 42, "", true},
		{"no_channel", MessageRef{Kind: RefPrivate}, 42, "", true},
		{"zero_id", MessageRef{Kind: RefPublic, Username: "durov"}, 0, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MessageLink(tc.ref, tc.msgID)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("MessageLink = %q, want error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("MessageLink: %v", err)
			}
			if got != tc.want {
				t.Fatalf("MessageLink = %q, want %q", got, tc.want)
			}
		})
	}
}
