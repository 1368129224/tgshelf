// search.go - the /search command: search a channel's media by keyword and batch download.
//
// Flow (two-phase, mirroring the link flow): /search <channel> <keyword> -> async search with a
// placeholder message -> confirmation panel with the shared tag keyboard -> picking a tag
// schedules a state.Task whose Link is "search:<id>"; the assembly layer resolves that to the
// cached media items and reuses the normal download kernel.
package bot

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"go.uber.org/zap"

	"github.com/1368129224/tgshelf/internal/bot/progress"
	"github.com/1368129224/tgshelf/internal/tg"
)

// SearchLinkPrefix marks a task link that refers to a cached search result instead of a t.me
// link. The assembly layer (cmd/tgshelf) checks this prefix to swap the resolve step for a
// SearchStore lookup.
const SearchLinkPrefix = "search:"

// searchStoreCap is how many search results are kept for download; the oldest entries fall out
// first (same spirit as the LinkStore LRU).
const searchStoreCap = 64

// SearchStore caches the media items of one search run, keyed by the numeric search id embedded
// in the task link ("search:<id>").
type SearchStore struct {
	mu    sync.Mutex
	m     map[string][]tg.MediaItem
	order []string
	cap   int
}

// NewSearchStore builds the cache with its capacity (<=0 uses the default).
func NewSearchStore(cap int) *SearchStore {
	if cap <= 0 {
		cap = searchStoreCap
	}
	return &SearchStore{m: make(map[string][]tg.MediaItem), cap: cap}
}

// Put stores the items of one search run, evicting the oldest entry when full. Re-Putting an
// existing id refreshes both its value and its recency (it moves to the back of the queue).
func (s *SearchStore) Put(id string, items []tg.MediaItem) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, dup := s.m[id]; dup {
		// Refresh recency: move the id to the back of the order list.
		for i, v := range s.order {
			if v == id {
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
		}
	}
	s.order = append(s.order, id)
	s.m[id] = items
	for len(s.order) > s.cap {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.m, oldest)
	}
}

// Get returns the cached items of one search run.
func (s *SearchStore) Get(id string) ([]tg.MediaItem, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	items, ok := s.m[id]
	return items, ok
}

// SearchMediaFunc performs one keyword search; the assembly layer adapts it to tg.Searcher.
type SearchMediaFunc func(ctx context.Context, channelArg, query string, progress func(done int)) ([]tg.MediaItem, tg.SearchStats, error)

// SetSearchMedia wires the /search implementation and the shared result cache.
func (b *Bot) SetSearchMedia(fn SearchMediaFunc, store *SearchStore) {
	b.searchMedia = fn
	b.searchStore = store
}

// searchUsageText is the reply when /search is malformed.
const searchUsageText = "Usage: /search <channel> <keyword>\n" +
	"<channel>: @username, t.me link (t.me/user or t.me/c/123) or numeric channel id\n" +
	"Example: /search @durov contest"

// onSearchCommand handles the /search command: parse the arguments, send a placeholder, then run
// the search asynchronously (dispatchText must not block on network calls) and edit it into the
// confirmation panel with the shared tag keyboard.
func (b *Bot) onSearchCommand(ctx context.Context, msg *models.Message) error {
	if msg == nil {
		return nil
	}
	chatID := msg.Chat.ID
	if b.searchMedia == nil || b.searchStore == nil {
		_, err := b.sendText(ctx, chatID, "Search is not available (not wired).")
		return err
	}
	args := commandArgs(msg.Text)
	if len(args) < 2 {
		_, err := b.sendText(ctx, chatID, searchUsageText)
		return err
	}
	channelArg := args[0]
	query := strings.Join(args[1:], " ")

	placeholder, err := b.api.SendMessage(ctx, &tgbot.SendMessageParams{
		ChatID: chatID,
		Text:   fmt.Sprintf("🔍 Searching %s for %q…", channelArg, query),
	})
	if err != nil {
		return err
	}

	// Detached context: the search must outlive the update handler.
	sctx := context.WithoutCancel(ctx)
	sctx, scancel := context.WithTimeout(sctx, 10*time.Minute)
	go func() {
		defer scancel()
		defer func() {
			if r := recover(); r != nil {
				b.logger.Error("search command panicked", zap.Any("panic", r))
			}
		}()
		b.runSearch(sctx, chatID, placeholder.ID, channelArg, query)
	}()
	return nil
}

