// Package progress renders download progress messages and throttles Telegram message edits.
//
// It is a separate package so progress rendering and bot interaction can evolve independently
// without breaking each other's builds. This package may import only internal/tg, go-telegram/bot,
// models, zap and the standard library; it must not import internal/bot (that would create an
// import cycle).
//
// # Design notes (rules that must hold)
//
//   - Throttling is by **time** (3s), not by "the Nth parse": when the output rate is not under
//     our control, line-count throttling either floods the chat or leaves it stale for a long time. An update
//     less than 3s after the previous edit is dropped outright.
//   - Message-edit failures are the norm (content unchanged, message deleted, rate limited), so
//     they are only logged at debug level: never returned as an error, never a panic, never an
//     interruption of the download.
//   - Speed is smoothed with an EWMA (alpha=0.3) so instantaneous jitter does not make the ETA
//     jump around.
package progress

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"go.uber.org/zap"

	tg "github.com/1368129224/tgshelf/internal/tg"
)

// Hard constants for rendering and throttling.
const (
	// barWidth is the number of cells in the progress bar.
	barWidth = 15
	// minEditInterval is the minimum gap between two Telegram message edits (by time, not by
	// line count).
	minEditInterval = 3 * time.Second
	// ewmaAlpha is the speed EWMA smoothing factor: new value weight 0.3, history weight 0.7.
	ewmaAlpha = 0.3
	// nameFieldWidth is the display column width of the filename on the title line (longer
	// names are not truncated; this only guarantees a minimum alignment).
	nameFieldWidth = 20
)

// EditorSender is the narrow interface needed to edit a message: both *tgbot.Bot and bot.BotAPI
// satisfy it structurally, and unit tests use a fake to stay completely offline.
type EditorSender interface {
	EditMessageText(ctx context.Context, p *tgbot.EditMessageTextParams) (*models.Message, error)
}

// Render turns a task progress snapshot into message text (a pure function, no side effects).
//
// The output looks like:
//
//	🎬 video.mp4            [2/5]
//	████████░░░░░░░ 62.4%
//
// Rules:
//   - The title line takes the basename of Current; with multiple files (ItemsTotal > 1) it
//     appends [done/total];
//   - The progress bar always has 15 cells; the percentage is clamped to [0, 100] (0%, 100% and
//     above 100% are all safe);
//   - When the total byte count is unknown (both total and p.BytesTotal are <= 0) it shows the
//     downloaded amount instead of a percentage;
//   - The speed/ETA line is appended by Editor (see UpdateProgress): the pure function holds no
//     speed state, because speed can only come from time-based sampling inside Editor.
func Render(p tg.TaskProgress, total int64) string {
	var b strings.Builder

	// Title line: 🎬 filename (basename); with multiple files, pad to the column width and
	// append [done/total].
	name := filepath.Base(p.Current)
	if name == "." || name == "/" {
		name = ""
	}
	if name != "" {
		title := "🎬 " + name
		if pad := nameFieldWidth - utf8.RuneCountInString(title); pad > 0 {
			title += strings.Repeat(" ", pad)
		}
		b.WriteString(title)
	}
	if p.ItemsTotal > 1 {
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		// Show "which file is downloading" rather than "how many are done": the first progress
		// sample arrives when the first chunk of the first file lands on disk, so ItemsDone is
		// still 0 and using it directly would show [0/9] (hit during real acceptance testing).
		// Adding one yields [1/9], clamped to [9/9] once everything is complete.
		cur := p.ItemsDone + 1
		if cur > p.ItemsTotal {
			cur = p.ItemsTotal
		}
		fmt.Fprintf(&b, "[%d/%d]", cur, p.ItemsTotal)
	}
	b.WriteString("\n")

	// Progress-bar line: fill cells by percentage when the total is known, otherwise show the
	// downloaded amount.
	totalBytes := total
	if totalBytes <= 0 {
		totalBytes = p.BytesTotal
	}
	if totalBytes > 0 {
		pct := clampPercent(float64(p.BytesDone) / float64(totalBytes) * 100)
		filled := int(math.Round(pct / 100 * barWidth))
		filled = clampInt(filled, 0, barWidth)
		bar := strings.Repeat("█", filled) + strings.Repeat("░", barWidth-filled)
		fmt.Fprintf(&b, "%s %.1f%%\n", bar, pct)
	} else {
		fmt.Fprintf(&b, "Downloaded %s\n", humanBytes(float64(p.BytesDone)))
	}
	return b.String()
}

