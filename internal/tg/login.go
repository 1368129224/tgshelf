// login.go - the login core for phone number + login code + 2FA.
//
// Design decisions (all measured against the live API):
//   - Do not hand-roll the 2FA SRP computation; use gotd's *auth.Client directly:
//     auth.Client.Password performs account.getPassword + auth.PasswordHash
//     internally (telegram/auth/user.go:44-79) and already normalizes the two
//     errors we need:
//     SESSION_PASSWORD_NEEDED -> auth.ErrPasswordAuthNeeded,
//     PASSWORD_HASH_INVALID   -> auth.ErrPasswordInvalid.
//     A hand-written SRP would have to handle PBKDF2/SHA256/MODPOW and refetch
//     SRP_ID itself, which is pure duplicated work.
//   - But do not use auth.Flow: Flow.Run runs phone -> code -> password in one
//     go and does not hand control back to the user between "send code" and
//     "verify code" (this project is a chat bot, so a user message sits between
//     the two steps), and Flow cannot inject DC migration handling.
//     So this file strings the flow together from three auth.Client methods.
package tg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// errClientNotInitialized reports that Client or its embedded telegram.Client is nil.
var errClientNotInitialized = errors.New("tg: client not initialized")

// errAlreadyAuthorized reports that the account is already logged in, so the
// login code flow is unnecessary. Telegram means exactly this when auth.sendCode
// returns auth.sentCodeSuccess.
var errAlreadyAuthorized = errors.New("tg: this session is already logged in, no need to send another login code")

// FloodWaitError reports that Telegram rate limited the operation.
// It differs from the automatic wait in the middleware: the login flow must tell
// the user the wait duration explicitly instead of silently blocking for tens of
// seconds, so it is surfaced as a readable error.
type FloodWaitError struct {
	Op   string        // name of the rate-limited operation, e.g. "send login code"
	Wait time.Duration // wait duration demanded by the server
}

// Error implements error and returns a readable message carrying the seconds.
func (e *FloodWaitError) Error() string {
	return fmt.Sprintf("tg: %s hit the Telegram rate limit, retry in %d seconds",
		e.Op, int(e.Wait/time.Second))
}

// loginAPI is the narrow interface the login flow depends on; *auth.Client
// satisfies it. The only reason for the interface is to let tests inject fakes
// (no real network allowed).
type loginAPI interface {
	SendCode(ctx context.Context, phone string, options auth.SendCodeOptions) (tg.AuthSentCodeClass, error)
	SignIn(ctx context.Context, phone, code, codeHash string) (*tg.AuthAuthorization, error)
	Password(ctx context.Context, password string) (*tg.AuthAuthorization, error)
}

// dcMigrator is the DC migration capability, satisfied by *telegram.Client.
type dcMigrator interface {
	MigrateTo(ctx context.Context, dcID int) error
}

// selfAPI is the narrow "query myself" interface, satisfied by *tg.Client.
type selfAPI interface {
	UsersGetUsers(ctx context.Context, id []tg.InputUserClass) ([]tg.UserClass, error)
}

// loginFlow bundles the three injectable capabilities the login flow needs.
type loginFlow struct {
	auth loginAPI   // *auth.Client: send code / verify code / 2FA
	mig  dcMigrator // *telegram.Client: DC migration after PHONE_MIGRATE
	self selfAPI    // *tg.Client: users.getUsers
	dc   int        // current DC, returned by Self
}

// loginFlow assembles the login flow from a constructed Client. It makes no
// network calls.
func (c *Client) loginFlow() (*loginFlow, error) {
	if c == nil || c.Client == nil {
		return nil, errClientNotInitialized
	}
	return &loginFlow{
		auth: c.Auth(),
		mig:  c.Client,
		self: c.API(),
		dc:   c.Client.Config().ThisDC,
	}, nil
}

// phoneSeparators lists the separators that may appear in a phone number and
// must be dropped.
const phoneSeparators = " \t-()."

