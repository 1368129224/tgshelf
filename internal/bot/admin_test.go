package bot

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"
	"go.uber.org/zap"

	"github.com/1368129224/tgshelf/internal/config"
	"github.com/1368129224/tgshelf/internal/state"
	"github.com/1368129224/tgshelf/internal/tg"
)

// userWithID builds a sender with only an ID (authorization only looks at the ID).
func userWithID(id int64) *models.User { return &models.User{ID: id} }

// newAdminTestBot builds a Bot with a chosen concurrency (for the queued-cancel scenarios).
func newAdminTestBot(t *testing.T, maxConc int) (*Bot, *fakeAPI, *state.Store) {
	t.Helper()
	api := &fakeAPI{}
	cfg := &config.Config{
		BotToken:     "123456:SECRET-BOT-TOKEN",
		DownloadPath: t.TempDir(),
		Tags:         []string{"video", "music"},
		ProxyURL:     "socks5://user:pass@127.0.0.1:1080",
		Bot:          config.BotConfig{MaxConcurrency: maxConc},
		Download:     config.DownloadConfig{Threads: 4, FileConcurrency: 2},
		Telegram:     config.TelegramConfig{APIHash: "SECRET-API-HASH"},
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

// Test 1: the various maskProxy inputs (ported from the old Python mask_proxy cases).
func TestMaskProxy(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"empty string", "", "not set"},
		{"whitespace only", "   ", "not set"},
		{"with credentials", "socks5://user:pass@127.0.0.1:1080", "socks5://***@127.0.0.1:1080"},
		{"with credentials, no port", "http://a:b@proxy.example.com", "http://***@proxy.example.com"},
		{"no credentials, with scheme", "socks5://127.0.0.1:1080", "socks5://***"},
		{"no scheme", "127.0.0.1:1080", "***"},
		{"credentials with @ keep the last one", "http://user@dom:pass@host:1", "http://***@host:1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := maskProxy(c.in); got != c.want {
				t.Fatalf("maskProxy(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// Test 2: /show_config contains no sensitive value.
func TestShowConfigNoSecrets(t *testing.T) {
	b, _, _ := newAdminTestBot(t, 2)
	text := b.showConfigText(context.Background())

	secrets := []string{
		b.cfg.BotToken, // 123456:SECRET-BOT-TOKEN
		b.cfg.Telegram.APIHash,
		"user:pass", // the raw proxy credentials
		"SECRET",
	}
	for _, s := range secrets {
		if s == "" {
			continue
		}
		if strings.Contains(text, s) {
			t.Fatalf("the config output leaks the sensitive value %q:\n%s", s, text)
		}
	}
	// Everything that should be shown must be there.
	for _, want := range []string{"Download folder", "Tags", "Concurrent tasks", "Threads per file", "Proxy", "Account"} {
		if !strings.Contains(text, want) {
			t.Fatalf("the config output is missing %q:\n%s", want, text)
		}
	}
	// The proxy must be masked.
	if !strings.Contains(text, "socks5://***@127.0.0.1:1080") {
		t.Fatalf("the proxy was not masked as expected:\n%s", text)
	}
}

// Test 3: /status shows RUNNING and queued PENDING tasks alike.
func TestStatusShowsRunningAndPending(t *testing.T) {
	b, api, _ := newAdminTestBot(t, 1)

	var mu sync.Mutex
	running := 0
	block := make(chan struct{})
	b.SetDownload(func(ctx context.Context, task state.Task, _ func(tg.TaskProgress)) error {
		mu.Lock()
		running++
		mu.Unlock()
		<-block
		return nil
	})

	now := time.Now()
	for _, id := range []string{"task-a", "task-b"} {
		if err := b.store.CreateTask(state.Task{
			ID: id, ChatID: 100, MessageID: 1, Link: "https://t.me/x/1",
			Tag: "video", CreatedAt: now,
		}); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
	}
	// task-a moves to RUNNING, task-b stays PENDING.
	if err := b.store.TransitionTask("task-a", state.StateRunning); err != nil {
		t.Fatalf("Transition: %v", err)
	}

	msg := textMessage(1, 100, "/status")
	msg.From = userWithID(1)
	if err := b.handleText(context.Background(), msg); err != nil {
		t.Fatalf("handleText: %v", err)
	}
	out := api.sent[len(api.sent)-1].Text
	if !strings.Contains(out, "Downloading") {
		t.Fatalf("/status did not show the RUNNING group:\n%s", out)
	}
	if !strings.Contains(out, "Queued") {
		t.Fatalf("/status did not show the queued tasks:\n%s", out)
	}
	if !strings.Contains(out, "task-b") {
		t.Fatalf("/status did not list the queued task task-b:\n%s", out)
	}
	_ = running
}

// Test 4: with no tasks /status gives an explicit text, not a blank message.
func TestStatusEmpty(t *testing.T) {
	b, api, _ := newAdminTestBot(t, 2)
	msg := textMessage(1, 100, "/status")
	msg.From = userWithID(1)
	if err := b.handleText(context.Background(), msg); err != nil {
		t.Fatalf("handleText: %v", err)
	}
	out := strings.TrimSpace(api.sent[len(api.sent)-1].Text)
	if out == "" {
		t.Fatal("/status must not reply with a blank message when there are no tasks")
	}
	if !strings.Contains(out, "No tasks") {
		t.Fatalf("/status no-task text mismatch: %q", out)
	}
}

// Test 5: cancelling while queued really prevents the task from starting.
func TestQueuedCancelPreventsStart(t *testing.T) {
	b, _, _ := newAdminTestBot(t, 1) // concurrency 1: the second task must queue

	var mu sync.Mutex
	var startedB int
	blockA := make(chan struct{})
	b.SetDownload(func(ctx context.Context, task state.Task, _ func(tg.TaskProgress)) error {
		if task.Tag == "music" {
			mu.Lock()
			startedB++
			mu.Unlock()
			return nil
		}
		<-blockA // hold the only slot
		return nil
	})

	// A starts and takes the slot.
	taskA, err := b.startDownload(context.Background(), 100, "https://t.me/a/1", "video")
	if err != nil {
		t.Fatalf("startDownload A: %v", err)
	}
	waitFor(t, "A to reach RUNNING", func() bool {
		got, ok := b.tasks.Get(taskA)
		return ok && got.State == state.StateRunning
	})

	// B can only queue.
	taskB, err := b.startDownload(context.Background(), 100, "https://t.me/b/2", "music")
	if err != nil {
		t.Fatalf("startDownload B: %v", err)
	}
	gotB, _ := b.tasks.Get(taskB)
	if gotB.State != state.StatePending {
		t.Fatalf("B should be PENDING, got %s", gotB.State)
	}

	// Cancel B while it is queued.
	msg := textMessage(1, 100, "/cancel "+taskB)
	msg.From = userWithID(1)
	if err := b.handleText(context.Background(), msg); err != nil {
		t.Fatalf("/cancel: %v", err)
	}

	// Release the slot and leave an observation window for a wrong start.
	close(blockA)
	time.Sleep(300 * time.Millisecond)

	mu.Lock()
	got := startedB
	mu.Unlock()
	if got != 0 {
		t.Fatalf("a task cancelled while queued still started %d times", got)
	}
	final, _ := b.tasks.Get(taskB)
	if final.State != state.StateCancelled {
		t.Fatalf("B's terminal state should be CANCELLED, got %s", final.State)
	}
}

// Test 6: /cancel can only cancel your own tasks (non-admin).
func TestCancelOtherUserDenied(t *testing.T) {
	b, api, st := newAdminTestBot(t, 2)
	if err := st.CreateTask(state.Task{
		ID: "other", ChatID: 999, MessageID: 1, Link: "https://t.me/a/1",
		Tag: "video", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	msg := textMessage(1, 100, "/cancel other")
	msg.From = userWithID(1) // not an admin
	if err := b.handleText(context.Background(), msg); err != nil {
		t.Fatalf("handleText: %v", err)
	}
	if out := api.sent[len(api.sent)-1].Text; !strings.Contains(out, "only cancel your own tasks") {
		t.Fatalf("cancelling across users should be rejected, got %q", out)
	}
	got, _ := st.GetTask("other")
	if got.State == state.StateCancelled {
		t.Fatal("another user's task must not be cancelled")
	}
}

// Test 7: /cancel on a terminal task answers "task already finished".
func TestCancelTerminal(t *testing.T) {
	b, api, st := newAdminTestBot(t, 2)
	if err := st.CreateTask(state.Task{
		ID: "done-1", ChatID: 100, MessageID: 1, Link: "https://t.me/a/1",
		Tag: "video", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	// The state machine requires PENDING -> RUNNING -> DONE; it cannot jump straight to a terminal
	// state.
	if err := st.TransitionTask("done-1", state.StateRunning); err != nil {
		t.Fatalf("Transition RUNNING: %v", err)
	}
	if err := st.TransitionTask("done-1", state.StateDone); err != nil {
		t.Fatalf("Transition DONE: %v", err)
	}
	msg := textMessage(1, 100, "/cancel done-1")
	msg.From = userWithID(1)
	if err := b.handleText(context.Background(), msg); err != nil {
		t.Fatalf("handleText: %v", err)
	}
	if out := api.sent[len(api.sent)-1].Text; !strings.Contains(out, "Task already finished") {
		t.Fatalf("cancelling a terminal task should answer \"task already finished\", got %q", out)
	}
}

// Test 8: a non-admin is rejected by /sync /login /logout /show_config.
func TestAdminCommandsRequireAdmin(t *testing.T) {
	b, api, _ := newAdminTestBot(t, 2)
	for _, cmd := range []string{"/sync", "/login", "/logout", "/show_config"} {
		api.sent = nil
		msg := textMessage(1, 100, cmd)
		msg.From = userWithID(1) // not in admin_user_ids
		if err := b.handleText(context.Background(), msg); err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		out := api.sent[len(api.sent)-1].Text
		if !strings.Contains(out, "Not allowed") {
			t.Fatalf("%s should be rejected for a non-admin, got %q", cmd, out)
		}
	}
}

// Test 9: an admin's /sync goes through the injected implementation.
func TestSyncAdmin(t *testing.T) {
	b, api, _ := newAdminTestBot(t, 2)
	b.cfg.Bot.AdminUserIDs = []int64{42}
	called := 0
	b.SetSync(func(ctx context.Context, full bool) (int, error) {
		called++
		return 7, nil
	})
	msg := textMessage(1, 100, "/sync")
	msg.From = userWithID(42)
	if err := b.handleText(context.Background(), msg); err != nil {
		t.Fatalf("handleText: %v", err)
	}
	if called != 1 {
		t.Fatalf("SyncFunc was called %d times, want 1", called)
	}
	if out := api.sent[len(api.sent)-1].Text; !strings.Contains(out, "7") {
		t.Fatalf("the sync result was not reported: %q", out)
	}
}

// Test 14: startup notifies the allow-listed users (used to tell a live process from a dead one
// when "nothing happens").
func TestNotifyStartup(t *testing.T) {
	b, api, _ := newAdminTestBot(t, 2)
	b.cfg.Bot.AllowedUserIDs = []int64{111, 222}
	b.notifyStartup(context.Background())

	if api.sentCount() != 2 {
		t.Fatalf("2 startup notifications should be sent, got %d", api.sentCount())
	}
	for i, p := range api.sent {
		if p.ChatID != int64(111+i*111) {
			t.Fatalf("notification %d should go to the allow-listed user, got %v", i, p.ChatID)
		}
		if !strings.Contains(p.Text, "tgshelf is up and running") {
			t.Fatalf("notification text mismatch: %q", p.Text)
		}
		// A notification must never contain a secret.
		if strings.Contains(p.Text, b.cfg.BotToken) || strings.Contains(p.Text, "SECRET") {
			t.Fatalf("the startup notification leaks a secret: %q", p.Text)
		}
	}
}

// Test 15: with an empty allow-list it falls back to admins; when neither exists it sends nothing.
func TestNotifyStartupFallback(t *testing.T) {
	b, api, _ := newAdminTestBot(t, 2)
	b.cfg.Bot.AllowedUserIDs = nil
	b.cfg.Bot.AdminUserIDs = []int64{999}
	b.notifyStartup(context.Background())
	if api.sentCount() != 1 || api.sent[0].ChatID != int64(999) {
		t.Fatalf("it should fall back to admin 999, got %+v", api.sent)
	}

	api.sent = nil
	b.cfg.Bot.AdminUserIDs = nil
	b.notifyStartup(context.Background())
	if api.sentCount() != 0 {
		t.Fatalf("no message should be sent without recipients, got %d", api.sentCount())
	}
}

// fakeLoginAPI is a zero-network implementation of LoginAPI.
type fakeLoginAPI struct {
	mu        sync.Mutex
	codeSent  int
	signedIn  int
	need2FA   bool
	lastPhone string
	lastCode  string
	lastPass  string
	sendErr   error
}

func (f *fakeLoginAPI) SendCode(_ context.Context, phone string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.codeSent++
	f.lastPhone = phone
	if f.sendErr != nil {
		return "", f.sendErr
	}
	return "hash-" + phone, nil
}

func (f *fakeLoginAPI) SignIn(_ context.Context, _, _, code string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signedIn++
	f.lastCode = code
	return f.need2FA, nil
}

func (f *fakeLoginAPI) CheckPassword(_ context.Context, password string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastPass = password
	return nil
}

// Test 11: an admin's /login really enters the wizard and the phone number is no longer treated as
// an ordinary message.
func TestLoginWizardEntryAndPhone(t *testing.T) {
	b, api, _ := newAdminTestBot(t, 2)
	b.cfg.Bot.AdminUserIDs = []int64{42}
	fake := &fakeLoginAPI{}
	b.SetLoginAPI(func(context.Context) (LoginAPI, error) { return fake, nil })

	// /login
	msg := textMessage(1, 100, "/login")
	msg.From = userWithID(42)
	if err := b.handleText(context.Background(), msg); err != nil {
		t.Fatalf("/login: %v", err)
	}
	if out := api.sent[len(api.sent)-1].Text; !strings.Contains(out, "Login wizard started") {
		t.Fatalf("/login should enter the wizard, got %q", out)
	}

	// Phone number: it must go to the wizard, not down the link branch.
	phoneMsg := textMessage(2, 100, "+8613800138000")
	phoneMsg.From = userWithID(42)
	if err := b.handleText(context.Background(), phoneMsg); err != nil {
		t.Fatalf("phone: %v", err)
	}
	fake.mu.Lock()
	sent := fake.codeSent
	lastPhone := fake.lastPhone
	fake.mu.Unlock()
	if sent != 1 {
		t.Fatalf("SendCode should be called once, got %d", sent)
	}
	if lastPhone != "+8613800138000" {
		t.Fatalf("the phone number was parsed wrong: %q", lastPhone)
	}
	out := api.sent[len(api.sent)-1].Text
	if !strings.Contains(out, "Login code sent") {
		t.Fatalf("it should report that the login code was sent, got %q", out)
	}
	if strings.Contains(out, "No Telegram link found") {
		t.Fatalf("the phone number was treated as a link: %q", out)
	}

	// The user's message must be deleted (codes and phone numbers must not stay in the chat
	// history).
	api.mu.Lock()
	deleted := len(api.deleted)
	api.mu.Unlock()
	if deleted != 1 {
		t.Fatalf("1 login input message should be deleted, got %d", deleted)
	}
}

// Test 12: /cancel leaves a login wizard in progress.
func TestLoginWizardCancelExit(t *testing.T) {
	b, api, _ := newAdminTestBot(t, 2)
	b.cfg.Bot.AdminUserIDs = []int64{42}
	fake := &fakeLoginAPI{}
	b.SetLoginAPI(func(context.Context) (LoginAPI, error) { return fake, nil })

	msg := textMessage(1, 100, "/login")
	msg.From = userWithID(42)
	if err := b.handleText(context.Background(), msg); err != nil {
		t.Fatalf("/login: %v", err)
	}
	cancelMsg := textMessage(2, 100, "/cancel")
	cancelMsg.From = userWithID(42)
	if err := b.handleText(context.Background(), cancelMsg); err != nil {
		t.Fatalf("/cancel: %v", err)
	}
	if out := api.sent[len(api.sent)-1].Text; !strings.Contains(out, "Login wizard closed") {
		t.Fatalf("/cancel should leave the wizard, got %q", out)
	}
}

// Test 12b: a non-admin in the same chat cannot hijack or cancel an admin's login wizard.
func TestLoginWizardIsolatedToStarterAdmin(t *testing.T) {
	b, _, _ := newAdminTestBot(t, 2)
	b.cfg.Bot.AdminUserIDs = []int64{42}
	fake := &fakeLoginAPI{}
	b.SetLoginAPI(func(context.Context) (LoginAPI, error) { return fake, nil })

	startMsg := textMessage(1, 100, "/login")
	startMsg.From = userWithID(42)
	if err := b.handleText(context.Background(), startMsg); err != nil {
		t.Fatalf("/login: %v", err)
	}

	// Non-admin sends a phone number in the same chat: must NOT feed the wizard.
	intruderMsg := textMessage(2, 100, "+8613800138000")
	intruderMsg.From = userWithID(99)
	if err := b.handleText(context.Background(), intruderMsg); err != nil {
		t.Fatalf("intruder phone: %v", err)
	}
	fake.mu.Lock()
	sent := fake.codeSent
	fake.mu.Unlock()
	if sent != 0 {
		t.Fatalf("non-admin must not advance login wizard, SendCode called %d times", sent)
	}

	// Non-admin sends /cancel in the same chat: must NOT close the admin's wizard.
	intruderCancel := textMessage(3, 100, "/cancel")
	intruderCancel.From = userWithID(99)
	if err := b.handleText(context.Background(), intruderCancel); err != nil {
		t.Fatalf("intruder cancel: %v", err)
	}
	if !b.login.ActiveFor(100, 42) {
		t.Fatalf("non-admin /cancel must not close admin's login wizard")
	}
}

// Test 13: /logout calls the injected implementation (which really deletes the session).
func TestLogoutInvokesHook(t *testing.T) {
	b, api, _ := newAdminTestBot(t, 2)
	b.cfg.Bot.AdminUserIDs = []int64{42}
	called := 0
	b.SetLogout(func(context.Context) error { called++; return nil })

	msg := textMessage(1, 100, "/logout")
	msg.From = userWithID(42)
	if err := b.handleText(context.Background(), msg); err != nil {
		t.Fatalf("/logout: %v", err)
	}
	if called != 1 {
		t.Fatalf("LogoutFunc should be called once, got %d", called)
	}
	if out := api.sent[len(api.sent)-1].Text; !strings.Contains(out, "Session file deleted") {
		t.Fatalf("/logout text mismatch: %q", out)
	}
}

// Test 10: /cancel without arguments gives a usage hint.
func TestCancelNoArgs(t *testing.T) {
	b, api, _ := newAdminTestBot(t, 2)
	msg := textMessage(1, 100, "/cancel")
	msg.From = userWithID(1)
	if err := b.handleText(context.Background(), msg); err != nil {
		t.Fatalf("handleText: %v", err)
	}
	if out := api.sent[len(api.sent)-1].Text; !strings.Contains(out, "Usage") {
		t.Fatalf("it should give a usage hint, got %q", out)
	}
}
