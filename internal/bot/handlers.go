// Business handlers for messages and callbacks: the main download path.
//
// Scope: parse the user's intent -> schedule a task -> reply with feedback. MTProto details are
// not its concern. The actual download runs through the DownloadFunc injected by the assembly layer
// via SetDownload; this package does not hard-bind gotd.
package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"go.uber.org/zap"

	"github.com/1368129224/tgshelf/internal/bot/progress"
	"github.com/1368129224/tgshelf/internal/state"
	"github.com/1368129224/tgshelf/internal/tg"
)

// adminCommandHook is the injection point for admin commands.
//
// The command dispatcher knows nothing about /status /cancel /show_config /sync and so on;
// admin.go implements those and injects them through this variable, so the two places never
// fight over one method name. handled=true means the command was processed and err is its result.
var adminCommandHook func(ctx context.Context, b *Bot, msg *models.Message, cmd string) (bool, error)

const (
	// helpText is the reply to /help and /start.
	helpText = "Usage: send one or more t.me message links to the bot, pick a tag and it downloads them.\n\n" +
		"Commands:\n" +
		"/help — show this help\n" +
		"/search <channel> <keyword> — search a channel's photo/video messages and batch download them\n" +
		"/status — show queued and running tasks\n" +
		"/cancel [ID|all] — cancel tasks\n" +
		"/show_config — show the current config (secrets masked)"

	// noLinkText is the reply when no link was found.
	noLinkText = "No Telegram link found.\nSend a link like https://t.me/channel_name/message_id."

	// tagPromptText introduces the tag selection message (reused when onPage rebuilds the
	// keyboard).
	tagPromptText = "Pick a download tag:\n"
)

// handleText handles a text message: commands take the command branch, everything else is treated
// as links.
func (b *Bot) handleText(ctx context.Context, msg *models.Message) error {
	if msg == nil {
		return nil
	}
	text := strings.TrimSpace(msg.Text)

	// Command branch: only entered when the first character is /, so ordinary text containing a
	// "/" is never mistaken for a command.
	if strings.HasPrefix(text, "/") {
		fields := strings.Fields(text)
		if len(fields) == 0 {
			return b.handleLinks(ctx, msg)
		}
		cmd := strings.TrimPrefix(fields[0], "/")
		// Commands in groups may carry an @botname suffix; strip it before matching.
		if i := strings.Index(cmd, "@"); i >= 0 {
			cmd = cmd[:i]
		}
		switch cmd {
		case "start", "help":
			_, err := b.sendText(ctx, msg.Chat.ID, helpText)
			return err
		case "search":
			return b.onSearchCommand(ctx, msg)
		case "cancel":
			// /cancel doubles as "leave the login wizard": while a wizard is running for this
			// admin user that wins, otherwise it falls through to task cancellation (cmdCancel).
			uid := senderID(msg)
			if isAdmin(b.cfg, uid) && b.login.ActiveFor(msg.Chat.ID, uid) {
				b.login.clear(msg.Chat.ID)
				_, err := b.sendText(ctx, msg.Chat.ID, "Login wizard closed (the existing session was not deleted).")
				return err
			}
			return b.dispatchCommand(ctx, msg, cmd)
		default:
			return b.dispatchCommand(ctx, msg, cmd)
		}
	}

	// The login wizard comes first: while it runs for this admin user, ordinary text is a phone
	// number / login code / 2FA password and must not be treated as a link.
	if uid := senderID(msg); isAdmin(b.cfg, uid) && b.login.ActiveFor(msg.Chat.ID, uid) {
		return b.handleLoginInput(ctx, msg)
	}

	return b.handleLinks(ctx, msg)
}

// handleLoginInput hands one message to the login wizard and deletes it afterwards.
//
// Deleting is mandatory: login codes and 2FA passwords must disappear from the chat history (the
// card notes: code messages are deleted automatically, so do not use this command in a public
// group). A failed delete does not block the login flow.
func (b *Bot) handleLoginInput(ctx context.Context, msg *models.Message) error {
	chatID := msg.Chat.ID
	text := strings.TrimSpace(msg.Text)

	// Delete first, then reply: even if a later RPC fails, the sensitive content is already gone
	// from the chat.
	if _, derr := b.api.DeleteMessage(ctx, &tgbot.DeleteMessageParams{
		ChatID:    chatID,
		MessageID: msg.ID,
	}); derr != nil {
		b.logger.Debug("failed to delete the login input message", zap.Error(derr))
	}

	reply, err := b.login.Handle(ctx, chatID, text)
	if err != nil {
		_, serr := b.sendText(ctx, chatID, b.translate(err))
		return serr
	}
	_, serr := b.sendText(ctx, chatID, reply)
	return serr
}