// runSearch performs the search and edits the placeholder into the confirmation panel.
func (b *Bot) runSearch(ctx context.Context, chatID int64, placeholderID int, channelArg, query string) {
	items, stats, err := b.searchMedia(ctx, channelArg, query, nil)
	if err != nil {
		b.logger.Warn("search failed",
			zap.String("channel", channelArg),
			zap.String("query", query),
			zap.Error(err),
		)
		b.editText(ctx, chatID, placeholderID, "❌ Search failed: "+b.translate(err), nil)
		return
	}
	if len(items) == 0 {
		b.editText(ctx, chatID, placeholderID,
			fmt.Sprintf("🔍 No photo/video found in %s for %q.", channelArg, query), nil)
		return
	}

	// Cache the result and register the synthetic link; the tag buttons reuse the normal
	// select flow, which puts "search:<id>" into state.Task.Link untouched.
	searchID := fmt.Sprintf("%d", b.now().UnixNano())
	b.searchStore.Put(searchID, items)
	link := SearchLinkPrefix + searchID
	shortID := b.links.Put(link)
	kb, _ := buildTagKeyboard(b.cfg.Tags, shortID, 0)

	size := progress.HumanBytes(float64(stats.TotalBytes))

	var head strings.Builder
	fmt.Fprintf(&head, "🔍 Search done: %s for %q\n", channelArg, query)
	fmt.Fprintf(&head, "• Matched messages: %d\n", stats.MatchedMessages)
	fmt.Fprintf(&head, "• Media files: %d\n", stats.MediaCount)
	if stats.Truncated {
		head.WriteString("• Note: the result was truncated (search cap reached)\n")
	}
	head.WriteString("\nLinks:")

	foot := fmt.Sprintf("Total size: %s\n\nPick a tag to start the download:", size)
	b.sendSearchPages(ctx, chatID, placeholderID, splitSearchPages(head.String(), foot, searchLinks(channelArg, stats, items)), kb)
}

// searchLinks builds one t.me link per matched message. Album members share a single message link,
// so they are not listed one by one: the search layer reports the hit messages, and the items are
// only used as a fallback (deduplicated by message id) when that list is unavailable.
func searchLinks(channelArg string, stats tg.SearchStats, items []tg.MediaItem) []string {
	ref, err := tg.ParseChannelArg(channelArg)
	if err != nil {
		return nil
	}
	ids := stats.HitMessages
	if len(ids) == 0 {
		seen := make(map[int]struct{}, len(items))
		for _, it := range items {
			if _, dup := seen[it.MessageID]; dup {
				continue
			}
			seen[it.MessageID] = struct{}{}
			ids = append(ids, it.MessageID)
		}
	}
	links := make([]string, 0, len(ids))
	for _, id := range ids {
		l, lerr := tg.MessageLink(ref, id)
		if lerr != nil {
			continue
		}
		links = append(links, l)
	}
	return links
}

// searchTextBudget is the character budget per message, kept well below Telegram's 4096 limit so
// that emoji and the trailing footer never push a message over the edge.
const searchTextBudget = 3500

// splitSearchPages lays the header, the one-link-per-line list and the footer out over as many
// messages as needed, each within searchTextBudget. The footer always ends the last page.
func splitSearchPages(header, footer string, links []string) []string {
	var pages []string
	cur := header
	for _, l := range links {
		if len(cur)+1+len(l) > searchTextBudget && cur != header {
			pages = append(pages, cur)
			cur = l
			continue
		}
		cur += "\n" + l
	}
	if len(cur)+2+len(footer) > searchTextBudget && cur != header {
		pages = append(pages, cur)
		cur = footer
	} else {
		cur += "\n\n" + footer
	}
	return append(pages, cur)
}

// sendSearchPages edits the placeholder into the first page and sends the remaining pages as new
// messages; the tag keyboard rides on the last page only.
func (b *Bot) sendSearchPages(ctx context.Context, chatID int64, placeholderID int, pages []string, kb *models.InlineKeyboardMarkup) {
	for i, page := range pages {
		var markup *models.InlineKeyboardMarkup
		if i == len(pages)-1 {
			markup = kb
		}
		if i == 0 {
			b.editText(ctx, chatID, placeholderID, page, markup)
			continue
		}
		params := &tgbot.SendMessageParams{ChatID: chatID, Text: page}
		if markup != nil {
			params.ReplyMarkup = markup
		}
		if _, err := b.api.SendMessage(ctx, params); err != nil {
			b.logger.Warn("send search page failed", zap.Int("page", i+1), zap.Error(err))
		}
	}
}

// editText edits one message's text (and optionally its markup); edit failures are logged, they
// must not break the flow.
func (b *Bot) editText(ctx context.Context, chatID int64, messageID int, text string, kb *models.InlineKeyboardMarkup) {
	params := &tgbot.EditMessageTextParams{
		ChatID:    chatID,
		MessageID: messageID,
		Text:      text,
	}
	if kb != nil {
		params.ReplyMarkup = kb
	}
	if _, err := b.api.EditMessageText(ctx, params); err != nil {
		b.logger.Warn("edit message failed", zap.Int("message_id", messageID), zap.Error(err))
	}
}