// NormalizePhone normalizes a phone number: it drops spaces, tabs, hyphens and
// parentheses and keeps the leading "+". A missing leading "+" is added
// (Telegram's phone_number field requires the international format).
//
// Illegal characters, or a digit count that is too short or too long, produce a
// readable error instead of being silently accepted.
func NormalizePhone(phone string) (string, error) {
	var b strings.Builder
	for _, r := range phone {
		switch {
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '+' && b.Len() == 0:
			b.WriteRune(r)
		case strings.ContainsRune(phoneSeparators, r):
			// drop separators as-is
		default:
			return "", fmt.Errorf("tg: phone number contains illegal character %q "+
				"(only digits and the separators space - ( ) . are allowed)", r)
		}
	}

	s := b.String()
	digits := strings.TrimPrefix(s, "+")
	if len(digits) < 5 || len(digits) > 15 {
		return "", fmt.Errorf("tg: phone number has %d digits, want 5 to 15", len(digits))
	}
	if !strings.HasPrefix(s, "+") {
		s = "+" + s
	}
	return s, nil
}

// migrateTarget parses PHONE_MIGRATE_x from a Telegram error and returns the
// target DC.
func migrateTarget(err error) (int, bool) {
	rpcErr, ok := tgerr.As(err)
	if !ok || !rpcErr.IsType("PHONE_MIGRATE") {
		return 0, false
	}
	if rpcErr.Argument <= 0 {
		return 0, false
	}
	return rpcErr.Argument, true
}

// asFloodWait converts FLOOD_WAIT_x into a readable error carrying the seconds.
func asFloodWait(op string, err error) error {
	wait, ok := tgerr.AsFloodWait(err)
	if !ok {
		return nil
	}
	return &FloodWaitError{Op: op, Wait: wait}
}

// maxMigrateReplay is the cap on consecutive PHONE_MIGRATE replays (guards
// against a server that keeps sending the same migration).
const maxMigrateReplay = 3

// SendCode sends a login code to phone and returns the phoneCodeHash the
// following SignIn call needs.
//
// Flow: normalize the phone number -> auth.sendCode -> on PHONE_MIGRATE_x
// migrate the DC and replay.
// FLOOD_WAIT is surfaced as *FloodWaitError carrying the wait seconds demanded
// by the server.
//
// Important note about PHONE_MIGRATE (measured, do not misread):
// gotd v0.163's *telegram.Client already performs DC migration internally -
// the *tg.Client returned by telegram.Client.API() goes through c.invoker, and
// c.invoker is chainMiddlewares(invokeDirect) (telegram/client.go:284-285);
// invokeDirect calls invokeMigrate for any *_MIGRATE error that is not FILE_/
// STATS_ and replays it (telegram/invoke.go:61-77). So **on the real call path
// this function's outer migration branch is basically never taken**: PHONE_MIGRATE
// is usually already absorbed by gotd one layer down.
// The branch is kept as an explicit fallback that does not depend on the call
// chain (e.g. a bare invoker in the future, or gotd changing its migration
// policy), and login_test.go asserts its behavior with fakes.
// It is not the production main path; when it breaks, look at gotd's migration
// logic first.
func (c *Client) SendCode(ctx context.Context, phone string) (string, error) {
	f, err := c.loginFlow()
	if err != nil {
		return "", err
	}
	return f.sendCode(ctx, phone)
}