// handleLinks extracts every link from the message and sends one candidate message with a tag
// keyboard per link.
func (b *Bot) handleLinks(ctx context.Context, msg *models.Message) error {
	links := extractLinks(msg.Text)
	if len(links) == 0 {
		_, err := b.sendText(ctx, msg.Chat.ID, noLinkText)
		return err
	}

	for _, link := range links {
		// Compress the link into a short ID and keep it in the LRU registry: buttons carry only
		// the short ID, which keeps us inside the 64-byte limit.
		shortID := b.links.Put(link)
		kb, _ := buildTagKeyboard(b.cfg.Tags, shortID, 0)
		if _, err := b.api.SendMessage(ctx, &tgbot.SendMessageParams{
			ChatID:      msg.Chat.ID,
			Text:        tagPromptText + link,
			ReplyMarkup: kb,
		}); err != nil {
			return err
		}
	}
	return nil
}

// dispatchCommand dispatches non-builtin commands; admin commands are injected through
// adminCommandHook.
func (b *Bot) dispatchCommand(ctx context.Context, msg *models.Message, cmd string) error {
	if adminCommandHook != nil {
		if handled, err := adminCommandHook(ctx, b, msg, cmd); handled {
			return err
		}
	}
	_, err := b.sendText(ctx, msg.Chat.ID, fmt.Sprintf("Unknown command: %s\nSend /help for usage", cmd))
	return err
}

// startDownload is the single entry point for starting a download task (shared by onSelectTag and
// onRetry).
//
// It returns the new task's ID. Deduplication is the caller's job (see hasActiveTask).
func (b *Bot) startDownload(ctx context.Context, chatID int64, link, tag string) (string, error) {
	// The task ID is created before the placeholder message: the placeholder needs a "Cancel"
	// button, and the button carries the taskID.
	taskID := fmt.Sprintf("%d-%d", chatID, b.now().UnixNano())

	placeholder, err := b.api.SendMessage(ctx, &tgbot.SendMessageParams{
		ChatID: chatID,
		Text:   "⏳ Preparing the download…",
		// Attach the cancel keyboard to the progress message, so the download can be aborted
		// from the chat while it runs.
		ReplyMarkup: mustCancelKeyboard(taskID),
	})
	if err != nil {
		return "", err
	}
	msgID := placeholder.ID

	task := state.Task{
		ID:        taskID,
		ChatID:    chatID,
		MessageID: int64(msgID),
		Link:      link,
		Tag:       tag,
		CreatedAt: b.now(),
	}

	// Log task creation (including the original link).
	b.logger.Info("download task created and submitted",
		zap.String("task_id", taskID),
		zap.String("link", link),
		zap.String("tag", tag),
		zap.Int64("chat_id", chatID),
	)

	// One dedicated Editor per task: progress throttling and EWMA state never bleed between
	// concurrent tasks.
	editor := progress.NewEditor(b.api, b.logger)
	// Keep the cancel button during progress updates (otherwise EditMessageText drops the
	// previously set buttons):
	editor.SetMarkup(mustCancelKeyboard(taskID))

	// lastProgress holds the last progress snapshot, used for the multi-file completion summary.
	// The TaskRunner may call back from several goroutines, hence the lock.
	var progressMu sync.Mutex
	var lastProgress tg.TaskProgress
	var hasProgress bool

	work := func(wctx context.Context, t state.Task) error {
		started := b.now()
		fn := b.getDownload()
		b.logger.Info("download task started",
			zap.String("task_id", taskID),
			zap.String("link", link),
			zap.String("tag", tag),
		)

		var runErr error
		if fn == nil {
			runErr = errors.New("download executor not wired")
		} else {
			runErr = fn(wctx, t, func(p tg.TaskProgress) {
				progressMu.Lock()
				lastProgress = p
				hasProgress = true
				progressMu.Unlock()
				editor.UpdateProgress(wctx, chatID, msgID, p, p.BytesTotal, false)
			})
		}

		// Cancel path: /cancel or the "Cancel" button may have written only "Cancelling…", so
		// finish the cancellation text here and drop the buttons. Never write "❌ Download failed"
		// over a cancellation - that is exactly what made real acceptance item 4 fail.
		if cur, ok := b.tasks.Get(taskID); ok && cur.State == state.StateCancelled {
			b.logger.Info("download task cancelled",
				zap.String("task_id", taskID),
				zap.String("link", link),
				zap.String("tag", tag),
			)
			b.editTerminal(chatID, msgID, "⛔ Task cancelled", emptyMarkup())
			return runErr
		}

		// Terminal message: skip the throttle and edit immediately, attaching a retry button on
		// failure.
		if runErr == nil {
			totalDur := b.now().Sub(started)
			elapsed := totalDur.Round(time.Second)
			avgSpeed := formatAvgSpeed(lastProgress.BytesDone, totalDur)

			b.logger.Info("download task completed successfully",
				zap.String("task_id", taskID),
				zap.String("link", link),
				zap.String("tag", tag),
				zap.Duration("elapsed", elapsed),
				zap.Int64("bytes_done", lastProgress.BytesDone),
				zap.String("avg_speed", avgSpeed),
			)
			// Pass an empty keyboard rather than nil: EditMessageText keeps the old keyboard when
			// reply_markup is omitted, and the "Cancel" button must go after success or
			// cancellation.
			b.editTerminal(chatID, msgID, successText(elapsed, lastProgress, hasProgress, totalDur), emptyMarkup())
			return nil
		}

		b.logger.Warn("download task failed",
			zap.String("task_id", taskID),
			zap.String("link", link),
			zap.String("tag", tag),
			zap.Duration("elapsed", b.now().Sub(started)),
			zap.Error(runErr),
		)

		markup := &models.InlineKeyboardMarkup{
			InlineKeyboard: [][]models.InlineKeyboardButton{{
				{Text: "Retry", CallbackData: mustEncodeRetry(taskID)},
			}},
		}
		b.editTerminal(chatID, msgID, "❌ Download failed: "+b.translate(runErr), markup)
		return runErr
	}

	if err := b.tasks.Submit(task, work); err != nil {
		b.logger.Error("failed to submit the download task",
			zap.String("task_id", taskID),
			zap.String("link", link),
			zap.String("tag", tag),
			zap.Error(err),
		)
		_ = b.editTerminalSync(ctx, chatID, msgID, "❌ Could not create the task: "+b.translate(err), nil)
		return "", err
	}
	return taskID, nil
}

