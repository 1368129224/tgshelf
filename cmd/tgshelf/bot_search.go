// bot_search.go - wiring the /search implementation into internal/bot.
//
// Layering rule (same as bot_download.go): internal/bot knows nothing about gotd; this adapter
// converts its SearchMediaFunc into tg.Searcher calls.
package main

import (
	"context"

	"github.com/1368129224/tgshelf/internal/bot"
	"github.com/1368129224/tgshelf/internal/tg"
)

// newBotSearcher adapts tg.Searcher to bot.SearchMediaFunc. maxCount<=0 lets the searcher apply
// its default cap.
func newBotSearcher(searcher *tg.Searcher) bot.SearchMediaFunc {
	return func(ctx context.Context, channelArg, query string, progress func(done int)) ([]tg.MediaItem, tg.SearchStats, error) {
		return searcher.SearchChannel(ctx, channelArg, query, 0, progress)
	}
}
