// search_test.go - unit tests for the /search command flow: usage text, not-wired guard, the
// async confirmation panel, the search result cache and the download hand-off through the normal
// tag-select flow. Everything runs against fakeAPI (zero network).
package bot

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

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
			tg.SearchStats{MatchedMessages: 2, MediaCount: 2, TotalBytes: 2468}, nil
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
	for _, want := range []string{"Search done", "@durov", `for "nice photos"`, "Matched messages: 2", "Media files: 2", "Pick a tag"} {
		if !strings.Contains(edit.Text, want) {
			t.Fatalf("panel text %q is missing %q", edit.Text, want)
		}
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
