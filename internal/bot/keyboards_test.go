package bot

import (
	"fmt"
	"strings"
	"testing"

	"github.com/go-telegram/bot/models"
)

// collectButtons flattens every button in the keyboard so a single loop can assert over all of
// them.
func collectButtons(t *testing.T, kb *models.InlineKeyboardMarkup) []models.InlineKeyboardButton {
	t.Helper()
	var btns []models.InlineKeyboardButton
	for _, row := range kb.InlineKeyboard {
		btns = append(btns, row...)
	}
	return btns
}

// Test 1: 10 tags -> one page, no pagination row, at most 6 buttons per row.
func TestBuildTagKeyboardSinglePage(t *testing.T) {
	tags := make([]string, 10)
	for i := range tags {
		tags[i] = fmt.Sprintf("tag%d", i)
	}
	kb, page := buildTagKeyboard(tags, "abc123", 0)
	if page != 0 {
		t.Fatalf("page = %d, want 0", page)
	}
	if len(kb.InlineKeyboard) != 3 { // 2 tag rows (6+4) + cancel row
		t.Fatalf("rows = %d, want 3 (2 tag rows + cancel)", len(kb.InlineKeyboard))
	}
	for i, row := range kb.InlineKeyboard[:len(kb.InlineKeyboard)-1] {
		if len(row) > 6 {
			t.Fatalf("row %d has %d buttons, more than 6", i, len(row))
		}
	}
	last := kb.InlineKeyboard[len(kb.InlineKeyboard)-1]
	if len(last) != 1 || last[0].Text != "Cancel" {
		t.Fatalf("the last row must be the cancel button, got %v", last)
	}
	// The last tag row must hold 4 entries (10 = 6 + 4).
	if got := len(kb.InlineKeyboard[1]); got != 4 {
		t.Fatalf("second tag row has %d entries, want 4", got)
	}
}

// Test 2: 25 tags -> 2 pages; the first page has next but no prev, the second has prev but no
// next.
func TestBuildTagKeyboardPagination(t *testing.T) {
	tags := make([]string, 25)
	for i := range tags {
		tags[i] = fmt.Sprintf("tag%d", i)
	}
	kb, page := buildTagKeyboard(tags, "abc123", 0)
	if page != 0 {
		t.Fatalf("first page number = %d, want 0", page)
	}
	// First page: 4 tag rows + 1 pagination row + 1 cancel row.
	if len(kb.InlineKeyboard) != 6 {
		t.Fatalf("first page rows = %d, want 6", len(kb.InlineKeyboard))
	}
	nav := kb.InlineKeyboard[4]
	if len(nav) != 2 {
		t.Fatalf("first page pagination row has %d buttons, want 2 (next + page number)", len(nav))
	}
	if nav[0].Text != "1/2" {
		t.Fatalf("first page pagination row starts with %q, want the page indicator 1/2", nav[0].Text)
	}
	if nav[1].Text != "next >" {
		t.Fatalf("first page pagination row ends with %q, want next >", nav[1].Text)
	}

	kb2, page2 := buildTagKeyboard(tags, "abc123", 1)
	if page2 != 1 {
		t.Fatalf("second page number = %d, want 1", page2)
	}
	// Second page: 1 tag row (25-24=1 entries) + 1 pagination row + 1 cancel row.
	if len(kb2.InlineKeyboard) != 3 {
		t.Fatalf("second page rows = %d, want 3", len(kb2.InlineKeyboard))
	}
	nav2 := kb2.InlineKeyboard[1]
	if len(nav2) != 2 {
		t.Fatalf("second page pagination row has %d buttons, want 2 (prev + page number)", len(nav2))
	}
	if nav2[0].Text != "< prev" {
		t.Fatalf("second page pagination row starts with %q, want < prev", nav2[0].Text)
	}
	if nav2[1].Text != "2/2" {
		t.Fatalf("second page pagination row ends with %q, want the page indicator 2/2", nav2[1].Text)
	}
}

// Test 3: TagIdx matches the global index (spot-checking the first button of page 2).
func TestBuildTagKeyboardTagIdxMatchesGlobalIndex(t *testing.T) {
	tags := make([]string, 25)
	for i := range tags {
		tags[i] = fmt.Sprintf("tag%d", i)
	}
	kb, _ := buildTagKeyboard(tags, "abc123", 1)
	first := kb.InlineKeyboard[0][0]
	if first.Text != "tag24" {
		t.Fatalf("first button text on page 2 = %q, want tag24", first.Text)
	}
	cb, err := ParseCallback(first.CallbackData)
	if err != nil {
		t.Fatalf("ParseCallback: %v", err)
	}
	if cb.Action != ActionSelect {
		t.Fatalf("Action = %q, want %q", cb.Action, ActionSelect)
	}
	if cb.TagIdx != 24 {
		t.Fatalf("TagIdx = %d, want 24 (global index)", cb.TagIdx)
	}
	if cb.Page != 1 {
		t.Fatalf("Page = %d, want 1", cb.Page)
	}
}

