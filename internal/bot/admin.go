// Admin commands: /status /cancel /show_config /sync /login /logout.
//
// This file hooks adminCommandHook into handlers.go's command dispatcher through init, so commands
// are registered in exactly one place (handlers.go's dispatchCommand).
package bot

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/1368129224/tgshelf/internal/state"
)

// SyncFunc is the /sync implementation (the assembly layer injects
// tg.PeerResolver.SyncDialogs).
type SyncFunc func(ctx context.Context, full bool) (int, error)

// AccountFunc returns the currently signed-in account; it errors when not signed in.
type AccountFunc func(ctx context.Context) (userID int64, username string, dc int, err error)

// SetSync injects the /sync implementation.
func (b *Bot) SetSync(fn SyncFunc) {
	b.depMu.Lock()
	b.syncDialogs = fn
	b.depMu.Unlock()
}

func (b *Bot) getSync() SyncFunc {
	b.depMu.RLock()
	fn := b.syncDialogs
	b.depMu.RUnlock()
	return fn
}

// SetAccount injects the account lookup implementation.
func (b *Bot) SetAccount(fn AccountFunc) {
	b.depMu.Lock()
	b.account = fn
	b.depMu.Unlock()
}

func (b *Bot) getAccount() AccountFunc {
	b.depMu.RLock()
	fn := b.account
	b.depMu.RUnlock()
	return fn
}

func init() {
	// The command dispatcher lives in handlers.go; this only attaches this file's implementation.
	adminCommandHook = adminDispatch
}

// adminDispatch is the single entry point for admin commands; handled means this file took the
// command.
func adminDispatch(ctx context.Context, b *Bot, msg *models.Message, cmd string) (bool, error) {
	switch cmd {
	case "status":
		return true, b.cmdStatus(ctx, msg)
	case "cancel", "cancel_all":
		return true, b.cmdCancel(ctx, msg, cmd)
	case "show_config", "config":
		return true, b.cmdShowConfig(ctx, msg)
	case "sync":
		return true, b.cmdSync(ctx, msg)
	case "login", "logout":
		return true, b.cmdLoginLogout(ctx, msg, cmd)
	}
	return false, nil
}

// cmdStatus lists the queued and running tasks visible to the current user.
//
// Key point: queued tasks must be visible. A task is persisted as PENDING the moment it is
// created, so reading Snapshot (non-terminal only) covers both kinds and needs no extra in-memory
// queue.
func (b *Bot) cmdStatus(ctx context.Context, msg *models.Message) error {
	all := b.tasks.Snapshot()
	mine := make([]state.Task, 0, len(all))
	admin := isAdmin(b.cfg, senderID(msg))
	for _, t := range all {
		if admin || t.ChatID == msg.Chat.ID {
			mine = append(mine, t)
		}
	}

	if len(mine) == 0 {
		_, err := b.sendText(ctx, msg.Chat.ID, "No tasks right now.\nSend a t.me link to start a download.")
		return err
	}

	sort.SliceStable(mine, func(i, j int) bool {
		return mine[i].CreatedAt.Before(mine[j].CreatedAt)
	})

	var sb strings.Builder
	fmt.Fprintf(&sb, "📋 Current tasks (%d)\n", len(mine))
	now := b.now()
	writeGroup := func(title string, stateWant state.TaskState) {
		var rows []state.Task
		for _, t := range mine {
			if t.State == stateWant {
				rows = append(rows, t)
			}
		}
		if len(rows) == 0 {
			return
		}
		sb.WriteString("\n" + title + "\n")
		for _, t := range rows {
			fmt.Fprintf(&sb, "• %s | %s\n  %s\n", t.ID, t.Tag, t.Link)
			switch t.State {
			case state.StateRunning:
				if t.StartedAt != nil {
					fmt.Fprintf(&sb, "  elapsed %s\n", formatElapsed(now.Sub(*t.StartedAt)))
				}
				if pct := taskProgressPercent(t); pct >= 0 {
					fmt.Fprintf(&sb, "  progress %.1f%%\n", pct)
				}
			case state.StatePending:
				sb.WriteString("  queued, waiting for a free slot\n")
			}
		}
	}
	writeGroup("▶️ Downloading", state.StateRunning)
	writeGroup("⏳ Queued", state.StatePending)

	_, err := b.sendText(ctx, msg.Chat.ID, sb.String())
	return err
}

// taskProgressPercent estimates the percentage from the items' downloaded bytes; it returns -1
// when there is no data.
func taskProgressPercent(t state.Task) float64 {
	var done, total int64
	for _, it := range t.Items {
		done += it.Downloaded
		total += it.Size
	}
	if total <= 0 {
		return -1
	}
	return float64(done) / float64(total) * 100
}

