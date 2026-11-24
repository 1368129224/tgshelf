// search_test.go - unit tests for the /search command flow: usage text, not-wired guard, the
// async confirmation panel, the search result cache and the download hand-off through the normal
// tag-select flow. Everything runs against fakeAPI (zero network).
package bot

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"

	"github.com/1368129224/tgshelf/internal/state"
	"github.com/1368129224/tgshelf/internal/tg"
)

// searchPhotoItem builds a minimal photo media item for cache fixtures.
func searchPhotoItem(id int) tg.MediaItem {
	return tg.MediaItem{
		Kind:      tg.MediaPhoto,
		Name:      "photo.jpg",
		Size:      1234,
		MessageID: id,
	}
}

func TestSearchNotWired(t *testing.T) {
	b, api, _ := newTestBot(t)
	msg := textMessage(1, 100, "/search @durov hello")
	if err := b.handleText(context.Background(), msg); err != nil {
		t.Fatalf("handleText: %v", err)
	}
	if api.sentCount() != 1 {
		t.Fatalf("sent = %d, want 1", api.sentCount())
	}
	if got := api.sent[0].Text; !strings.Contains(got, "not available") {
		t.Fatalf("text = %q, want the not-wired notice", got)
	}
}

func TestSearchUsage(t *testing.T) {
	b, api, _ := newTestBot(t)
	// Wire the searcher (so the not-wired guard passes) but send a malformed command.
	b.SetSearchMedia(func(context.Context, string, string, func(int)) ([]tg.MediaItem, tg.SearchStats, error) {
		t.Fatal("searchMedia must not be called for a malformed command")
		return nil, tg.SearchStats{}, nil
	}, NewSearchStore(0))

	msg := textMessage(1, 100, "/search") // no arguments at all
	if err := b.handleText(context.Background(), msg); err != nil {
		t.Fatalf("handleText: %v", err)
	}
	if api.sentCount() != 1 {
		t.Fatalf("sent = %d, want 1", api.sentCount())
	}
	if got := api.sent[0].Text; !strings.Contains(got, "Usage:") || !strings.Contains(got, "/search @durov") {
		t.Fatalf("text = %q, want the usage text", got)
	}
}

func TestSearchConfirmationPanel(t *testing.T) {
	b, api, _ := newTestBot(t)
	b.SetSearchMedia(func(_ context.Context, channelArg, query string, _ func(int)) ([]tg.MediaItem, tg.SearchStats, error) {
		if channelArg != "@durov" {
			t.Errorf("channelArg = %q, want @durov", channelArg)
		}
		if query != "nice photos" {
			t.Errorf("query = %q, want %q", query, "nice photos")
		}
		return []tg.MediaItem{searchPhotoItem(1), searchPhotoItem(2)},
			tg.SearchStats{MatchedMessages: 2, MediaCount: 2, TotalBytes: 2468, HitMessages: []int{1, 2}}, nil
	}, NewSearchStore(0))

	msg := textMessage(1, 100, "/search @durov nice photos")
	if err := b.handleText(context.Background(), msg); err != nil {
		t.Fatalf("handleText: %v", err)
	}
	// The search runs asynchronously: wait for the confirmation edit.
	waitFor(t, "the confirmation panel", func() bool { return len(api.allEdits()) == 1 })
	edit := api.allEdits()[0]
	if edit.MessageID != 1 {
		t.Fatalf("edited message id = %d, want 1 (the placeholder)", edit.MessageID)
	}
	for _, want := range []string{
		"Search done", "@durov", `for "nice photos"`,
		"Matched messages: 2", "Media files: 2",
		// One link per matched message, in the order the search returned them.
		"https://t.me/durov/1", "https://t.me/durov/2",
		// The size sits at the very end, before the prompt.
		"Total size: ", "Pick a tag",
	} {
		if !strings.Contains(edit.Text, want) {
			t.Fatalf("panel text %q is missing %q", edit.Text, want)
		}
	}
	if !strings.HasSuffix(edit.Text, "Pick a tag to start the download:") {
		t.Fatalf("panel must end with the pick-a-tag prompt, got %q", edit.Text)
	}
	if idx := strings.Index(edit.Text, "https://t.me/durov/1"); idx > strings.Index(edit.Text, "Total size: ") {
		t.Fatal("the links must come before the total size")
	}
	// The panel must carry the shared tag keyboard.
	if edit.ReplyMarkup == nil {
		t.Fatal("panel has no keyboard")
	}
	if _, ok := edit.ReplyMarkup.(*models.InlineKeyboardMarkup); !ok {
		t.Fatalf("panel keyboard type = %T, want *models.InlineKeyboardMarkup", edit.ReplyMarkup)
	}
}

func TestSearchNoResults(t *testing.T) {
	b, api, _ := newTestBot(t)
	b.SetSearchMedia(func(context.Context, string, string, func(int)) ([]tg.MediaItem, tg.SearchStats, error) {
		return nil, tg.SearchStats{}, nil
	}, NewSearchStore(0))

	msg := textMessage(1, 100, "/search @durov nothing")
	if err := b.handleText(context.Background(), msg); err != nil {
		t.Fatalf("handleText: %v", err)
	}
	waitFor(t, "the empty-result edit", func() bool { return len(api.allEdits()) == 1 })
	if got := api.allEdits()[0].Text; !strings.Contains(got, "No photo/video found") {
		t.Fatalf("text = %q, want the empty-result notice", got)
	}
}