// sendCode is the testable implementation of SendCode.
func (f *loginFlow) sendCode(ctx context.Context, phone string) (string, error) {
	normalized, err := NormalizePhone(phone)
	if err != nil {
		return "", err
	}

	for attempt := 0; ; attempt++ {
		sent, err := f.auth.SendCode(ctx, normalized, auth.SendCodeOptions{})
		if err == nil {
			return phoneCodeHashOf(sent)
		}

		// AUTH_RESTART: the local auth key is dead, retrying is pointless, return
		// recovery guidance right away.
		if isAuthRestart(err) {
			return "", fmt.Errorf("%w (original error: %s)", ErrAuthRestart, err)
		}

		targetDC, ok := migrateTarget(err)
		if !ok {
			if fw := asFloodWait("send login code", err); fw != nil {
				return "", fw
			}
			return "", fmt.Errorf("tg: send login code: %w", err)
		}

		if attempt >= maxMigrateReplay {
			return "", fmt.Errorf("tg: send login code: asked to migrate DC %d times in a row "+
				"(last target DC %d), giving up", attempt, targetDC)
		}
		if migrateErr := f.mig.MigrateTo(ctx, targetDC); migrateErr != nil {
			return "", fmt.Errorf("tg: migrate to DC %d: %w", targetDC, migrateErr)
		}
		f.dc = targetDC
		// After a successful migration replay the same auth.sendCode.
	}
}

// Login errors are exposed separately so the upper layers can give different
// recovery advice (a wrong login code can be re-entered, an expired one requires
// a new code).
var (
	// ErrPhoneCodeInvalid reports that the login code itself is wrong.
	ErrPhoneCodeInvalid = errors.New("tg: wrong login code, please try again")
	// ErrPhoneCodeExpired reports that the login code expired and must be resent.
	ErrPhoneCodeExpired = errors.New("tg: login code expired, please request a new one")
	// ErrPasswordHashInvalid reports a wrong 2FA password.
	ErrPasswordHashInvalid = errors.New("tg: wrong 2FA password")
	// ErrNotAuthorized reports that the current session is not logged in.
	ErrNotAuthorized = errors.New("tg: current session is not logged in, run the login flow first")
	// ErrAuthRestart reports that the server demands a fresh authorization: the
	// auth key in the local session is unusable (typically because the previous
	// login failed midway, the auth key was never bound, or the server dropped
	// it). Recovery is to delete the session file and log in again - callers turn
	// that into explicit guidance.
	ErrAuthRestart = errors.New("tg: session authorization state is invalid (AUTH_RESTART), " +
		"delete the session file and log in again")
)

// isAuthRestart reports whether err is an AUTH_RESTART family error.
// Telegram returns 500 AUTH_RESTART when the auth key is unusable, meaning
// "the authorization context of this connection is gone, start over"; retrying
// the same RPC is pointless.
func isAuthRestart(err error) bool {
	if err == nil {
		return false
	}
	rpcErr, ok := tgerr.As(err)
	if !ok {
		return false
	}
	return rpcErr.IsType("AUTH_RESTART")
}

// SignIn completes login code verification with phone + phoneCodeHash + code.
//
// need2FA=true with err=nil means the account has two-step verification enabled:
// the caller should ask for the password and call CheckPassword. A wrong code and
// an expired code are independent errors.
func (c *Client) SignIn(ctx context.Context, phone, hash, code string) (bool, error) {
	f, err := c.loginFlow()
	if err != nil {
		return false, err
	}
	return f.signIn(ctx, phone, hash, code)
}

// signIn is the testable implementation of SignIn.
func (f *loginFlow) signIn(ctx context.Context, phone, hash, code string) (bool, error) {
	normalized, err := NormalizePhone(phone)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(hash) == "" {
		return false, errors.New("tg: phoneCodeHash is empty, call SendCode first")
	}
	if strings.TrimSpace(code) == "" {
		return false, errors.New("tg: login code is empty")
	}

	for attempt := 0; ; attempt++ {
		_, err := f.auth.SignIn(ctx, normalized, strings.TrimSpace(code), hash)
		if err == nil {
			return false, nil
		}

		// gotd already normalized SESSION_PASSWORD_NEEDED into auth.ErrPasswordAuthNeeded.
		if errors.Is(err, auth.ErrPasswordAuthNeeded) {
			return true, nil
		}

		switch {
		case tgerr.Is(err, "PHONE_CODE_INVALID"):
			return false, ErrPhoneCodeInvalid
		case tgerr.Is(err, "PHONE_CODE_EXPIRED"):
			return false, ErrPhoneCodeExpired
		}

		targetDC, ok := migrateTarget(err)
		if !ok {
			if fw := asFloodWait("verify login code", err); fw != nil {
				return false, fw
			}
			return false, fmt.Errorf("tg: log in: %w", err)
		}
		if attempt >= maxMigrateReplay {
			return false, fmt.Errorf("tg: log in: asked to migrate DC %d times in a row "+
				"(last target DC %d), giving up", attempt, targetDC)
		}
		if migrateErr := f.mig.MigrateTo(ctx, targetDC); migrateErr != nil {
			return false, fmt.Errorf("tg: migrate to DC %d: %w", targetDC, migrateErr)
		}
		f.dc = targetDC
	}
}

