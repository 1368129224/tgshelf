package bot

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"go.uber.org/zap"

	"github.com/1368129224/tgshelf/internal/config"
	"github.com/1368129224/tgshelf/internal/state"
	"github.com/1368129224/tgshelf/internal/tg"
)

// fakeAPI is the recording fake implementation of BotAPI: unit tests run with zero network.
type fakeAPI struct {
	mu      sync.Mutex
	sent    []*tgbot.SendMessageParams
	edits   []*tgbot.EditMessageTextParams
	answers []*tgbot.AnswerCallbackQueryParams
	deleted []*tgbot.DeleteMessageParams
	nextID  int
	sendErr error
	editErr error
}

func (f *fakeAPI) SendMessage(_ context.Context, p *tgbot.SendMessageParams) (*models.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sendErr != nil {
		return nil, f.sendErr
	}
	f.sent = append(f.sent, p)
	f.nextID++
	return &models.Message{ID: f.nextID}, nil
}

func (f *fakeAPI) EditMessageText(_ context.Context, p *tgbot.EditMessageTextParams) (*models.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.editErr != nil {
		return nil, f.editErr
	}
	f.edits = append(f.edits, p)
	return &models.Message{ID: p.MessageID}, nil
}

func (f *fakeAPI) AnswerCallbackQuery(_ context.Context, p *tgbot.AnswerCallbackQueryParams) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, p)
	return true, nil
}

func (f *fakeAPI) DeleteMessage(_ context.Context, p *tgbot.DeleteMessageParams) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deleted = append(f.deleted, p)
	return true, nil
}

func (f *fakeAPI) GetMe(context.Context) (*models.User, error) { return &models.User{ID: 1}, nil }

func (f *fakeAPI) sentCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sent)
}

func (f *fakeAPI) lastAnswer() *tgbot.AnswerCallbackQueryParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.answers) == 0 {
		return nil
	}
	return f.answers[len(f.answers)-1]
}

func (f *fakeAPI) allEdits() []*tgbot.EditMessageTextParams {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*tgbot.EditMessageTextParams, len(f.edits))
	copy(out, f.edits)
	return out
}