// mustCancelKeyboard builds the "Cancel" keyboard shown on the progress message.
//
// The 64-byte risk is handled as for the retry button: a task ID is normally far below the limit,
// and if one ever exceeds it, fall back to a no-op button so the whole keyboard still sends.
func mustCancelKeyboard(taskID string) *models.InlineKeyboardMarkup {
	data, err := Callback{Action: ActionCancelTask, TaskID: taskID}.Encode()
	if err != nil {
		data = MustEncode(Callback{Action: ActionNoop})
	}
	return &models.InlineKeyboardMarkup{
		InlineKeyboard: [][]models.InlineKeyboardButton{{{Text: "Cancel", CallbackData: data}}},
	}
}

// emptyMarkup returns an inline keyboard with no buttons, used to strip the old keyboard.
func emptyMarkup() models.ReplyMarkup {
	return &models.InlineKeyboardMarkup{InlineKeyboard: [][]models.InlineKeyboardButton{}}
}

// formatAvgSpeed computes and formats the average download speed.
func formatAvgSpeed(bytes int64, d time.Duration) string {
	if bytes <= 0 || d <= 0 {
		return ""
	}
	if d < time.Millisecond {
		d = time.Millisecond
	}
	sec := d.Seconds()
	speed := float64(bytes) / sec
	return progress.HumanBytes(speed) + "/s"
}

// successText builds the success terminal message: elapsed time, a multi-file success summary and
// the average download speed.
func successText(elapsed time.Duration, last tg.TaskProgress, has bool, totalDur time.Duration) string {
	var speedPart string
	bytes := last.BytesDone
	if bytes <= 0 && last.BytesTotal > 0 {
		bytes = last.BytesTotal
	}
	if bytes > 0 {
		if sp := formatAvgSpeed(bytes, totalDur); sp != "" {
			speedPart = ", avg " + sp
		}
	}

	elapsedStr := elapsed.String()
	if elapsed <= 0 {
		elapsedStr = "<1s"
	}

	if has && last.ItemsTotal > 1 {
		return fmt.Sprintf("✅ Download complete, %d/%d succeeded, took %s%s", last.ItemsDone, last.ItemsTotal, elapsedStr, speedPart)
	}
	return fmt.Sprintf("✅ Download complete, took %s%s", elapsedStr, speedPart)
}