// formatElapsed formats a duration as mm:ss (under 1 hour) or HH:MM:SS.
func formatElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int(d / time.Second)
	h, rem := total/3600, total%3600
	m, s := rem/60, rem%60
	if h > 0 {
		return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// cmdCancel handles /cancel <id> and /cancel all.
//
// Cancelling a queued task really works: Manager.Cancel sets CANCELLED first, then the worker
// re-reads the state after taking the semaphore and gives up when it is no longer PENDING (see the
// gate in manager.go).
func (b *Bot) cmdCancel(ctx context.Context, msg *models.Message, cmd string) error {
	var target string
	if cmd == "cancel_all" {
		target = "all"
	} else {
		args := commandArgs(msg.Text) // the arguments after stripping /cancel and @botname
		if len(args) == 0 {
			_, err := b.sendText(ctx, msg.Chat.ID, "Usage: /cancel <task ID> or /cancel all")
			return err
		}
		target = args[0]
	}

	admin := isAdmin(b.cfg, senderID(msg))

	if strings.EqualFold(target, "all") {
		var cancelled, skipped int
		for _, t := range b.tasks.Snapshot() {
			if !admin && t.ChatID != msg.Chat.ID {
				continue
			}
			if _, err := b.tasks.Cancel(t.ID); err != nil {
				skipped++
				continue
			}
			cancelled++
		}
		if cancelled == 0 {
			_, err := b.sendText(ctx, msg.Chat.ID, "No tasks to cancel.")
			return err
		}
		_, err := b.sendText(ctx, msg.Chat.ID,
			fmt.Sprintf("Cancelled %d tasks%s.", cancelled, skippedText(skipped)))
		return err
	}

	task, ok := b.tasks.Get(target)
	if !ok {
		_, err := b.sendText(ctx, msg.Chat.ID, "Task not found or expired: "+target)
		return err
	}
	if task.ChatID != msg.Chat.ID && !admin {
		_, err := b.sendText(ctx, msg.Chat.ID, "You can only cancel your own tasks.")
		return err
	}

	prev := task.State
	_, err := b.tasks.Cancel(target)
	if errors.Is(err, ErrTaskTerminal) {
		_, serr := b.sendText(ctx, msg.Chat.ID, "Task already finished.")
		return serr
	}
	if err != nil {
		_, serr := b.sendText(ctx, msg.Chat.ID, "Cancel failed: "+b.translate(err))
		return serr
	}
	msgText := "Cancelled: " + target
	if prev == state.StatePending {
		msgText = "Cancelled (it was queued and will not start): " + target
	}
	if task.MessageID > 0 {
		// Strip the "Cancel" button so it cannot be tapped after the task ended.
		_ = b.editTerminalSync(ctx, task.ChatID, int(task.MessageID), "⛔ Task cancelled", emptyMarkup())
	}
	_, serr := b.sendText(ctx, msg.Chat.ID, msgText)
	return serr
}

func skippedText(n int) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf(", %d already finished and skipped", n)
}

// cmdShowConfig prints a config snapshot (admins only). It never includes bot_token / api_hash /
// session contents.
func (b *Bot) cmdShowConfig(ctx context.Context, msg *models.Message) error {
	if !isAdmin(b.cfg, senderID(msg)) {
		_, err := b.sendText(ctx, msg.Chat.ID, "Not allowed: /show_config is admin only.")
		return err
	}
	_, err := b.sendText(ctx, msg.Chat.ID, b.showConfigText(ctx))
	return err
}

// showConfigText builds the masked config text (extracted so tests can assert the sensitive values
// are absent).
func (b *Bot) showConfigText(ctx context.Context) string {
	var sb strings.Builder
	sb.WriteString("⚙️ Current config\n")
	fmt.Fprintf(&sb, "Download folder: %s\n", b.cfg.DownloadPath)
	fmt.Fprintf(&sb, "Tags: %s\n", strings.Join(b.cfg.Tags, ", "))
	fmt.Fprintf(&sb, "Concurrent tasks: %d\n", b.cfg.Bot.MaxConcurrency)
	fmt.Fprintf(&sb, "Threads per file: %d\n", b.cfg.Download.Threads)
	fmt.Fprintf(&sb, "File concurrency: %d\n", b.cfg.Download.FileConcurrency)
	fmt.Fprintf(&sb, "Proxy: %s\n", maskProxy(b.cfg.ProxyURL))
	sb.WriteString("Account: " + b.accountStatus(ctx))
	return sb.String()
}

