// Bot struct, dependency injection and polling wiring.
//
// Bot only assembles and schedules: its fields are the injected interfaces, and it holds neither the
// MTProto client nor a TaskRunner - download execution is injected as a TaskRunner through
// Manager.Submit's work callback, which keeps the bot package from hard-binding to gotd.
package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	tgbot "github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
	"go.uber.org/zap"

	"github.com/1368129224/tgshelf/internal/config"
	"github.com/1368129224/tgshelf/internal/state"
	"github.com/1368129224/tgshelf/internal/tg"
)

// BotAPI is a narrow interface over *tgbot.Bot; unit tests use a fake implementation, zero
// network.
type BotAPI interface {
	SendMessage(ctx context.Context, p *tgbot.SendMessageParams) (*models.Message, error)
	EditMessageText(ctx context.Context, p *tgbot.EditMessageTextParams) (*models.Message, error)
	AnswerCallbackQuery(ctx context.Context, p *tgbot.AnswerCallbackQueryParams) (bool, error)
	DeleteMessage(ctx context.Context, p *tgbot.DeleteMessageParams) (bool, error)
	GetMe(ctx context.Context) (*models.User, error)
}

// textHandlerFunc / callbackHandlerFunc are Bot's internal dispatch signatures (used when
// registering handlers).
type textHandlerFunc func(ctx context.Context, msg *models.Message) error

type callbackHandlerFunc func(ctx context.Context, cbq *models.CallbackQuery) error

// DownloadFunc is the download executor signature (injected by the assembly layer; the bot
// package does not hard-bind gotd).
type DownloadFunc func(ctx context.Context, t state.Task, report func(tg.TaskProgress)) error

// Bot is the assembly and scheduling layer of the Telegram bot.
type Bot struct {
	api    BotAPI
	cfg    *config.Config
	logger *zap.Logger
	store  *state.Store
	links  *LinkStore // shortid.go
	tasks  *Manager   // manager.go
	now    func() time.Time

	download DownloadFunc
	// logout is the /logout implementation; when nil, /logout only reports "not wired".
	logout LogoutFunc

	// textHandlers / callbackHandlers are the dispatch entry points registered on the real poller
	// in Run, or driven directly by a test fake.
	textHandlers     []textHandlerFunc
	callbackHandlers []callbackHandlerFunc

	login *loginWizard
}

// SetDownload injects the download executor (called by the assembly layer; without it download
// tasks end with an explicit error).
func (b *Bot) SetDownload(fn DownloadFunc) {
	b.download = fn
}

// LogoutFunc is the /logout implementation (the assembly layer deletes the session file).
type LogoutFunc func(ctx context.Context) error

// SetLoginAPI injects the login RPC getter (logging in is itself how authorization is obtained,
// so a getter is injected instead of an instance).
func (b *Bot) SetLoginAPI(getAPI LoginAPIGetter) {
	b.login = newLoginWizard(getAPI, b.logger)
}

// SetLogout injects the /logout implementation.
func (b *Bot) SetLogout(fn LogoutFunc) {
	b.logout = fn
}