// msgSample is the sampling and throttling state of one message (per-message, accessed under
// the lock).
type msgSample struct {
	// hasEdit records whether an edit has happened (the first edit is never throttled).
	hasEdit bool
	// lastEdit is when the last edit was actually issued.
	lastEdit time.Time

	// hasSample records whether a byte-sampling baseline exists (the first sample only
	// establishes the baseline and yields no speed).
	hasSample bool
	// lastBytes/lastTime are the bytes and time of the previous sample.
	lastBytes int64
	lastTime  time.Time
	// ewma is the smoothed speed (bytes/second); hasSpeed false means speed is unavailable.
	ewma     float64
	hasSpeed bool

	// lastItemsDone is the completed-file count at the last edit. A small album often takes less
	// than 3 seconds in total, so pure time throttling would swallow the whole [n/N] progression;
	// therefore a change in this value must let one update through.
	lastItemsDone int
	hasItemsDone  bool
}

// sampleKey uniquely identifies a progress message by (chatID, messageID).
type sampleKey struct {
	chatID    int64
	messageID int
}

// Editor is a progress message editor with time-based throttling.
//
// It is concurrency safe: all per-message state is read and written under mu, so it works under
// -race.
type Editor struct {
	sender  EditorSender
	logger  *zap.Logger
	now     func() time.Time // injectable clock: unit tests advance time by setting this field
	mu      sync.Mutex
	samples map[sampleKey]*msgSample
	markup  models.ReplyMarkup // inline keyboard kept on progress updates (e.g. the cancel button)
}

// NewEditor creates an editor. A nil logger degrades to a Nop logger.
func NewEditor(s EditorSender, logger *zap.Logger) *Editor {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Editor{
		sender:  s,
		logger:  logger,
		now:     time.Now,
		samples: make(map[sampleKey]*msgSample),
	}
}

// SetMarkup sets the inline keyboard attached to progress updates (e.g. the cancel button).
// In the Telegram Bot API, EditMessageText removes any existing buttons on the message when
// ReplyMarkup is not specified; setting this field keeps the given ReplyMarkup on every progress
// refresh.
func (e *Editor) SetMarkup(m models.ReplyMarkup) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.markup = m
}

// Update is the throttled editing entry point: an update less than 3s after the previous edit is
// dropped; final=true (terminal state) skips throttling and edits immediately. EditMessageText
// failures are only logged at debug level - the function returns no error, never panics and never
// interrupts the download.
func (e *Editor) Update(ctx context.Context, chatID int64, messageID int, text string, final bool) {
	e.update(ctx, chatID, messageID, text, final, -1)
}

// update is Update's internal implementation. itemsDone >= 0 means this update carries the
// completed-file count: any change in that value skips the throttle so a small album's [n/N]
// appears step by step (required by real acceptance testing).
func (e *Editor) update(ctx context.Context, chatID int64, messageID int, text string, final bool, itemsDone int) {
	key := sampleKey{chatID: chatID, messageID: messageID}
	now := e.now()

	e.mu.Lock()
	st := e.samples[key]
	if st == nil {
		st = &msgSample{}
		e.samples[key] = st
	}
	// Time-based throttling: drop when < 3s since the previous edit; a terminal state and a
	// "completed-file count changed" update skip it.
	itemChanged := itemsDone >= 0 && (!st.hasItemsDone || itemsDone != st.lastItemsDone)
	if !final && !itemChanged && st.hasEdit && now.Sub(st.lastEdit) < minEditInterval {
		e.mu.Unlock()
		return
	}
	st.hasEdit = true
	st.lastEdit = now
	if itemsDone >= 0 {
		st.lastItemsDone = itemsDone
		st.hasItemsDone = true
	}
	markup := e.markup
	if final {
		delete(e.samples, key)
	}
	e.mu.Unlock()

	params := &tgbot.EditMessageTextParams{
		ChatID:    chatID,
		MessageID: messageID,
		Text:      text,
	}
	if markup != nil {
		params.ReplyMarkup = markup
	}

	// Edit failures are normal; just log them and leave the caller (the download flow) alone.
	if _, err := e.sender.EditMessageText(ctx, params); err != nil {
		e.logger.Debug("edit progress message (download unaffected)",
			zap.Int64("chat_id", chatID),
			zap.Int("message_id", messageID),
			zap.Error(err))
	}
}

