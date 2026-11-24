// State machine skeleton for the /login wizard.
//
// It only manages state and orchestrates message in/out: chatID -> LoginState (TTL 10 minutes).
// The actual MTProto calls (send code / sign in / 2FA) are injected through the LoginAPI
// interface, so a fake implementation drives the whole state machine in unit tests with zero
// network access.
//
// Note: gotd types never enter this package - LoginAPI's parameters and results are all plain Go
// primitives.
package bot

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// loginStateTTL is the maximum lifetime of one login session; after it expires /login must be
// sent again.
const loginStateTTL = 10 * time.Minute

// LoginStep is the step the wizard is currently on.
type LoginStep string

const (
	// LoginStepPhone waits for the phone number.
	LoginStepPhone LoginStep = "phone"
	// LoginStepCode waits for the login code.
	LoginStepCode LoginStep = "code"
	// LoginStepPassword waits for the 2FA password.
	LoginStepPassword LoginStep = "password"
)

// ErrLoginNotStarted means this chat has no login wizard in progress.
var ErrLoginNotStarted = errors.New("bot: no login wizard in progress, send /login first")

// LoginAPI is the set of MTProto capabilities login needs (a minimal interface).
//
// Semantics mirror internal/tg.Client's SendCode / SignIn / CheckPassword:
//   - SendCode returns phoneCodeHash (the implementation keeps the login state itself);
//   - SignIn returns needPassword when the server demands 2FA;
//   - CheckPassword completes the 2FA check.
//
// The implementation is responsible for wrapping the tg.Client.Run(ctx, ...)
// session lifecycle; a fake can implement this interface directly for state machine unit tests.
type LoginAPI interface {
	SendCode(ctx context.Context, phone string) (phoneCodeHash string, err error)
	SignIn(ctx context.Context, phone, phoneCodeHash, code string) (needPassword bool, err error)
	CheckPassword(ctx context.Context, password string) error
}

// LoginAPIGetter supplies the LoginAPI lazily: the bot assembly layer may not have an authorized
// MTProto client yet (logging in is how you get one), so inject a getter instead of an instance.
type LoginAPIGetter func(ctx context.Context) (LoginAPI, error)

// LoginState is the login wizard state of a single chat.
type LoginState struct {
	UserID        int64
	Step          LoginStep
	Phone         string
	PhoneCodeHash string
	// ExpiresAt is when the state expires (reset to now+loginStateTTL as each step advances).
	ExpiresAt time.Time
	gen       uint64
}

// loginWizard is the /login wizard: one state per chat plus TTL cleanup.
// The Bot owns the message in/out orchestration (the wizard only returns reply text and errors),
// so the struct holds no BotAPI - the dependency direction stays wizard -> abstract interfaces.
type loginWizard struct {
	mu      sync.Mutex
	states  map[int64]LoginState
	nextGen uint64
	getAPI  LoginAPIGetter
	logger  *zap.Logger
}

// newLoginWizard creates the wizard. With a nil getAPI, Handle reports "login RPC not wired".
func newLoginWizard(getAPI LoginAPIGetter, logger *zap.Logger) *loginWizard {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &loginWizard{
		states: make(map[int64]LoginState),
		getAPI: getAPI,
		logger: logger,
	}
}

// state returns the current state of a chat; an expired or missing state yields
// ErrLoginNotStarted.
func (w *loginWizard) state(chatID int64) (LoginState, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	st, ok := w.states[chatID]
	if !ok || time.Now().After(st.ExpiresAt) {
		delete(w.states, chatID)
		return LoginState{}, ErrLoginNotStarted
	}
	return st, nil
}

// setState writes the state and refreshes its TTL.
func (w *loginWizard) setState(chatID int64, st LoginState) {
	if st.ExpiresAt.IsZero() {
		st.ExpiresAt = time.Now().Add(loginStateTTL)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.states[chatID] = st
}

// advanceState updates the chat's wizard state only if it still exists with expectedGen.
// This prevents an in-flight RPC from resurrecting a wizard that was cancelled or restarted.
func (w *loginWizard) advanceState(chatID int64, expectedGen uint64, st LoginState) bool {
	if st.ExpiresAt.IsZero() {
		st.ExpiresAt = time.Now().Add(loginStateTTL)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	cur, ok := w.states[chatID]
	if !ok || cur.gen != expectedGen {
		return false
	}
	st.gen = expectedGen
	w.states[chatID] = st
	return true
}

// clear deletes the chat's state (successful login / explicit cancel / expiry cleanup).
func (w *loginWizard) clear(chatID int64) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.states, chatID)
}

// Active reports whether the chat has an unexpired login wizard.
//
// Text routing relies on it to decide "is this message login input or an ordinary message", so it
// must be a side-effect-free read-only check (unlike state it must not drop expired entries along
// the way; dropping them is correct but has to take the lock).
func (w *loginWizard) Active(chatID int64) bool {
	_, err := w.state(chatID)
	return err == nil
}