// New builds a Bot from an injected BotAPI (unit test setup: api is a fake, zero network).
func New(api BotAPI, cfg *config.Config, logger *zap.Logger, st *state.Store) (*Bot, error) {
	if api == nil {
		return nil, errors.New("bot: api must not be nil")
	}
	if cfg == nil {
		return nil, errors.New("bot: cfg must not be nil")
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	if st == nil {
		return nil, errors.New("bot: store must not be nil")
	}
	if len(strings.TrimSpace(cfg.BotToken)) == 0 {
		logger.Warn("bot_token is empty: the bot cannot send any request to Telegram")
	}
	if len(cfg.Bot.AllowedUserIDs) == 0 {
		// An empty allow-list lets everyone in (isAllowed's semantics), but the user must be
		// warned at startup.
		logger.Warn("allowed_user_ids is empty: anyone can use this bot, consider setting [bot] allowed_user_ids")
	}
	maxConc := cfg.Bot.MaxConcurrency
	if maxConc <= 0 {
		maxConc = 1
	}
	b := &Bot{
		api:    api,
		cfg:    cfg,
		logger: logger,
		store:  st,
		links:  NewLinkStore(LinkStoreMaxSize),
		tasks:  NewManager(st, maxConc, logger),
		now:    time.Now,
		login:  newLoginWizard(nil, logger),
	}
	b.registerHandlers()
	return b, nil
}

// NewPolling builds a Bot from a real *tgbot.Bot: Run registers the handlers on it and then
// starts it.
//
// *tgbot.Bot satisfies BotAPI structurally (identical method set, no adapter needed).
func NewPolling(raw *tgbot.Bot, cfg *config.Config, logger *zap.Logger, st *state.Store) (*Bot, error) {
	if raw == nil {
		return nil, errors.New("bot: raw bot must not be nil")
	}
	return New(raw, cfg, logger, st)
}

// Run enters polling. Real *tgbot.Bot: handlers are already registered, so Start(ctx) blocks until
// ctx is cancelled; fake case: blocks until ctx.Done() then returns nil, which lets integration
// tests exercise "event -> reply".
// Startup is the once-per-process-start preparation: wind down ghost tasks + send the startup
// notification.
//
// It is split from Run because Run is now called repeatedly by the reconnect loop: a reconnect
// must not resend the startup notification, and even less should it treat "tasks left over from the
// previous process" as something to wind down on every reconnect.
func (b *Bot) Startup(ctx context.Context) {
	if n := b.tasks.ReconcileOrphans(); n > 0 {
		b.logger.Warn("marked tasks left over from the previous run as finished", zap.Int("count", n))
	}
	b.notifyStartup(ctx)
}

// Run enters polling. Real *tgbot.Bot: handlers are already registered, so Start(ctx) blocks until
// ctx is cancelled; fake case: blocks until ctx.Done() then returns nil, which lets integration
// tests exercise "event -> reply".
func (b *Bot) Run(ctx context.Context) error {
	poller, ok := b.api.(*tgbot.Bot)
	if ok {
		b.logger.Info("start polling Telegram updates")
		poller.Start(ctx)
		b.logger.Info("polling stopped")
		return nil
	}
	<-ctx.Done()
	return nil
}

// NotifyReconnect logs the dropped MTProto connection; it never messages users about it.
func (b *Bot) NotifyReconnect(ctx context.Context, attempt int, err error) {
	b.logger.Warn("MTProto connection reconnect", zap.Int("attempt", attempt), zap.Error(err))
}

// notifyStartup sends one startup message to each allow-listed user (admins when none are
// configured).
//
// A failed send only logs a WARN: the most common cause is a user who never chatted with the bot
// (Telegram does not let a bot open a conversation), so this must not fail startup.
func (b *Bot) notifyStartup(ctx context.Context) {
	targets := b.cfg.Bot.AllowedUserIDs
	if len(targets) == 0 {
		targets = b.cfg.Bot.AdminUserIDs
	}
	if len(targets) == 0 {
		b.logger.Warn("no allowed_user_ids / admin_user_ids configured, cannot send the startup notification")
		return
	}
	text := b.startupText()
	for _, id := range targets {
		if _, err := b.sendText(ctx, id, text); err != nil {
			b.logger.Warn("failed to send the startup notification (the user may never have chatted with the bot)",
				zap.Int64("user_id", id), zap.Error(err))
		}
	}
}

// startupText builds the startup notification body (it contains no secret).
func (b *Bot) startupText() string {
	var sb strings.Builder
	sb.WriteString("✅ tgshelf is up and running.\n\n")
	fmt.Fprintf(&sb, "Download folder: %s\n", b.cfg.DownloadPath)
	fmt.Fprintf(&sb, "Tags: %s\n", strings.Join(b.cfg.Tags, ", "))
	fmt.Fprintf(&sb, "Concurrent tasks: %d\n", b.cfg.Bot.MaxConcurrency)
	sb.WriteString("Account: " + b.accountStatus(context.Background()) + "\n\n")
	sb.WriteString("Send a t.me link to download; /help for commands.")
	return sb.String()
}

// registerHandlers registers the internal handlers on a real *tgbot.Bot (dispatch by prefix); the
// fake case is served by registerPollingFake.
func (b *Bot) registerHandlers() {
	// Text messages: commands and links alike are dispatched to handleText by prefix.
	textDispatch := textHandlerFunc(b.dispatchText)
	callbackDispatch := callbackHandlerFunc(b.dispatchCallback)
	if poller, ok := b.api.(*tgbot.Bot); ok {
		poller.RegisterHandler(tgbot.HandlerTypeMessageText, "", tgbot.MatchTypeContains,
			func(ctx context.Context, _ *tgbot.Bot, upd *models.Update) {
				if upd.Message == nil {
					return
				}
				_ = textDispatch(ctx, upd.Message)
			})
		poller.RegisterHandler(tgbot.HandlerTypeCallbackQueryData, "", tgbot.MatchTypeContains,
			func(ctx context.Context, _ *tgbot.Bot, upd *models.Update) {
				if upd.CallbackQuery == nil {
					return
				}
				_ = callbackDispatch(ctx, upd.CallbackQuery)
			})
	}
	b.textHandlers = []textHandlerFunc{textDispatch}
	b.callbackHandlers = []callbackHandlerFunc{callbackDispatch}
}

// dispatchText is the single entry point for text messages: authorize, then dispatch by prefix.
func (b *Bot) dispatchText(ctx context.Context, msg *models.Message) (err error) {
	defer func() {
		if r := recover(); r != nil {
			b.logger.Error("panic while handling a message, recovered automatically",
				zap.Any("recover", r),
				zap.Stack("stack"),
			)
			if msg != nil {
				_, _ = b.sendText(ctx, msg.Chat.ID, "❌ Internal error, recovered automatically. Please try again.")
			}
			err = fmt.Errorf("bot: panic recovered: %v", r)
		}
	}()
	if msg == nil {
		return nil
	}
	if msg.From == nil || !isAllowed(b.cfg, msg.From.ID) {
		return b.denyMessage(ctx, msg, msg.From)
	}
	return b.handleText(ctx, msg)
}

// dispatchCallback is the single entry point for callbacks: authorize, parse callback_data, then
// dispatch.
func (b *Bot) dispatchCallback(ctx context.Context, cbq *models.CallbackQuery) (err error) {
	defer func() {
		if r := recover(); r != nil {
			b.logger.Error("panic while handling a callback, recovered automatically",
				zap.Any("recover", r),
				zap.Stack("stack"),
			)
			if cbq != nil {
				_ = b.answerCallback(ctx, cbq, "Internal error, please try again", true)
			}
			err = fmt.Errorf("bot: callback panic recovered: %v", r)
		}
	}()
	if cbq == nil {
		return nil
	}
	if !isAllowed(b.cfg, cbq.From.ID) {
		return b.denyCallback(ctx, cbq)
	}
	cb, err := ParseCallback(cbq.Data)
	if err != nil {
		b.logger.Warn("failed to parse callback_data", zap.String("data", cbq.Data), zap.Error(err))
		return b.answerCallback(ctx, cbq, "This button has expired, send the link again", true)
	}
	switch cb.Action {
	case ActionSelect:
		return b.onSelectTag(ctx, cbq, cb)
	case ActionCancelSelect:
		return b.onCancelSelect(ctx, cbq, cb)
	case ActionPage:
		return b.onPage(ctx, cbq, cb)
	case ActionCancelTask:
		return b.onCancelTask(ctx, cbq, cb)
	case ActionRetry:
		return b.onRetry(ctx, cbq, cb)
	case ActionNoop:
		return b.answerCallback(ctx, cbq, "", false)
	}
	return fmt.Errorf("bot: unhandled callback action %q", cb.Action)
}

// denyMessage is the shared rejection reply.
func (b *Bot) denyMessage(ctx context.Context, msg *models.Message, from *models.User) error {
	who := ""
	if from != nil {
		who = fmt.Sprintf(" (user id: %d)", from.ID)
	}
	text := fmt.Sprintf("You are not allowed to use this bot%s.\nIf this is your bot, add that user id to `[bot] allowed_user_ids` in the config file and restart.", who)
	_, err := b.api.SendMessage(ctx, &tgbot.SendMessageParams{
		ChatID: msg.Chat.ID,
		Text:   text,
	})
	return err
}

// denyCallback is the shared callback rejection (AnswerCallbackQuery show_alert).
func (b *Bot) denyCallback(ctx context.Context, cbq *models.CallbackQuery) error {
	return b.answerCallback(ctx, cbq, "Not allowed: add your user id to [bot] allowed_user_ids in the config file and restart", true)
}

// answerCallback is a thin wrapper over AnswerCallbackQuery.
func (b *Bot) answerCallback(ctx context.Context, cbq *models.CallbackQuery, text string, alert bool) error {
	_, err := b.api.AnswerCallbackQuery(ctx, &tgbot.AnswerCallbackQueryParams{
		CallbackQueryID: cbq.ID,
		Text:            text,
		ShowAlert:       alert,
	})
	return err
}

// sendText is the minimal outgoing-text wrapper (reused across the package).
func (b *Bot) sendText(ctx context.Context, chatID int64, text string) (*models.Message, error) {
	return b.api.SendMessage(ctx, &tgbot.SendMessageParams{ChatID: chatID, Text: text})
}

// translate turns any error into one user-facing sentence (never surface a raw error
// to the user).
func (b *Bot) translate(err error) string {
	return tg.Translate(err)
}