// accountStatus returns "signed in (user id/username)" or "not signed in".
func (b *Bot) accountStatus(ctx context.Context) string {
	accountFn := b.getAccount()
	if accountFn == nil {
		return "not signed in (account lookup not wired)"
	}
	id, username, dc, err := accountFn(ctx)
	if err != nil {
		return "not signed in: " + b.translate(err)
	}
	if username == "" {
		return fmt.Sprintf("signed in (user id %d, DC %d)", id, dc)
	}
	return fmt.Sprintf("signed in (@%s, user id %d, DC %d)", username, id, dc)
}

// maskProxy masks a proxy URL into scheme://***@host or scheme://***.
//
// A proxy string usually carries credentials and must never appear in a chat; even without
// credentials the host and port are masked, so the internal network layout does not leak.
func maskProxy(raw string) string {
	v := strings.TrimSpace(raw)
	if v == "" {
		return "not set"
	}
	scheme, rest, hasScheme := strings.Cut(v, "://")
	if strings.Contains(v, "@") {
		if !hasScheme || rest == "" {
			return "***"
		}
		// Keep the host part after the last @ and mask everything before it.
		host := rest[strings.LastIndex(rest, "@")+1:]
		return scheme + "://***@" + host
	}
	if hasScheme {
		return scheme + "://***"
	}
	return "***"
}

// cmdSync synchronizes dialogs immediately and refreshes the access_hash cache (admins only).
func (b *Bot) cmdSync(ctx context.Context, msg *models.Message) error {
	if !isAdmin(b.cfg, senderID(msg)) {
		_, err := b.sendText(ctx, msg.Chat.ID, "Not allowed: /sync is admin only.")
		return err
	}
	syncFn := b.getSync()
	if syncFn == nil {
		_, err := b.sendText(ctx, msg.Chat.ID, "Sync not wired: this process has no MTProto client.")
		return err
	}
	_, err := b.sendText(ctx, msg.Chat.ID, "Syncing the dialog list…")
	if err != nil {
		return err
	}
	n, serr := syncFn(ctx, true)
	if serr != nil {
		_, werr := b.sendText(ctx, msg.Chat.ID, "Sync failed: "+b.translate(serr))
		return werr
	}
	_, werr := b.sendText(ctx, msg.Chat.ID, fmt.Sprintf("Sync done, %d dialogs.", n))
	return werr
}

// cmdLoginLogout handles /login and /logout (admins only).
//
// The login wizard's state machine itself lives in wizard.go; this function
// owns the permission check and the entry text.
func (b *Bot) cmdLoginLogout(ctx context.Context, msg *models.Message, cmd string) error {
	if !isAdmin(b.cfg, senderID(msg)) {
		_, err := b.sendText(ctx, msg.Chat.ID, fmt.Sprintf("Not allowed: /%s is admin only.", cmd))
		return err
	}
	if cmd == "logout" {
		// Stop a wizard in progress so no login state outlives the logout.
		b.login.clear(msg.Chat.ID)
		logoutFn := b.getLogout()
		if logoutFn == nil {
			_, err := b.sendText(ctx, msg.Chat.ID, "Logout not wired: this process cannot delete the session file.")
			return err
		}
		if lerr := logoutFn(ctx); lerr != nil {
			_, err := b.sendText(ctx, msg.Chat.ID, "Failed to delete the session: "+b.translate(lerr))
			return err
		}
		_, err := b.sendText(ctx, msg.Chat.ID,
			"Session file deleted.\nNote: this process still holds the established connection, so it only really "+
				"becomes signed out after a restart. Telegram's \"active sessions\" also have to be terminated "+
				"by hand on your phone; this bot cannot revoke them for you.")
		return err
	}

	// /login: actually enter the wizard (before this it only replied with a static text and the
	// phone number was treated as an ordinary message).
	reply, err := b.login.Start(msg.Chat.ID, senderID(msg))
	if err != nil {
		_, serr := b.sendText(ctx, msg.Chat.ID, b.translate(err))
		return serr
	}
	_, serr := b.sendText(ctx, msg.Chat.ID,
		"Login wizard started. Login code and 2FA password messages are deleted automatically, so do not "+
			"use this command in a public group.\nSend /cancel at any time to leave the wizard.\n\n"+reply)
	return serr
}

// senderID returns the message sender's ID; 0 when there is no sender (channel messages etc.).
func senderID(msg *models.Message) int64 {
	if msg == nil || msg.From == nil {
		return 0
	}
	return msg.From.ID
}

// commandArgs returns the argument slice after the command name (the @botname suffix already
// stripped).
func commandArgs(text string) []string {
	fields := strings.Fields(strings.TrimSpace(text))
	if len(fields) <= 1 {
		return nil
	}
	return fields[1:]
}