// CheckPassword completes login with the 2FA password after SignIn returned
// need2FA=true.
//
// The SRP computation is delegated to gotd's auth.Client.Password (internally
// account.getPassword + auth.PasswordHash + auth.checkPassword, see
// telegram/auth/user.go:44-79); this layer only normalizes errors:
// PASSWORD_HASH_INVALID -> ErrPasswordHashInvalid.
func (c *Client) CheckPassword(ctx context.Context, password string) error {
	f, err := c.loginFlow()
	if err != nil {
		return err
	}
	return f.checkPassword(ctx, password)
}

// checkPassword is the testable implementation of CheckPassword.
func (f *loginFlow) checkPassword(ctx context.Context, password string) error {
	if strings.TrimSpace(password) == "" {
		return errors.New("tg: 2FA password is empty")
	}

	_, err := f.auth.Password(ctx, password)
	if err == nil {
		return nil
	}
	// gotd already normalized PASSWORD_HASH_INVALID into auth.ErrPasswordInvalid.
	if errors.Is(err, auth.ErrPasswordInvalid) || tgerr.Is(err, "PASSWORD_HASH_INVALID") {
		return ErrPasswordHashInvalid
	}
	if fw := asFloodWait("verify 2FA password", err); fw != nil {
		return fw
	}
	return fmt.Errorf("tg: 2FA verification: %w", err)
}

// Self queries the currently logged-in account and returns the user ID, the
// username and the DC it sits on.
// The username may be empty (accounts without a @username); that is not an error.
// Not logged in (401-class errors) returns ErrNotAuthorized.
func (c *Client) Self(ctx context.Context) (int64, string, int, error) {
	f, err := c.loginFlow()
	if err != nil {
		return 0, "", 0, err
	}
	return f.self_(ctx)
}

// self_ is the testable implementation of Self (the trailing underscore keeps the
// name from colliding with the self field).
func (f *loginFlow) self_(ctx context.Context) (int64, string, int, error) {
	users, err := f.self.UsersGetUsers(ctx, []tg.InputUserClass{&tg.InputUserSelf{}})
	if err != nil {
		if tgerr.IsCode(err, 401) {
			return 0, "", 0, ErrNotAuthorized
		}
		return 0, "", 0, fmt.Errorf("tg: query logged-in account: %w", err)
	}

	// users.getUsers may return userEmpty or an empty list; both mean not logged in.
	for _, u := range users {
		if user, ok := u.(*tg.User); ok && !user.Deleted {
			return user.ID, user.Username, f.dc, nil
		}
	}
	return 0, "", 0, ErrNotAuthorized
}

// phoneCodeHashOf extracts phoneCodeHash from an auth.sentCode result.
func phoneCodeHashOf(sent tg.AuthSentCodeClass) (string, error) {
	switch v := sent.(type) {
	case *tg.AuthSentCode:
		if v.PhoneCodeHash == "" {
			return "", errors.New("tg: server returned an empty phoneCodeHash")
		}
		return v.PhoneCodeHash, nil
	case *tg.AuthSentCodeSuccess:
		return "", errAlreadyAuthorized
	case nil:
		return "", errors.New("tg: server returned a nil auth.sentCode")
	default:
		return "", fmt.Errorf("tg: unsupported auth.sentCode type %s", sent.TypeName())
	}
}
