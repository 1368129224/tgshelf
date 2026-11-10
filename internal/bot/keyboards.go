// Tag-picker keyboard construction: renders a tag list, paginated, as an inline keyboard.
// Tag text only ever goes into the button text; callback_data carries just the short ID and the
// global index (matching the encoding contract in callback.go, which naturally stays within
// Telegram's 64-byte limit).
package bot

import (
	"fmt"

	"github.com/go-telegram/bot/models"
)

const (
	// tagButtonsPerRow is the maximum number of tag buttons per row.
	tagButtonsPerRow = 6
	// tagRowsPerPage is the maximum number of tag rows per page (6*4=24 tags).
	tagRowsPerPage = 4
	// Label of the cancel button.
	cancelButtonText = "Cancel"
	// Labels of the pagination buttons.
	prevButtonText = "< prev"
	nextButtonText = "next >"
)

// buildTagKeyboard builds the tag-picker keyboard and returns it with the effective page number
// (clamped to the valid range). At most 6 tags per row and 4 rows per page; a pagination row is
// appended when there is more than one page; a "Cancel" button is always pinned at the bottom.
// With no tags it returns a valid keyboard holding only the cancel button.
func buildTagKeyboard(tags []string, shortID string, page int) (*models.InlineKeyboardMarkup, int) {
	totalPages := (len(tags) + tagButtonsPerRow*tagRowsPerPage - 1) / (tagButtonsPerRow * tagRowsPerPage)
	if totalPages < 1 {
		totalPages = 1
	}
	if page < 0 {
		page = 0
	}
	if page > totalPages-1 {
		page = totalPages - 1
	}

	start := page * tagButtonsPerRow * tagRowsPerPage
	end := start + tagButtonsPerRow*tagRowsPerPage
	if end > len(tags) {
		end = len(tags)
	}

	markup := &models.InlineKeyboardMarkup{}
	for i := start; i < end; i += tagButtonsPerRow {
		rowEnd := i + tagButtonsPerRow
		if rowEnd > end {
			rowEnd = end
		}
		row := make([]models.InlineKeyboardButton, 0, rowEnd-i)
		for j := i; j < rowEnd; j++ {
			row = append(row, models.InlineKeyboardButton{
				// The tag text itself only goes into text, never into callback_data
				// (Chinese text would blow past the 64-byte limit).
				Text:         tags[j],
				CallbackData: MustEncode(Callback{Action: ActionSelect, ShortID: shortID, Page: page, TagIdx: j}),
			})
		}
		markup.InlineKeyboard = append(markup.InlineKeyboard, row)
	}

	if totalPages > 1 {
		nav := make([]models.InlineKeyboardButton, 0, 3)
		if page > 0 {
			nav = append(nav, models.InlineKeyboardButton{
				Text:         prevButtonText,
				CallbackData: MustEncode(Callback{Action: ActionPage, ShortID: shortID, Page: page - 1}),
			})
		}
		nav = append(nav, models.InlineKeyboardButton{
			Text:         fmt.Sprintf("%d/%d", page+1, totalPages),
			CallbackData: MustEncode(Callback{Action: ActionNoop}),
		})
		if page < totalPages-1 {
			nav = append(nav, models.InlineKeyboardButton{
				Text:         nextButtonText,
				CallbackData: MustEncode(Callback{Action: ActionPage, ShortID: shortID, Page: page + 1}),
			})
		}
		markup.InlineKeyboard = append(markup.InlineKeyboard, nav)
	}

	markup.InlineKeyboard = append(markup.InlineKeyboard, []models.InlineKeyboardButton{{
		Text:         cancelButtonText,
		CallbackData: MustEncode(Callback{Action: ActionCancelSelect, ShortID: shortID}),
	}})
	return markup, page
}