// mustEncodeRetry builds the retry button's callback_data. The task ID is a short string generated
// by this process and normally far below 64 bytes; if it ever exceeds the limit (not reachable in
// theory) it falls back to a no-op button so the whole keyboard still sends - a trap the old
// implementation hit.
func mustEncodeRetry(taskID string) string {
	data, err := Callback{Action: ActionRetry, TaskID: taskID}.Encode()
	if err != nil {
		return MustEncode(Callback{Action: ActionNoop})
	}
	return data
}

// editTerminal edits the terminal message; a failed terminal edit does not change the task
// outcome, it is only logged.
func (b *Bot) editTerminal(chatID int64, messageID int, text string, markup models.ReplyMarkup) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = b.editTerminalSync(ctx, chatID, messageID, text, markup)
}

// editTerminalSync is editTerminal with a ctx (for when the error must be read back immediately).
func (b *Bot) editTerminalSync(ctx context.Context, chatID int64, messageID int, text string, markup models.ReplyMarkup) error {
	params := &tgbot.EditMessageTextParams{
		ChatID:    chatID,
		MessageID: messageID,
		Text:      text,
	}
	if markup != nil {
		params.ReplyMarkup = markup
	}
	_, err := b.api.EditMessageText(ctx, params)
	if err != nil {
		b.logger.Debug("failed to edit the terminal message (does not affect the task outcome)", zap.Error(err))
	}
	return err
}

// hasActiveTask reports whether this chat already has a non-terminal task for the same
// (link, tag) pair.
//
// It blocks "tap the same tag twice on one link" from downloading twice (an explicit card
// requirement).
func (b *Bot) hasActiveTask(chatID int64, link, tag string) bool {
	for _, t := range b.tasks.Snapshot() {
		if t.ChatID == chatID && t.Link == link && t.Tag == tag {
			return true
		}
	}
	return false
}

// callbackChat returns the chat and message ID behind a callback; ok=false for inline or
// unreachable messages.
func callbackChat(cbq *models.CallbackQuery) (chatID int64, messageID int, ok bool) {
	if cbq == nil || cbq.Message.Message == nil {
		return 0, 0, false
	}
	return cbq.Message.Message.Chat.ID, cbq.Message.Message.ID, true
}

// onSelectTag handles the "select a tag" callback: it starts a background download task and
// returns from the handler immediately.
func (b *Bot) onSelectTag(ctx context.Context, cbq *models.CallbackQuery, cb Callback) error {
	link, ok := b.links.Get(cb.ShortID)
	if !ok {
		return b.answerCallback(ctx, cbq, "This button has expired, send the link again", true)
	}
	if cb.TagIdx < 0 || cb.TagIdx >= len(b.cfg.Tags) {
		return b.answerCallback(ctx, cbq, "Invalid tag, please pick again", true)
	}
	chatID, promptMsgID, ok := callbackChat(cbq)
	if !ok {
		return b.answerCallback(ctx, cbq, "Cannot identify this chat", true)
	}
	tag := b.cfg.Tags[cb.TagIdx]

	// Deduplication: don't start a second task while the same link + tag is running.
	if b.hasActiveTask(chatID, link, tag) {
		return b.answerCallback(ctx, cbq, "This link is already downloading", true)
	}

	if _, err := b.startDownload(ctx, chatID, link, tag); err != nil {
		return b.answerCallback(ctx, cbq, "Could not start: "+b.translate(err), true)
	}

	// Delete the tag selection prompt as soon as the user picks, instead of waiting for the
	// download to finish.
	if promptMsgID > 0 {
		delCtx, delCancel := context.WithTimeout(context.Background(), 10*time.Second)
		if _, derr := b.api.DeleteMessage(delCtx, &tgbot.DeleteMessageParams{
			ChatID:    chatID,
			MessageID: promptMsgID,
		}); derr != nil {
			b.logger.Debug("failed to delete the tag selection message", zap.Error(derr), zap.String("link", link))
		}
		delCancel()
	}

	return b.answerCallback(ctx, cbq, "Download started", false)
}

// onCancelSelect handles the "cancel the selection" callback: it deletes the tag selection
// message.
func (b *Bot) onCancelSelect(ctx context.Context, cbq *models.CallbackQuery, cb Callback) error {
	chatID, messageID, ok := callbackChat(cbq)
	if !ok {
		return b.answerCallback(ctx, cbq, "", false)
	}
	if _, err := b.api.DeleteMessage(ctx, &tgbot.DeleteMessageParams{
		ChatID:    chatID,
		MessageID: messageID,
	}); err != nil {
		b.logger.Debug("failed to delete the tag selection message", zap.Error(err))
	}
	return b.answerCallback(ctx, cbq, "", false)
}