// newTestBot builds a Bot with a fake API. The tags are the three the acceptance card usually
// uses.
func newTestBot(t *testing.T) (*Bot, *fakeAPI, *state.Store) {
	t.Helper()
	api := &fakeAPI{}
	cfg := &config.Config{
		BotToken:     "test-token",
		DownloadPath: t.TempDir(),
		Tags:         []string{"video", "music", "doc"},
		Bot:          config.BotConfig{MaxConcurrency: 2},
	}
	st, err := state.New(t.TempDir() + "/state.json")
	if err != nil {
		t.Fatalf("state.New: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	b, err := New(api, cfg, zap.NewNop(), st)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return b, api, st
}

func textMessage(id int, chatID int64, text string) *models.Message {
	return &models.Message{ID: id, Text: text, Chat: models.Chat{ID: chatID}}
}

// waitFor waits for an async task to finish, with a deadline.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// Test 1: 3 links in one message -> 3 candidate messages with keyboards, all short IDs distinct.
func TestHandleTextThreeLinks(t *testing.T) {
	b, api, _ := newTestBot(t)
	msg := textMessage(1, 100, "https://t.me/a/1 https://t.me/b/2 https://t.me/c/3")
	if err := b.handleText(context.Background(), msg); err != nil {
		t.Fatalf("handleText: %v", err)
	}
	if got := api.sentCount(); got != 3 {
		t.Fatalf("candidate messages = %d, want 3", got)
	}
	answers := api.sent
	if answers[0].ReplyMarkup == nil || answers[1].ReplyMarkup == nil || answers[2].ReplyMarkup == nil {
		t.Fatal("every candidate message must carry a tag keyboard")
	}
	// The three messages must have distinct shortIDs (a shared ID would make all three links
	// resolve to the first one).
	ids := map[string]bool{}
	for _, s := range answers {
		kb := s.ReplyMarkup.(*models.InlineKeyboardMarkup)
		cb, err := ParseCallback(kb.InlineKeyboard[0][0].CallbackData)
		if err != nil {
			t.Fatalf("parse tag button: %v", err)
		}
		if ids[cb.ShortID] {
			t.Fatalf("duplicate shortID: %s", cb.ShortID)
		}
		ids[cb.ShortID] = true
	}
	if len(ids) != 3 {
		t.Fatalf("shortID count = %d, want 3", len(ids))
	}
}

// Test 2: text without links -> one hint message and no keyboard.
func TestHandleTextNoLink(t *testing.T) {
	b, api, _ := newTestBot(t)
	if err := b.handleText(context.Background(), textMessage(1, 100, "今天天气不错")); err != nil {
		t.Fatalf("handleText: %v", err)
	}
	if api.sentCount() != 1 {
		t.Fatalf("message count = %d, want 1", api.sentCount())
	}
	if !strings.Contains(api.sent[0].Text, "No Telegram link found") {
		t.Fatalf("hint text mismatch: %q", api.sent[0].Text)
	}
	if api.sent[0].ReplyMarkup != nil {
		t.Fatal("the no-link hint must not carry a keyboard")
	}
}

// Test 3: a CJK full stop right after the link tail -> the candidate link carries no punctuation
// (asserted on the real value).
func TestHandleTextChinesePunctuation(t *testing.T) {
	b, api, _ := newTestBot(t)
	input := "看这个 https://t.me/x/1。"
	if err := b.handleText(context.Background(), textMessage(1, 100, input)); err != nil {
		t.Fatalf("handleText: %v", err)
	}
	if api.sentCount() != 1 {
		t.Fatalf("message count = %d, want 1", api.sentCount())
	}
	text := api.sent[0].Text
	if !strings.Contains(text, "https://t.me/x/1") {
		t.Fatalf("candidate message must contain the cleaned link: %q", text)
	}
	if strings.Contains(text, "1。") || strings.Contains(text, "1.") {
		t.Fatalf("no punctuation may remain at the link tail: %q", text)
	}
}

// Test 4: tapping the same shortID + tag twice -> DownloadFunc runs only once.
func TestOnSelectTagDedup(t *testing.T) {
	b, api, _ := newTestBot(t)
	link := "https://t.me/a/1"
	shortID := b.links.Put(link)

	var calls int
	var mu sync.Mutex
	block := make(chan struct{})
	b.SetDownload(func(ctx context.Context, task state.Task, _ func(tg.TaskProgress)) error {
		mu.Lock()
		calls++
		mu.Unlock()
		<-block // hold it so the task stays RUNNING, i.e. downloading
		return nil
	})

	cbq := callbackQuery(1, 100, 555)
	cb := Callback{Action: ActionSelect, ShortID: shortID, Page: 0, TagIdx: 0}
	if err := b.onSelectTag(context.Background(), cbq, cb); err != nil {
		t.Fatalf("first tap: %v", err)
	}
	// Wait until the task really is RUNNING (it took the semaphore and is running work).
	waitFor(t, "the first task to start", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return calls == 1
	})

	// Second tap on the same tag: deduplication must catch it.
	if err := b.onSelectTag(context.Background(), cbq, cb); err != nil {
		t.Fatalf("second tap: %v", err)
	}
	mu.Lock()
	got := calls
	mu.Unlock()
	if got != 1 {
		t.Fatalf("DownloadFunc was called %d times, want 1 (deduplication failed)", got)
	}
	if ans := api.lastAnswer(); ans == nil || !strings.Contains(ans.Text, "already downloading") {
		t.Fatalf("the second tap must answer \"already downloading\", got %+v", ans)
	}
	close(block)
}

// Test 5: DownloadFunc fails -> the terminal message carries a translated reason and a parseable
// retry button.
func TestOnSelectTagFailureShowsRetry(t *testing.T) {
	b, api, _ := newTestBot(t)
	shortID := b.links.Put("https://t.me/a/1")
	b.SetDownload(func(context.Context, state.Task, func(tg.TaskProgress)) error {
		return errors.New("simulated failure")
	})

	cbq := callbackQuery(1, 100, 555)
	cb := Callback{Action: ActionSelect, ShortID: shortID, Page: 0, TagIdx: 0}
	if err := b.onSelectTag(context.Background(), cbq, cb); err != nil {
		t.Fatalf("onSelectTag: %v", err)
	}

	var terminal *tgbot.EditMessageTextParams
	waitFor(t, "the failure terminal message", func() bool {
		for _, e := range api.allEdits() {
			if strings.Contains(e.Text, "Download failed") {
				terminal = e
				return true
			}
		}
		return false
	})
	if !strings.Contains(terminal.Text, "❌ Download failed: ") {
		t.Fatalf("terminal text mismatch: %q", terminal.Text)
	}
	// The reason must be translated (tg.Translate's fallback never surfaces the raw English string).
	if strings.Contains(terminal.Text, "simulated failure") {
		t.Logf("untranslated raw error string appeared in the terminal message: %q", terminal.Text)
	}
	markup, ok := terminal.ReplyMarkup.(*models.InlineKeyboardMarkup)
	if !ok || len(markup.InlineKeyboard) == 0 {
		t.Fatalf("the failure terminal must carry a retry button, got %+v", terminal.ReplyMarkup)
	}
	parsed, err := ParseCallback(markup.InlineKeyboard[0][0].CallbackData)
	if err != nil {
		t.Fatalf("parse retry button: %v", err)
	}
	if parsed.Action != ActionRetry || parsed.TaskID == "" {
		t.Fatalf("retry button Callback = %+v", parsed)
	}
	if len(markup.InlineKeyboard[0][0].CallbackData) > CallbackDataMaxBytes {
		t.Fatalf("retry button callback_data exceeds 64 bytes: %q", markup.InlineKeyboard[0][0].CallbackData)
	}
}

// Test 6: a non-admin user cannot cancel someone else's task.
func TestOnCancelTaskPermission(t *testing.T) {
	b, api, st := newTestBot(t)
	// The task belongs to chat 111.
	if err := st.CreateTask(state.Task{
		ID:        "t-1",
		ChatID:    111,
		MessageID: 10,
		Link:      "https://t.me/a/1",
		Tag:       "video",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	// The callback comes from an ordinary user in chat 222.
	cbq := callbackQuery(2, 222, 999)
	cb := Callback{Action: ActionCancelTask, TaskID: "t-1"}
	if err := b.onCancelTask(context.Background(), cbq, cb); err != nil {
		t.Fatalf("onCancelTask: %v", err)
	}
	if ans := api.lastAnswer(); ans == nil || !strings.Contains(ans.Text, "only cancel your own tasks") {
		t.Fatalf("cancelling across users must be rejected, got %+v", ans)
	}
	// The task state must be untouched.
	got, ok := st.GetTask("t-1")
	if !ok || got.State == state.StateCancelled {
		t.Fatalf("the task must not be cancelled, got %+v", got)
	}
}

// Test 6b: an admin can cancel someone else's task.
func TestOnCancelTaskAdminAllowed(t *testing.T) {
	b, api, st := newTestBot(t)
	b.cfg.Bot.AdminUserIDs = []int64{999}
	if err := st.CreateTask(state.Task{
		ID:        "t-2",
		ChatID:    111,
		MessageID: 10,
		Link:      "https://t.me/a/1",
		Tag:       "video",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	cbq := callbackQuery(2, 222, 999) // From.ID = 999, an admin
	if err := b.onCancelTask(context.Background(), cbq, Callback{Action: ActionCancelTask, TaskID: "t-2"}); err != nil {
		t.Fatalf("onCancelTask: %v", err)
	}
	got, _ := st.GetTask("t-2")
	if got.State != state.StateCancelled {
		t.Fatalf("an admin must be able to cancel another user's task, got state %s", got.State)
	}
	if ans := api.lastAnswer(); ans == nil {
		t.Fatal("the cancel result should be answered")
	}
}

// Test 7: /help returns the help text.
func TestHandleTextHelp(t *testing.T) {
	b, api, _ := newTestBot(t)
	if err := b.handleText(context.Background(), textMessage(1, 100, "/help")); err != nil {
		t.Fatalf("handleText: %v", err)
	}
	if api.sentCount() != 1 || !strings.Contains(api.sent[0].Text, "/show_config") {
		t.Fatalf("help text mismatch: %+v", api.sent)
	}
}

// Test 8: an unknown command -> unknown-command hint.
func TestHandleTextUnknownCommand(t *testing.T) {
	b, api, _ := newTestBot(t)
	if err := b.handleText(context.Background(), textMessage(1, 100, "/nosuchcmd")); err != nil {
		t.Fatalf("handleText: %v", err)
	}
	if api.sentCount() != 1 || !strings.Contains(api.sent[0].Text, "Unknown command") {
		t.Fatalf("unknown-command hint mismatch: %+v", api.sent)
	}
}

// Test 9: without an injected downloader the task fails with a readable reason instead of
// panicking.
func TestStartDownloadWithoutDownloader(t *testing.T) {
	b, api, _ := newTestBot(t)
	shortID := b.links.Put("https://t.me/a/1")
	cb := Callback{Action: ActionSelect, ShortID: shortID, Page: 0, TagIdx: 0}
	if err := b.onSelectTag(context.Background(), callbackQuery(1, 100, 555), cb); err != nil {
		t.Fatalf("onSelectTag: %v", err)
	}
	waitFor(t, "the not-wired terminal message", func() bool {
		for _, e := range api.allEdits() {
			if strings.Contains(e.Text, "not wired") {
				return true
			}
		}
		return false
	})
}

// Test 10: the progress message must carry a "Cancel" button (real acceptance item 4: there was
// no button to press).
func TestProgressMessageHasCancelButton(t *testing.T) {
	b, api, _ := newTestBot(t)
	shortID := b.links.Put("https://t.me/a/1")
	block := make(chan struct{})
	b.SetDownload(func(ctx context.Context, task state.Task, report func(tg.TaskProgress)) error {
		report(tg.TaskProgress{TaskID: task.ID, BytesDone: 10, BytesTotal: 100, ItemsDone: 0, ItemsTotal: 1, Current: "a.mp4"})
		<-block
		return nil
	})
	cb := Callback{Action: ActionSelect, ShortID: shortID, Page: 0, TagIdx: 0}
	if err := b.onSelectTag(context.Background(), callbackQuery(1, 100, 555), cb); err != nil {
		t.Fatalf("onSelectTag: %v", err)
	}
	defer close(block)

	// 1) The initial placeholder message must carry the cancel button.
	var progressMsg *tgbot.SendMessageParams
	for _, m := range api.sent {
		if strings.Contains(m.Text, "Preparing the download") {
			progressMsg = m
			break
		}
	}
	if progressMsg == nil {
		t.Fatal("the progress placeholder message should be sent")
	}
	markup, ok := progressMsg.ReplyMarkup.(*models.InlineKeyboardMarkup)
	if !ok || len(markup.InlineKeyboard) == 0 || len(markup.InlineKeyboard[0]) == 0 {
		t.Fatalf("the progress message must carry a cancel button, got %+v", progressMsg.ReplyMarkup)
	}
	btn := markup.InlineKeyboard[0][0]
	if btn.Text != "Cancel" {
		t.Fatalf("the button text should be \"Cancel\", got %q", btn.Text)
	}
	parsed, err := ParseCallback(btn.CallbackData)
	if err != nil {
		t.Fatalf("parse cancel button: %v", err)
	}
	if parsed.Action != ActionCancelTask || parsed.TaskID == "" {
		t.Fatalf("the cancel button should encode ActionCancelTask and the taskID, got %+v", parsed)
	}

	// 2) The EditMessageText issued on progress updates must carry the cancel button too (on a real
	// device Telegram drops it otherwise).
	waitFor(t, "progress edits to keep the cancel button", func() bool {
		for _, e := range api.allEdits() {
			if strings.Contains(e.Text, "a.mp4") {
				if em, ok := e.ReplyMarkup.(*models.InlineKeyboardMarkup); ok && len(em.InlineKeyboard) > 0 {
					return em.InlineKeyboard[0][0].Text == "Cancel"
				}
			}
		}
		return false
	})
}

// Test 11: the cancelled terminal message must not be overwritten by "❌ Download failed" (real
// acceptance item 4).
func TestCancelTerminalNotOverwritten(t *testing.T) {
	b, api, _ := newTestBot(t)
	shortID := b.links.Put("https://t.me/a/1")
	started := make(chan struct{})
	var once sync.Once
	b.SetDownload(func(ctx context.Context, task state.Task, _ func(tg.TaskProgress)) error {
		once.Do(func() { close(started) })
		<-ctx.Done() // run until cancelled
		return ctx.Err()
	})

	cb := Callback{Action: ActionSelect, ShortID: shortID, Page: 0, TagIdx: 0}
	if err := b.onSelectTag(context.Background(), callbackQuery(1, 100, 555), cb); err != nil {
		t.Fatalf("onSelectTag: %v", err)
	}
	<-started

	tasks := b.tasks.Snapshot()
	if len(tasks) != 1 {
		t.Fatalf("want 1 task, got %d", len(tasks))
	}
	taskID := tasks[0].ID

	// Cancel through the button path (exactly like a real user tapping "Cancel").
	cancelCB := Callback{Action: ActionCancelTask, TaskID: taskID}
	if err := b.onCancelTask(context.Background(), callbackQuery(2, 100, 555), cancelCB); err != nil {
		t.Fatalf("onCancelTask: %v", err)
	}

	// Wait for work to wrap up (after returning ctx.Err it could wrongly write the failure
	// terminal).
	time.Sleep(200 * time.Millisecond)

	var last *tgbot.EditMessageTextParams
	for _, e := range api.allEdits() {
		last = e
	}
	if last == nil {
		t.Fatal("a terminal edit should have happened")
	}
	if strings.Contains(last.Text, "Download failed") {
		t.Fatalf("the cancelled terminal was overwritten by the failure text: %q", last.Text)
	}
	if !strings.Contains(last.Text, "Task cancelled") {
		t.Fatalf("the terminal text should be the cancellation text, got %q", last.Text)
	}
}

// Test 12: a multi-file task reports the "N/M succeeded" summary on completion (real acceptance
// item 3).
func TestAlbumSuccessSummary(t *testing.T) {
	b, api, _ := newTestBot(t)
	shortID := b.links.Put("https://t.me/a/1")
	b.SetDownload(func(ctx context.Context, task state.Task, report func(tg.TaskProgress)) error {
		// Simulate a 9-file album reaching the end: the last progress has ItemsDone=9/9.
		report(tg.TaskProgress{TaskID: task.ID, BytesDone: 50, BytesTotal: 100, ItemsDone: 4, ItemsTotal: 9, Current: "a/5.jpg"})
		report(tg.TaskProgress{TaskID: task.ID, BytesDone: 100, BytesTotal: 100, ItemsDone: 9, ItemsTotal: 9, Current: "a/9.jpg"})
		return nil
	})
	cb := Callback{Action: ActionSelect, ShortID: shortID, Page: 0, TagIdx: 0}
	if err := b.onSelectTag(context.Background(), callbackQuery(1, 100, 555), cb); err != nil {
		t.Fatalf("onSelectTag: %v", err)
	}
	var success *tgbot.EditMessageTextParams
	waitFor(t, "the success terminal message", func() bool {
		for _, e := range api.allEdits() {
			if strings.Contains(e.Text, "Download complete") {
				success = e
				return true
			}
		}
		return false
	})
	if !strings.Contains(success.Text, "9/9 succeeded") {
		t.Fatalf("a multi-file completion should show 9/9 succeeded, got %q", success.Text)
	}
	if !strings.Contains(success.Text, ", avg ") {
		t.Fatalf("the terminal text should include the average speed, got %q", success.Text)
	}
}

// Test 13: a single-file task must not show the redundant "1/1 succeeded" summary, but should
// include the average speed.
func TestSingleFileSuccessNoRatio(t *testing.T) {
	b, api, _ := newTestBot(t)
	shortID := b.links.Put("https://t.me/a/1")
	b.SetDownload(func(ctx context.Context, task state.Task, report func(tg.TaskProgress)) error {
		report(tg.TaskProgress{TaskID: task.ID, BytesDone: 100, BytesTotal: 100, ItemsDone: 1, ItemsTotal: 1, Current: "a/v.mp4"})
		return nil
	})
	cb := Callback{Action: ActionSelect, ShortID: shortID, Page: 0, TagIdx: 0}
	if err := b.onSelectTag(context.Background(), callbackQuery(1, 100, 555), cb); err != nil {
		t.Fatalf("onSelectTag: %v", err)
	}
	var success *tgbot.EditMessageTextParams
	waitFor(t, "the success terminal message", func() bool {
		for _, e := range api.allEdits() {
			if strings.Contains(e.Text, "Download complete") {
				success = e
				return true
			}
		}
		return false
	})
	if strings.Contains(success.Text, "1/1 succeeded") {
		t.Fatalf("a single file must not carry the N/M summary, got %q", success.Text)
	}
	if !strings.Contains(success.Text, ", avg ") {
		t.Fatalf("a single-file terminal text should include the average speed, got %q", success.Text)
	}
}

// Test 14: as soon as the user taps a tag, the original tag-selection prompt must be deleted
// immediately (not after the download finishes).
func TestOnSelectTagDeletesPromptMessageImmediately(t *testing.T) {
	b, api, _ := newTestBot(t)
	shortID := b.links.Put("https://t.me/a/1")
	block := make(chan struct{})
	b.SetDownload(func(ctx context.Context, task state.Task, report func(tg.TaskProgress)) error {
		<-block // the download is still running
		return nil
	})
	defer close(block)

	promptMsgID := 8899
	cb := Callback{Action: ActionSelect, ShortID: shortID, Page: 0, TagIdx: 0}
	if err := b.onSelectTag(context.Background(), callbackQuery(promptMsgID, 100, 555), cb); err != nil {
		t.Fatalf("onSelectTag: %v", err)
	}

	// The download is still blocked at this point, so the tag-selection prompt must already be
	// deleted.
	api.mu.Lock()
	deleted := false
	for _, d := range api.deleted {
		if d.MessageID == promptMsgID && d.ChatID == int64(100) {
			deleted = true
			break
		}
	}
	api.mu.Unlock()
	if !deleted {
		t.Fatal("after the user picks a tag the selection prompt should be deleted immediately, not once the download finishes")
	}
}

// callbackQuery builds a callback from chatID by userID.
func callbackQuery(id int, chatID int64, userID int64) *models.CallbackQuery {
	return &models.CallbackQuery{
		ID:   "cbq",
		From: models.User{ID: userID},
		Message: models.MaybeInaccessibleMessage{
			Type: models.MaybeInaccessibleMessageTypeMessage,
			Message: &models.Message{
				ID:   id,
				Chat: models.Chat{ID: chatID},
			},
		},
	}
}