func TestSearchFailureShowsError(t *testing.T) {
	b, api, _ := newTestBot(t)
	b.SetSearchMedia(func(context.Context, string, string, func(int)) ([]tg.MediaItem, tg.SearchStats, error) {
		return nil, tg.SearchStats{}, errors.New("simulated search failure")
	}, NewSearchStore(0))

	msg := textMessage(1, 100, "/search @durov boom")
	if err := b.handleText(context.Background(), msg); err != nil {
		t.Fatalf("handleText: %v", err)
	}
	waitFor(t, "the failure edit", func() bool { return len(api.allEdits()) == 1 })
	if got := api.allEdits()[0].Text; !strings.Contains(got, "Search failed") || !strings.Contains(got, "simulated search failure") {
		t.Fatalf("text = %q, want the failure notice", got)
	}
}

// TestSearchTagSelectDownloads: after the confirmation panel, tapping a tag must schedule a
// state.Task whose Link carries the search prefix, and the downloader must be able to read the
// cached items from the store.
func TestSearchTagSelectDownloads(t *testing.T) {
	b, api, _ := newTestBot(t)
	items := []tg.MediaItem{searchPhotoItem(1), searchPhotoItem(2), searchPhotoItem(3)}
	b.SetSearchMedia(func(context.Context, string, string, func(int)) ([]tg.MediaItem, tg.SearchStats, error) {
		return items, tg.SearchStats{MatchedMessages: 3, MediaCount: 3, TotalBytes: 3702}, nil
	}, NewSearchStore(0))

	var (
		mu       sync.Mutex
		gotTask  state.Task
		download int
	)
	b.SetDownload(func(ctx context.Context, task state.Task, _ func(tg.TaskProgress)) error {
		mu.Lock()
		gotTask = task
		download++
		mu.Unlock()
		return nil
	})

	msg := textMessage(1, 100, "/search @durov hello")
	if err := b.handleText(context.Background(), msg); err != nil {
		t.Fatalf("handleText: %v", err)
	}
	waitFor(t, "the confirmation panel", func() bool { return len(api.allEdits()) == 1 })

	// Extract the short ID from the panel keyboard and tap the first tag.
	kb, ok := api.allEdits()[0].ReplyMarkup.(*models.InlineKeyboardMarkup)
	if !ok || len(kb.InlineKeyboard) == 0 || len(kb.InlineKeyboard[0]) == 0 {
		t.Fatal("panel has no tag keyboard")
	}
	data := kb.InlineKeyboard[0][0].CallbackData
	cb, err := ParseCallback(data)
	if err != nil {
		t.Fatalf("ParseCallback(%q): %v", data, err)
	}
	if cb.Action != ActionSelect {
		t.Fatalf("first button action = %q, want select", cb.Action)
	}

	// The link behind the short ID must be the search form.
	link, ok := b.links.Get(cb.ShortID)
	if !ok {
		t.Fatalf("short id %q not in the link store", cb.ShortID)
	}
	if !strings.HasPrefix(link, SearchLinkPrefix) {
		t.Fatalf("link = %q, want the %q prefix", link, SearchLinkPrefix)
	}
	searchID := strings.TrimPrefix(link, SearchLinkPrefix)
	cached, ok := b.searchStore.Get(searchID)
	if !ok || len(cached) != 3 {
		t.Fatalf("cached items = (%v, %v), want 3 items", cached, ok)
	}

	// Tap the tag: the download task must carry the search link.
	cbq := callbackQuery(2, 100, 555)
	if err := b.onSelectTag(context.Background(), cbq, cb); err != nil {
		t.Fatalf("onSelectTag: %v", err)
	}
	waitFor(t, "the download to start", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return download == 1
	})
	mu.Lock()
	defer mu.Unlock()
	if !strings.HasPrefix(gotTask.Link, SearchLinkPrefix) {
		t.Fatalf("task.Link = %q, want the %q prefix", gotTask.Link, SearchLinkPrefix)
	}
	if gotTask.Tag != "video" {
		t.Fatalf("task.Tag = %q, want the first tag", gotTask.Tag)
	}
}

func TestSearchStoreLRU(t *testing.T) {
	s := NewSearchStore(2)
	s.Put("a", []tg.MediaItem{searchPhotoItem(1)})
	s.Put("b", []tg.MediaItem{searchPhotoItem(2)})
	s.Put("c", []tg.MediaItem{searchPhotoItem(3)})
	if _, ok := s.Get("a"); ok {
		t.Fatal("oldest entry a survived the eviction")
	}
	if _, ok := s.Get("b"); !ok {
		t.Fatal("entry b fell out early")
	}
	if _, ok := s.Get("c"); !ok {
		t.Fatal("entry c missing")
	}
	// Re-Put refreshes the entry (and does not grow the order list).
	s.Put("b", []tg.MediaItem{searchPhotoItem(22)})
	s.Put("d", []tg.MediaItem{searchPhotoItem(4)})
	if _, ok := s.Get("b"); !ok {
		t.Fatal("refreshed entry b was evicted")
	}
	if _, ok := s.Get("c"); ok {
		t.Fatal("entry c should have been evicted after the re-Put of b")
	}
}