// onPage handles the "change page" callback: it rebuilds the tag keyboard for that page.
func (b *Bot) onPage(ctx context.Context, cbq *models.CallbackQuery, cb Callback) error {
	link, ok := b.links.Get(cb.ShortID)
	if !ok {
		return b.answerCallback(ctx, cbq, "This button has expired, send the link again", true)
	}
	chatID, messageID, ok := callbackChat(cbq)
	if !ok {
		return b.answerCallback(ctx, cbq, "Cannot identify this chat", true)
	}
	kb, _ := buildTagKeyboard(b.cfg.Tags, cb.ShortID, cb.Page)
	// The text is rebuilt from the link looked up by short ID, so resend the whole message
	// instead of only swapping the keyboard.
	if _, err := b.api.EditMessageText(ctx, &tgbot.EditMessageTextParams{
		ChatID:      chatID,
		MessageID:   messageID,
		Text:        tagPromptText + link,
		ReplyMarkup: kb,
	}); err != nil {
		b.logger.Debug("failed to rebuild the keyboard on page change", zap.Error(err))
		return b.answerCallback(ctx, cbq, "Page change failed", true)
	}
	return b.answerCallback(ctx, cbq, "", false)
}

// onCancelTask handles the "cancel a task" callback.
//
// Key point: Cancel works for both queued (PENDING) and running (RUNNING) tasks - the Manager gate
// guarantees a cancelled queued task never starts when it gets the semaphore.
func (b *Bot) onCancelTask(ctx context.Context, cbq *models.CallbackQuery, cb Callback) error {
	task, ok := b.tasks.Get(cb.TaskID)
	if !ok {
		return b.answerCallback(ctx, cbq, "Task not found or expired", true)
	}

	// Permissions: you can only cancel tasks in your own chat; admins can cancel anyone's.
	chatID, _, hasChat := callbackChat(cbq)
	if !isAdmin(b.cfg, cbq.From.ID) && (!hasChat || task.ChatID != chatID) {
		return b.answerCallback(ctx, cbq, "You can only cancel your own tasks", true)
	}

	b.logger.Info("user requested to cancel a download task",
		zap.String("task_id", cb.TaskID),
		zap.String("link", task.Link),
		zap.String("tag", task.Tag),
		zap.Int64("chat_id", task.ChatID),
	)

	prev := task.State
	updated, err := b.tasks.Cancel(cb.TaskID)
	if errors.Is(err, ErrTaskTerminal) {
		return b.answerCallback(ctx, cbq, "Task already finished", true)
	}
	if err != nil {
		return b.answerCallback(ctx, cbq, "Cancel failed: "+b.translate(err), true)
	}

	msg := "Cancelling…"
	if prev == state.StatePending {
		msg = "Cancelled (it was still queued and never started)"
	}
	if updated.MessageID > 0 {
		b.editTerminal(task.ChatID, int(updated.MessageID), "⛔ "+msg, nil)
	}
	return b.answerCallback(ctx, cbq, msg, false)
}

// onRetry handles the "retry" callback: it starts a new task with the original link and tag and
// leaves the old card in place.
func (b *Bot) onRetry(ctx context.Context, cbq *models.CallbackQuery, cb Callback) error {
	old, ok := b.tasks.Get(cb.TaskID)
	if !ok {
		return b.answerCallback(ctx, cbq, "The original task has expired, send the link again", true)
	}
	// Permissions: you can only retry tasks in your own chat; admins can retry anyone's.
	chatID, _, hasChat := callbackChat(cbq)
	if !isAdmin(b.cfg, cbq.From.ID) && (!hasChat || old.ChatID != chatID) {
		return b.answerCallback(ctx, cbq, "You can only retry your own tasks", true)
	}
	if b.hasActiveTask(old.ChatID, old.Link, old.Tag) {
		return b.answerCallback(ctx, cbq, "This link is already downloading", true)
	}
	b.logger.Info("user triggered a retry of the download task",
		zap.String("old_task_id", cb.TaskID),
		zap.String("link", old.Link),
		zap.String("tag", old.Tag),
	)
	if _, err := b.startDownload(ctx, old.ChatID, old.Link, old.Tag); err != nil {
		return b.answerCallback(ctx, cbq, "Retry failed: "+b.translate(err), true)
	}
	return b.answerCallback(ctx, cbq, "Download restarted", false)
}