// ActiveFor reports whether the chat has an unexpired login wizard owned by userID.
func (w *loginWizard) ActiveFor(chatID, userID int64) bool {
	st, err := w.state(chatID)
	return err == nil && st.UserID == userID
}

// setGetAPI updates the lazy LoginAPI getter safely (e.g. across MTProto reconnects) without
// discarding any active login wizard state.
func (w *loginWizard) setGetAPI(getAPI LoginAPIGetter) {
	w.mu.Lock()
	w.getAPI = getAPI
	w.mu.Unlock()
}

func (w *loginWizard) currentGetAPI() LoginAPIGetter {
	w.mu.Lock()
	fn := w.getAPI
	w.mu.Unlock()
	return fn
}

// Start registers a fresh PHONE-step state owned by userID and returns the first prompt for the
// user. Sending the message is the caller's (Bot's) job; the wizard only produces text.
func (w *loginWizard) Start(chatID, userID int64) (string, error) {
	if w.currentGetAPI() == nil {
		return "", errors.New("bot: login RPC not wired")
	}
	w.mu.Lock()
	w.nextGen++
	w.states[chatID] = LoginState{
		UserID:    userID,
		Step:      LoginStepPhone,
		ExpiresAt: time.Now().Add(loginStateTTL),
		gen:       w.nextGen,
	}
	w.mu.Unlock()
	return "Send your phone number in international format, e.g. +8613800138000:", nil
}

// Handle routes the input: it passes the user message to the handler for the current step.
// The return value is the reply text for the user; the error is non-nil only for failures worth
// showing to the user (through Translate).
func (w *loginWizard) Handle(ctx context.Context, chatID int64, text string) (string, error) {
	st, err := w.state(chatID)
	if err != nil {
		return "", err
	}
	getAPI := w.currentGetAPI()
	if getAPI == nil {
		return "", errors.New("bot: login RPC not wired")
	}
	api, err := getAPI(ctx)
	if err != nil {
		return "", err
	}
	switch st.Step {
	case LoginStepPhone:
		return w.handlePhone(ctx, api, chatID, st, text)
	case LoginStepCode:
		return w.handleCode(ctx, api, chatID, st, text)
	case LoginStepPassword:
		return w.handlePassword(ctx, api, chatID, st, text)
	}
	return "", fmt.Errorf("bot: unknown login step %q", st.Step)
}

// handlePhone handles the phone number input: after a rough check it sends the login code and
// moves to the CODE step.
func (w *loginWizard) handlePhone(ctx context.Context, api LoginAPI, chatID int64, st LoginState, text string) (string, error) {
	phone := strings.TrimSpace(text)
	// Rough check: require a leading + followed by digits; the strict check is tg.NormalizePhone's
	// job.
	if len(phone) < 2 || phone[0] != '+' {
		return "", errors.New("bot: invalid phone number, it must start with +, e.g. +8613800138000")
	}
	for _, r := range phone[1:] {
		if r < '0' || r > '9' {
			return "", errors.New("bot: invalid phone number, only digits are allowed after the +")
		}
	}
	hash, err := api.SendCode(ctx, phone)
	if err != nil {
		return "", err // sent back to the user after tg.Translate
	}
	st.Step = LoginStepCode
	st.Phone = phone
	st.PhoneCodeHash = hash
	st.ExpiresAt = time.Now().Add(loginStateTTL)
	if !w.advanceState(chatID, st.gen, st) {
		return "", ErrLoginNotStarted
	}
	return "Login code sent. Enter the code you received:", nil
}

// handleCode handles the login code input: clear the state when SignIn succeeds, move to the
// PASSWORD step when the account needs a password.
func (w *loginWizard) handleCode(ctx context.Context, api LoginAPI, chatID int64, st LoginState, text string) (string, error) {
	code := strings.TrimSpace(text)
	if code == "" {
		return "", errors.New("bot: login code must not be empty")
	}
	needPassword, err := api.SignIn(ctx, st.Phone, st.PhoneCodeHash, code)
	if err != nil {
		return "", err
	}
	if needPassword {
		st.Step = LoginStepPassword
		st.ExpiresAt = time.Now().Add(loginStateTTL)
		if !w.advanceState(chatID, st.gen, st) {
			return "", ErrLoginNotStarted
		}
		return "This account has two-step verification enabled. Enter your 2FA password:", nil
	}
	w.clear(chatID)
	return "Logged in.", nil
}

// handlePassword handles the 2FA password input: on success it clears the state and finishes the
// login.
func (w *loginWizard) handlePassword(ctx context.Context, api LoginAPI, chatID int64, st LoginState, text string) (string, error) {
	if text == "" {
		return "", errors.New("bot: password must not be empty")
	}
	if err := api.CheckPassword(ctx, text); err != nil {
		return "", err
	}
	w.clear(chatID)
	return "Logged in.", nil
}