// UpdateProgress is the convenience entry point for progress updates: it samples bytes and
// updates the EWMA speed inside Editor first (sampling is not affected by throttling; throttling
// only drops "edits", never "samples"), then renders the title and bar with Render, appends the
// speed/ETA line and hands the result to Update for throttled editing.
func (e *Editor) UpdateProgress(ctx context.Context, chatID int64, messageID int, p tg.TaskProgress, total int64, final bool) {
	speed, ok := e.sample(chatID, messageID, p.BytesDone)

	text := Render(p, total)
	if line := speedLine(speed, ok, effectiveTotal(total, p.BytesTotal), p.BytesDone); line != "" {
		text += line
	}
	// Pass ItemsDone along: update skips the throttle when the file count changes, so [n/N]
	// appears step by step.
	e.update(ctx, chatID, messageID, text, final, p.ItemsDone)
}

// sample records one byte sample and returns the smoothed speed. When bytes go backwards
// (re-download or file switch) only the baseline is reset, leaving the historical EWMA alone.
// Speed is unavailable (first sample, or a zero interval) when ok is false.
func (e *Editor) sample(chatID int64, messageID int, bytes int64) (float64, bool) {
	now := e.now()
	e.mu.Lock()
	defer e.mu.Unlock()

	key := sampleKey{chatID: chatID, messageID: messageID}
	st := e.samples[key]
	if st == nil {
		st = &msgSample{}
		e.samples[key] = st
	}
	if !st.hasSample || bytes < st.lastBytes {
		st.hasSample = true
		st.lastBytes = bytes
		st.lastTime = now
		return st.ewma, st.hasSpeed
	}
	if dt := now.Sub(st.lastTime).Seconds(); dt > 0 {
		inst := float64(bytes-st.lastBytes) / dt
		if st.hasSpeed {
			st.ewma = ewmaAlpha*inst + (1-ewmaAlpha)*st.ewma
		} else {
			st.ewma = inst
			st.hasSpeed = true
		}
	}
	st.lastBytes = bytes
	st.lastTime = now
	return st.ewma, st.hasSpeed
}

// speedLine renders the speed/ETA line: it shows "calculating..." while speed is unavailable,
// and appends the remaining time estimated from the smoothed speed once the total is known.
func speedLine(speed float64, ok bool, total, done int64) string {
	if !ok || speed <= 0 {
		return "calculating..."
	}
	line := humanBytes(speed) + "/s"
	if total > 0 {
		rem := total - done
		if rem < 0 {
			rem = 0
		}
		line += fmt.Sprintf("   remaining %s", formatETA(float64(rem)/speed))
	}
	return line
}

// effectiveTotal returns the total byte count used for percentage/ETA: the total passed by the
// caller when positive, otherwise the snapshot's BytesTotal; 0 when both are unknown.
func effectiveTotal(total, bytesTotal int64) int64 {
	if total > 0 {
		return total
	}
	if bytesTotal > 0 {
		return bytesTotal
	}
	return 0
}

// clampPercent clamps a percentage to [0, 100].
func clampPercent(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// clampInt clamps an integer to [lo, hi].
func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// HumanBytes formats a byte count as a human-readable unit (base 1024, e.g. 12.3 MB).
func HumanBytes(n float64) string {
	return humanBytes(n)
}

// humanBytes formats a byte count as a human-readable unit (base 1024): whole bytes stay
// integral, every other unit keeps one decimal, e.g. 12.3 MB.
func humanBytes(n float64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	i := 0
	for n >= 1024 && i < len(units)-1 {
		n /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%.0f %s", n, units[0])
	}
	return fmt.Sprintf("%.1f %s", n, units[i])
}

// formatETA formats a number of seconds as mm:ss (under one hour) or HH:MM:SS.
// Extremely large / infinite values are capped to avoid int overflow when speed decays toward 0.
func formatETA(seconds float64) string {
	if math.IsNaN(seconds) || math.IsInf(seconds, 1) || seconds > 99*3600 {
		return ">99h"
	}
	total := int(math.Max(0, math.Floor(seconds)))
	hours, rem := total/3600, total%3600
	minutes, secs := rem/60, rem%60
	if hours > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", hours, minutes, secs)
	}
	return fmt.Sprintf("%02d:%02d", minutes, secs)
}