// Test 4: walk every button and assert callback_data is at most 64 bytes and round-trips into
// the expected Callback.
func TestBuildTagKeyboardCallbackDataRoundTrip(t *testing.T) {
	tags := make([]string, 50)
	for i := range tags {
		tags[i] = fmt.Sprintf("tag%d", i)
	}
	for _, page := range []int{0, 1, 2} {
		kb, _ := buildTagKeyboard(tags, "abc123", page)
		for ri, row := range kb.InlineKeyboard {
			for ci, btn := range row {
				if len(btn.CallbackData) > 64 {
					t.Fatalf("button(%d,%d) callback_data is %d bytes, more than 64", ri, ci, len(btn.CallbackData))
				}
				cb, err := ParseCallback(btn.CallbackData)
				if err != nil {
					t.Fatalf("button(%d,%d) parse: %v", ri, ci, err)
				}
				switch cb.Action {
				case ActionSelect:
					if cb.ShortID != "abc123" {
						t.Fatalf("button(%d,%d) ShortID = %q, want abc123", ri, ci, cb.ShortID)
					}
					if cb.Page != page {
						t.Fatalf("button(%d,%d) Page = %d, want %d", ri, ci, cb.Page, page)
					}
					if cb.TagIdx < 0 || cb.TagIdx >= len(tags) {
						t.Fatalf("button(%d,%d) TagIdx = %d, out of range", ri, ci, cb.TagIdx)
					}
					if tags[cb.TagIdx] != btn.Text {
						t.Fatalf("button(%d,%d) text %q does not match the tag %q at TagIdx=%d", ri, ci, btn.Text, tags[cb.TagIdx], cb.TagIdx)
					}
				case ActionPage:
					if cb.Page < 0 || cb.Page > 2 {
						t.Fatalf("button(%d,%d) pagination Page = %d, out of range", ri, ci, cb.Page)
					}
				case ActionNoop, ActionCancelSelect:
					// noop carries no fields; cancel_select has its ShortID checked.
					if cb.Action == ActionCancelSelect && cb.ShortID != "abc123" {
						t.Fatalf("cancel button ShortID = %q, want abc123", cb.ShortID)
					}
				default:
					t.Fatalf("button(%d,%d) has unexpected action %q", ri, ci, cb.Action)
				}
			}
		}
	}
}

// Test 5: CJK tags plus a 32-character shortID still keep callback_data within 64 bytes.
func TestBuildTagKeyboardChineseTagsLongShortID(t *testing.T) {
	tags := []string{"中文标签一", "超长中文名称的标签项非常多字", "日本語タグ", "한국어 태그", "中文/english 混合"}
	shortID := strings.Repeat("a", 32)
	kb, _ := buildTagKeyboard(tags, shortID, 0)
	for ri, row := range kb.InlineKeyboard {
		for ci, btn := range row {
			if len(btn.CallbackData) > 64 {
				t.Fatalf("button(%d,%d) callback_data is %d bytes, more than 64", ri, ci, len(btn.CallbackData))
			}
			if _, err := ParseCallback(btn.CallbackData); err != nil {
				t.Fatalf("button(%d,%d) parse: %v", ri, ci, err)
			}
		}
	}
	// CJK text must appear only in text, never in callback_data.
	for _, btn := range collectButtons(t, kb) {
		for _, r := range btn.CallbackData {
			if r > 127 {
				t.Fatalf("callback_data contains the non-ASCII character %q: %q", r, btn.CallbackData)
			}
		}
	}
}

// Test 6: page values -5 and 999 are both clamped into the valid range.
func TestBuildTagKeyboardPageClamping(t *testing.T) {
	tags := make([]string, 25) // 2 pages
	for i := range tags {
		tags[i] = fmt.Sprintf("tag%d", i)
	}
	kb, page := buildTagKeyboard(tags, "abc123", -5)
	if page != 0 {
		t.Fatalf("page=-5 returned %d, want it clamped to 0", page)
	}
	if nav := kb.InlineKeyboard[len(kb.InlineKeyboard)-2]; nav[0].Text != "1/2" {
		t.Fatalf("page=-5 must render page 1, pagination row = %v", nav)
	}
	kb2, page2 := buildTagKeyboard(tags, "abc123", 999)
	if page2 != 1 {
		t.Fatalf("page=999 returned %d, want it clamped to 1", page2)
	}
	// Find the page-indicator button by scanning the pagination row rather than relying on its
	// index within the row.
	navRow := kb2.InlineKeyboard[len(kb2.InlineKeyboard)-2]
	var pageBtn *models.InlineKeyboardButton
	for i := range navRow {
		if strings.Contains(navRow[i].Text, "/") {
			pageBtn = &navRow[i]
			break
		}
	}
	if pageBtn == nil || pageBtn.Text != "2/2" {
		t.Fatalf("page=999 must render page 2, pagination row = %v", navRow)
	}
}

// Test 7: empty tags neither panic nor produce an invalid keyboard; only the cancel button is
// returned.
func TestBuildTagKeyboardEmptyTags(t *testing.T) {
	kb, page := buildTagKeyboard(nil, "abc123", 0)
	if page != 0 {
		t.Fatalf("page = %d, want 0", page)
	}
	if len(kb.InlineKeyboard) != 1 {
		t.Fatalf("rows = %d, want 1 (cancel only)", len(kb.InlineKeyboard))
	}
	row := kb.InlineKeyboard[0]
	if len(row) != 1 || row[0].Text != "Cancel" {
		t.Fatalf("expected only the cancel button, got %v", row)
	}
	cb, err := ParseCallback(row[0].CallbackData)
	if err != nil {
		t.Fatalf("cancel button parse: %v", err)
	}
	if cb.Action != ActionCancelSelect || cb.ShortID != "abc123" {
		t.Fatalf("cancel button Callback = %+v", cb)
	}
}