// TestSearchPanelListsLinksForPrivateChannel: a t.me/c/<id> argument yields t.me/c/<id>/<msgID>
// links, and album members (same target message) are collapsed.
func TestSearchPanelListsLinksForPrivateChannel(t *testing.T) {
	b, api, _ := newTestBot(t)
	// Three album members (one hit) plus one plain message: exactly two links.
	items := []tg.MediaItem{
		searchPhotoItem(501),
		searchPhotoItem(502),
		searchPhotoItem(503),
		searchPhotoItem(600),
	}
	b.SetSearchMedia(func(context.Context, string, string, func(int)) ([]tg.MediaItem, tg.SearchStats, error) {
		return items, tg.SearchStats{
			MatchedMessages: 2, MediaCount: 4, TotalBytes: 5000, HitMessages: []int{501, 600},
		}, nil
	}, NewSearchStore(0))

	msg := textMessage(1, 100, "/search t.me/c/1234567890 hello")
	if err := b.handleText(context.Background(), msg); err != nil {
		t.Fatalf("handleText: %v", err)
	}
	waitFor(t, "the confirmation panel", func() bool { return len(api.allEdits()) == 1 })
	text := api.allEdits()[0].Text
	for _, want := range []string{"https://t.me/c/1234567890/501", "https://t.me/c/1234567890/600"} {
		if !strings.Contains(text, want) {
			t.Fatalf("panel text %q is missing the link %s", text, want)
		}
	}
	// Album siblings 502/503 must not be listed: one link per hit message.
	for _, unwanted := range []string{"https://t.me/c/1234567890/502", "https://t.me/c/1234567890/503"} {
		if strings.Contains(text, unwanted) {
			t.Fatalf("album member link %s must not be listed, text = %q", unwanted, text)
		}
	}
}

// TestSearchPanelSplitsLongLists: a long result must be laid out over several messages, each
// within the budget, and only the last one carries the tag keyboard.
func TestSearchPanelSplitsLongLists(t *testing.T) {
	b, api, _ := newTestBot(t)
	const n = 300
	items := make([]tg.MediaItem, 0, n)
	hits := make([]int, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, searchPhotoItem(1000+i))
		hits = append(hits, 1000+i)
	}
	b.SetSearchMedia(func(context.Context, string, string, func(int)) ([]tg.MediaItem, tg.SearchStats, error) {
		return items, tg.SearchStats{MatchedMessages: n, MediaCount: n, TotalBytes: int64(n) * 1000, HitMessages: hits}, nil
	}, NewSearchStore(0))

	msg := textMessage(1, 100, "/search @durov hello")
	if err := b.handleText(context.Background(), msg); err != nil {
		t.Fatalf("handleText: %v", err)
	}
	// 300 links cannot fit in one message: expect the placeholder edit plus extra sends.
	waitFor(t, "every page to be delivered", func() bool {
		return len(api.allEdits()) == 1 && api.sentCount() >= 3
	})

	edits := api.allEdits()
	if len(edits) != 1 {
		t.Fatalf("placeholder edits = %d, want 1", len(edits))
	}
	if edits[0].ReplyMarkup != nil {
		t.Fatal("the first page must not carry the keyboard")
	}

	api.mu.Lock()
	sent := append([]*tgbot.SendMessageParams(nil), api.sent...)
	api.mu.Unlock()

	// Every page (the edited placeholder and the sent ones) must stay within the budget.
	if got := len(edits[0].Text); got > searchTextBudget {
		t.Fatalf("page 1 length = %d, want <= %d", got, searchTextBudget)
	}
	for i, s := range sent {
		if len(s.Text) > searchTextBudget {
			t.Fatalf("page %d length = %d, want <= %d", i+2, len(s.Text), searchTextBudget)
		}
	}
	// Only the very last message carries the keyboard.
	last := sent[len(sent)-1]
	if last.ReplyMarkup == nil {
		t.Fatal("the last page must carry the tag keyboard")
	}
	for i, s := range sent[:len(sent)-1] {
		if s.ReplyMarkup != nil {
			t.Fatalf("page %d carries a keyboard, only the last page may", i+2)
		}
	}
	// The total size closes the list.
	if !strings.Contains(last.Text, "Total size: ") {
		t.Fatalf("last page %q is missing the total size", last.Text)
	}
	// Every link is present exactly once across all pages.
	all := edits[0].Text
	for _, s := range sent {
		all += s.Text
	}
	for _, id := range []int{1000, 1001, 1299} {
		want := "https://t.me/durov/" + strconv.Itoa(id)
		if strings.Count(all, want+"\n")+strings.Count(all, want) == 0 {
			t.Fatalf("link %s missing from the panel", want)
		}
	}
}
