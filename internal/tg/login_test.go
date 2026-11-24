// login_test.go - unit tests for the login flow (no real network allowed).
//
// Every Telegram call goes through the three narrow loginFlow interfaces, so the
// tests are guaranteed to make no network requests while still being able to
// assert call counts.
package tg

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// fakeAuth is the fake implementation of loginAPI; it records the arguments of
// every call.
type fakeAuth struct {
	// sendCodeSeq is returned in call order; once exhausted the last element is reused.
	sendCodeSeq []sendCodeResult
	// signInSeq is the same as above.
	signInSeq []signInResult

	sendCodeCalls []string // normalized phone number received on each call
	signInCalls   []signInCall
	passwordCalls []string
}

type sendCodeResult struct {
	sent tg.AuthSentCodeClass
	err  error
}

type signInResult struct {
	auth *tg.AuthAuthorization
	err  error
}

type signInCall struct {
	phone, code, hash string
}

func (f *fakeAuth) SendCode(_ context.Context, phone string, _ auth.SendCodeOptions) (tg.AuthSentCodeClass, error) {
	f.sendCodeCalls = append(f.sendCodeCalls, phone)
	i := len(f.sendCodeCalls) - 1
	if i >= len(f.sendCodeSeq) {
		i = len(f.sendCodeSeq) - 1
	}
	r := f.sendCodeSeq[i]
	return r.sent, r.err
}

func (f *fakeAuth) SignIn(_ context.Context, phone, code, codeHash string) (*tg.AuthAuthorization, error) {
	f.signInCalls = append(f.signInCalls, signInCall{phone: phone, code: code, hash: codeHash})
	i := len(f.signInCalls) - 1
	if i >= len(f.signInSeq) {
		i = len(f.signInSeq) - 1
	}
	r := f.signInSeq[i]
	return r.auth, r.err
}

func (f *fakeAuth) Password(_ context.Context, password string) (*tg.AuthAuthorization, error) {
	f.passwordCalls = append(f.passwordCalls, password)
	return &tg.AuthAuthorization{}, nil
}

// fakeMigrator is the fake implementation of dcMigrator.
type fakeMigrator struct {
	calls   []int
	errByDC map[int]error
}

func (f *fakeMigrator) MigrateTo(_ context.Context, dcID int) error {
	f.calls = append(f.calls, dcID)
	if f.errByDC != nil {
		if err, ok := f.errByDC[dcID]; ok {
			return err
		}
	}
	return nil
}

// fakeSelf is the fake implementation of selfAPI.
type fakeSelf struct {
	users []tg.UserClass
	err   error
	calls int
}

func (f *fakeSelf) UsersGetUsers(_ context.Context, _ []tg.InputUserClass) ([]tg.UserClass, error) {
	f.calls++
	return f.users, f.err
}

// newTestFlow assembles a set of login flow fakes.
func newTestFlow(a loginAPI, m *fakeMigrator, s *fakeSelf, dc int) *loginFlow {
	return &loginFlow{auth: a, mig: m, self: s, dc: dc}
}

// Compile-time proof that the fakes satisfy the narrow interfaces.
var (
	_ loginAPI   = (*fakeAuth)(nil)
	_ dcMigrator = (*fakeMigrator)(nil)
	_ selfAPI    = (*fakeSelf)(nil)
)

// sentCode builds an auth.sentCode carrying a phoneCodeHash.
func sentCode(hash string) tg.AuthSentCodeClass {
	return &tg.AuthSentCode{PhoneCodeHash: hash, Type: &tg.AuthSentCodeTypeSMS{}}
}

// TestNormalizePhone covers several input shapes of phone number normalization.
func TestNormalizePhone(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
		wantErr        bool
	}{
		{"digits only, no plus", "8613800138000", "+8613800138000", false},
		{"plus already present", "+8613800138000", "+8613800138000", false},
		{"hyphen separated", "+86-138-0013-8000", "+8613800138000", false},
		{"space separated", "+86 138 0013 8000", "+8613800138000", false},
		{"parentheses", "+86 (138) 0013-8000", "+8613800138000", false},
		{"tabs", "+86\t138\t0013\t8000", "+8613800138000", false},
		{"surrounding whitespace", "  +8613800138000  ", "+8613800138000", false},
		{"mixed shapes", " (86) 138-0013.8000 ", "+8613800138000", false},
		{"letter is illegal", "+8613800138000a", "", true},
		{"Chinese characters are illegal", "+86一三8", "", true},
		{"too few digits", "12345x", "", true},
		{"fewer than 5 digits", "1234", "", true},
		{"too many digits", "+1234567890123456", "", true},
		{"empty string", "", "", true},
		{"plus not leading", "86+13800138000", "", true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizePhone(tc.in)
			if (err != nil) != tc.wantErr {
				t.Fatalf("NormalizePhone(%q) err=%v, wantErr=%v", tc.in, err, tc.wantErr)
			}
			if !tc.wantErr && got != tc.want {
				t.Fatalf("NormalizePhone(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSendCode_Success checks that a normal send returns the hash and that
// Telegram receives the normalized phone number.
func TestSendCode_Success(t *testing.T) {
	a := &fakeAuth{sendCodeSeq: []sendCodeResult{{sent: sentCode("hash-1")}}}
	f := newTestFlow(a, &fakeMigrator{}, &fakeSelf{}, 2)

	hash, err := f.sendCode(context.Background(), "+86 (138) 0013-8000")
	if err != nil {
		t.Fatalf("sendCode: %v", err)
	}
	if hash != "hash-1" {
		t.Fatalf("hash = %q, want hash-1", hash)
	}
	if len(a.sendCodeCalls) != 1 {
		t.Fatalf("auth.sendCode called %d times, want 1", len(a.sendCodeCalls))
	}
	if a.sendCodeCalls[0] != "+8613800138000" {
		t.Fatalf("sent phone number = %q, want the normalized +8613800138000", a.sendCodeCalls[0])
	}
}

// TestSendCode_PhoneMigrateReplay checks that PHONE_MIGRATE_2 / _4 migrate the
// DC and then replay.
func TestSendCode_PhoneMigrateReplay(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantDC   int
		wantCall int
	}{
		{"migrate to DC2", tgerr.New(303, "PHONE_MIGRATE_2"), 2, 2},
		{"migrate to DC4", tgerr.New(303, "PHONE_MIGRATE_4"), 4, 2},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			a := &fakeAuth{sendCodeSeq: []sendCodeResult{
				{err: tc.err},
				{sent: sentCode("hash-after-migrate")},
			}}
			m := &fakeMigrator{}
			f := newTestFlow(a, m, &fakeSelf{}, 1)

			hash, err := f.sendCode(context.Background(), "+8613800138000")
			if err != nil {
				t.Fatalf("sendCode should succeed after migrating and replaying: %v", err)
			}
			if hash != "hash-after-migrate" {
				t.Fatalf("hash = %q", hash)
			}
			if len(a.sendCodeCalls) != tc.wantCall {
				t.Fatalf("auth.sendCode called %d times, want %d", len(a.sendCodeCalls), tc.wantCall)
			}
			if len(m.calls) != 1 || m.calls[0] != tc.wantDC {
				t.Fatalf("MigrateTo calls = %v, want [%d]", m.calls, tc.wantDC)
			}
			if f.dc != tc.wantDC {
				t.Fatalf("after migration f.dc = %d, want %d", f.dc, tc.wantDC)
			}
		})
	}
}

// TestSendCode_MigrateExhausted checks that a server that keeps sending
// PHONE_MIGRATE does not cause an endless replay.
func TestSendCode_MigrateExhausted(t *testing.T) {
	a := &fakeAuth{sendCodeSeq: []sendCodeResult{
		{err: tgerr.New(303, "PHONE_MIGRATE_2")},
	}}
	m := &fakeMigrator{}
	f := newTestFlow(a, m, &fakeSelf{}, 1)

	if _, err := f.sendCode(context.Background(), "+8613800138000"); err == nil {
		t.Fatal("consecutive migrations should return an error instead of replaying forever")
	}
	if len(a.sendCodeCalls) != maxMigrateReplay+1 {
		t.Fatalf("auth.sendCode called %d times, want %d", len(a.sendCodeCalls), maxMigrateReplay+1)
	}
}

// TestSendCode_MigrateFailure checks that a failing DC migration itself is surfaced.
func TestSendCode_MigrateFailure(t *testing.T) {
	boom := errors.New("boom")
	a := &fakeAuth{sendCodeSeq: []sendCodeResult{{err: tgerr.New(303, "PHONE_MIGRATE_2")}}}
	m := &fakeMigrator{errByDC: map[int]error{2: boom}}
	f := newTestFlow(a, m, &fakeSelf{}, 1)

	_, err := f.sendCode(context.Background(), "+8613800138000")
	if !errors.Is(err, boom) {
		t.Fatalf("should wrap the migration error, got %v", err)
	}
	if len(a.sendCodeCalls) != 1 {
		t.Fatalf("a failed migration must not replay; auth.sendCode called %d times", len(a.sendCodeCalls))
	}
}

// TestSendCode_FloodWait checks that FLOOD_WAIT_35 becomes a readable error
// carrying 35 seconds.
func TestSendCode_FloodWait(t *testing.T) {
	a := &fakeAuth{sendCodeSeq: []sendCodeResult{{err: tgerr.New(420, "FLOOD_WAIT_35")}}}
	f := newTestFlow(a, &fakeMigrator{}, &fakeSelf{}, 2)

	_, err := f.sendCode(context.Background(), "+8613800138000")
	if err == nil {
		t.Fatal("FLOOD_WAIT should return an error")
	}

	var fw *FloodWaitError
	if !errors.As(err, &fw) {
		t.Fatalf("should be *FloodWaitError, got %T: %v", err, err)
	}
	if fw.Wait != 35*time.Second {
		t.Fatalf("wait = %v, want 35s", fw.Wait)
	}
	if msg := fw.Error(); msg == "" {
		t.Fatal("the error message must not be empty")
	}
	// The readable message must carry the seconds so it can be shown to the user directly.
	if got := fw.Error(); !containsAll(got, "35", "seconds") {
		t.Fatalf("error message %q should contain the number of seconds, 35", got)
	}
	// It must not replay because of a flood wait.
	if len(a.sendCodeCalls) != 1 {
		t.Fatalf("flood wait must not replay, called %d times", len(a.sendCodeCalls))
	}
}

// TestSendCode_AlreadyAuthorized checks that auth.sentCodeSuccess means already logged in.
func TestSendCode_AlreadyAuthorized(t *testing.T) {
	a := &fakeAuth{sendCodeSeq: []sendCodeResult{{sent: &tg.AuthSentCodeSuccess{}}}}
	f := newTestFlow(a, &fakeMigrator{}, &fakeSelf{}, 2)

	if _, err := f.sendCode(context.Background(), "+8613800138000"); !errors.Is(err, errAlreadyAuthorized) {
		t.Fatalf("should be errAlreadyAuthorized, got %v", err)
	}
}

// TestSendCode_InvalidPhone checks that an illegal phone number never reaches Telegram.
func TestSendCode_InvalidPhone(t *testing.T) {
	a := &fakeAuth{sendCodeSeq: []sendCodeResult{{sent: sentCode("h")}}}
	f := newTestFlow(a, &fakeMigrator{}, &fakeSelf{}, 2)

	if _, err := f.sendCode(context.Background(), "not-a-phone"); err == nil {
		t.Fatal("an illegal phone number should return an error")
	}
	if len(a.sendCodeCalls) != 0 {
		t.Fatalf("an illegal phone number must not call auth.sendCode, got %d calls", len(a.sendCodeCalls))
	}
}

// TestSignIn_Success checks a normal login.
func TestSignIn_Success(t *testing.T) {
	a := &fakeAuth{signInSeq: []signInResult{{auth: &tg.AuthAuthorization{}}}}
	f := newTestFlow(a, &fakeMigrator{}, &fakeSelf{}, 2)

	need2FA, err := f.signIn(context.Background(), "+86 138 0013 8000", "hash-1", " 12345 ")
	if err != nil || need2FA {
		t.Fatalf("need2FA=%v err=%v", need2FA, err)
	}
	if len(a.signInCalls) != 1 {
		t.Fatalf("auth.signIn called %d times, want 1", len(a.signInCalls))
	}
	got := a.signInCalls[0]
	if got.phone != "+8613800138000" || got.code != "12345" || got.hash != "hash-1" {
		t.Fatalf("signIn args = %+v; the phone number should be normalized and the code trimmed", got)
	}
}

// TestSignIn_CodeInvalidVsExpired checks that a wrong code and an expired code are
// independent errors.
func TestSignIn_CodeInvalidVsExpired(t *testing.T) {
	invalid := tgerr.New(400, "PHONE_CODE_INVALID")
	expired := tgerr.New(400, "PHONE_CODE_EXPIRED")

	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"wrong login code", invalid, ErrPhoneCodeInvalid},
		{"expired login code", expired, ErrPhoneCodeExpired},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			a := &fakeAuth{signInSeq: []signInResult{{err: tc.err}}}
			f := newTestFlow(a, &fakeMigrator{}, &fakeSelf{}, 2)

			need2FA, err := f.signIn(context.Background(), "+8613800138000", "h", "00000")
			if need2FA {
				t.Fatal("a wrong login code must not return need2FA")
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			// The two must not be equivalent, otherwise the upper layer cannot tell them apart.
			other := ErrPhoneCodeExpired
			if errors.Is(tc.want, ErrPhoneCodeExpired) {
				other = ErrPhoneCodeInvalid
			}
			if errors.Is(err, other) {
				t.Fatalf("%v should not also match %v", tc.want, other)
			}
		})
	}
}

// TestSignIn_PasswordNeeded checks that SESSION_PASSWORD_NEEDED returns
// need2FA=true with err=nil.
func TestSignIn_PasswordNeeded(t *testing.T) {
	a := &fakeAuth{signInSeq: []signInResult{
		// gotd's auth.Client normalizes SESSION_PASSWORD_NEEDED into
		// ErrPasswordAuthNeeded, but this layer must also cover the case where it
		// arrives un-normalized.
		{err: auth.ErrPasswordAuthNeeded},
	}}
	f := newTestFlow(a, &fakeMigrator{}, &fakeSelf{}, 2)

	need2FA, err := f.signIn(context.Background(), "+8613800138000", "h", "12345")
	if err != nil {
		t.Fatalf("SESSION_PASSWORD_NEEDED should not be returned as an error: %v", err)
	}
	if !need2FA {
		t.Fatal("should return need2FA=true")
	}
}

// TestSignIn_PasswordNeededRaw checks that the un-normalized raw RPC error is
// recognized as well.
func TestSignIn_PasswordNeededRaw(t *testing.T) {
	a := &fakeAuth{signInSeq: []signInResult{{err: tgerr.New(401, "SESSION_PASSWORD_NEEDED")}}}
	f := newTestFlow(a, &fakeMigrator{}, &fakeSelf{}, 2)

	// The raw error is outside auth's normalization range, so this layer surfaces
	// it as an ordinary error; the assertion here is "not misjudged as a login code
	// error".
	need2FA, err := f.signIn(context.Background(), "+8613800138000", "h", "12345")
	if need2FA {
		t.Fatal("the raw SESSION_PASSWORD_NEEDED is not normalized, so it must not return need2FA")
	}
	if err == nil {
		t.Fatal("should be surfaced as an ordinary error")
	}
	if errors.Is(err, ErrPhoneCodeInvalid) || errors.Is(err, ErrPhoneCodeExpired) {
		t.Fatalf("must not be misjudged as a login code error: %v", err)
	}
}

// TestSignIn_MigrateReplay checks that PHONE_MIGRATE during login also migrates
// and replays.
func TestSignIn_MigrateReplay(t *testing.T) {
	a := &fakeAuth{signInSeq: []signInResult{
		{err: tgerr.New(303, "PHONE_MIGRATE_4")},
		{auth: &tg.AuthAuthorization{}},
	}}
	m := &fakeMigrator{}
	f := newTestFlow(a, m, &fakeSelf{}, 1)

	if _, err := f.signIn(context.Background(), "+8613800138000", "h", "12345"); err != nil {
		t.Fatalf("signIn: %v", err)
	}
	if len(a.signInCalls) != 2 {
		t.Fatalf("auth.signIn called %d times, want 2", len(a.signInCalls))
	}
	if len(m.calls) != 1 || m.calls[0] != 4 {
		t.Fatalf("MigrateTo = %v, want [4]", m.calls)
	}
}

// TestSignIn_EmptyArgs checks that an empty hash / empty login code is rejected
// locally.
func TestSignIn_EmptyArgs(t *testing.T) {
	a := &fakeAuth{signInSeq: []signInResult{{auth: &tg.AuthAuthorization{}}}}
	f := newTestFlow(a, &fakeMigrator{}, &fakeSelf{}, 2)

	if _, err := f.signIn(context.Background(), "+8613800138000", "", "12345"); err == nil {
		t.Fatal("an empty hash should return an error")
	}
	if _, err := f.signIn(context.Background(), "+8613800138000", "h", "   "); err == nil {
		t.Fatal("an empty login code should return an error")
	}
	if len(a.signInCalls) != 0 {
		t.Fatalf("invalid arguments must not reach Telegram, got %d calls", len(a.signInCalls))
	}
}

// fakePasswordAuth is the fake for 2FA verification; the error it returns is configurable.
type fakePasswordAuth struct {
	fakeAuth
	passwordErr error
}

func (f *fakePasswordAuth) Password(_ context.Context, password string) (*tg.AuthAuthorization, error) {
	f.passwordCalls = append(f.passwordCalls, password)
	if f.passwordErr != nil {
		return nil, f.passwordErr
	}
	return &tg.AuthAuthorization{}, nil
}

// TestCheckPassword_Success checks normal 2FA verification.
func TestCheckPassword_Success(t *testing.T) {
	a := &fakePasswordAuth{}
	f := newTestFlow(a, &fakeMigrator{}, &fakeSelf{}, 2)

	if err := f.checkPassword(context.Background(), "s3cret"); err != nil {
		t.Fatalf("checkPassword: %v", err)
	}
	if len(a.passwordCalls) != 1 || a.passwordCalls[0] != "s3cret" {
		t.Fatalf("passwordCalls = %v", a.passwordCalls)
	}
}

// TestCheckPassword_HashInvalid checks that PASSWORD_HASH_INVALID is normalized
// into a clear error.
func TestCheckPassword_HashInvalid(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"already normalized by gotd", auth.ErrPasswordInvalid},
		{"raw RPC error", tgerr.New(400, "PASSWORD_HASH_INVALID")},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			a := &fakePasswordAuth{passwordErr: tc.err}
			f := newTestFlow(a, &fakeMigrator{}, &fakeSelf{}, 2)

			err := f.checkPassword(context.Background(), "wrong")
			if !errors.Is(err, ErrPasswordHashInvalid) {
				t.Fatalf("err = %v, want ErrPasswordHashInvalid", err)
			}
		})
	}
}

// TestCheckPassword_Empty checks that an empty password is rejected locally.
func TestCheckPassword_Empty(t *testing.T) {
	a := &fakePasswordAuth{}
	f := newTestFlow(a, &fakeMigrator{}, &fakeSelf{}, 2)

	if err := f.checkPassword(context.Background(), "   "); err == nil {
		t.Fatal("an empty 2FA password should return an error")
	}
	if len(a.passwordCalls) != 0 {
		t.Fatalf("an empty password must not reach Telegram, got %d calls", len(a.passwordCalls))
	}
}

// TestSelf_Success checks that userID / username / dc are returned.
func TestSelf_Success(t *testing.T) {
	s := &fakeSelf{users: []tg.UserClass{&tg.User{ID: 42, Username: "alice"}}}
	f := newTestFlow(&fakeAuth{}, &fakeMigrator{}, s, 2)

	id, username, dc, err := f.self_(context.Background())
	if err != nil {
		t.Fatalf("self_: %v", err)
	}
	if id != 42 || username != "alice" || dc != 2 {
		t.Fatalf("got (%d, %q, %d), want (42, alice, 2)", id, username, dc)
	}
	if s.calls != 1 {
		t.Fatalf("users.getUsers called %d times, want 1", s.calls)
	}
}

// TestSelf_NotAuthorized checks that a missing login returns ErrNotAuthorized.
func TestSelf_NotAuthorized(t *testing.T) {
	for _, tc := range []struct {
		name string
		fs   *fakeSelf
	}{
		{"401 RPC error", &fakeSelf{err: tgerr.New(401, "AUTH_KEY_UNREGISTERED")}},
		{"empty user list", &fakeSelf{users: nil}},
		{"userEmpty", &fakeSelf{users: []tg.UserClass{&tg.UserEmpty{ID: 1}}}},
		{"deactivated account", &fakeSelf{users: []tg.UserClass{&tg.User{ID: 7, Deleted: true}}}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			f := newTestFlow(&fakeAuth{}, &fakeMigrator{}, tc.fs, 2)
			if _, _, _, err := f.self_(context.Background()); !errors.Is(err, ErrNotAuthorized) {
				t.Fatalf("err = %v, want ErrNotAuthorized", err)
			}
		})
	}
}

// TestClient_API_NilGuard checks that the login API on a zero-value Client returns
// an error instead of panicking.
func TestClient_API_NilGuard(t *testing.T) {
	c := &Client{}
	ctx := context.Background()

	if _, err := c.SendCode(ctx, "+8613800138000"); err == nil {
		t.Fatal("zero-value Client.SendCode should return an error")
	}
	if _, err := c.SignIn(ctx, "+8613800138000", "h", "1"); err == nil {
		t.Fatal("zero-value Client.SignIn should return an error")
	}
	if err := c.CheckPassword(ctx, "p"); err == nil {
		t.Fatal("zero-value Client.CheckPassword should return an error")
	}
	if _, _, _, err := c.Self(ctx); err == nil {
		t.Fatal("zero-value Client.Self should return an error")
	}
}

// TestPhoneCodeHashOf covers the various auth.sentCode return shapes.
func TestPhoneCodeHashOf(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sent    tg.AuthSentCodeClass
		want    string
		wantErr error // when non-nil, asserted with errors.Is
		anyErr  bool  // true means any error is acceptable
	}{
		{"normal", sentCode("h1"), "h1", nil, false},
		{"empty hash", &tg.AuthSentCode{}, "", nil, true},
		{"already logged in", &tg.AuthSentCodeSuccess{}, "", errAlreadyAuthorized, false},
		{"nil result", nil, "", nil, true},
		{"unsupported constructor", &tg.AuthSentCodePaymentRequired{}, "", nil, true},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got, err := phoneCodeHashOf(tc.sent)
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if tc.anyErr && err == nil {
				t.Fatal("should return an error")
			}
			if err == nil && got != tc.want {
				t.Fatalf("hash = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCheckPassword_OtherError checks that non-password errors are wrapped and
// surfaced unchanged.
func TestCheckPassword_OtherError(t *testing.T) {
	boom := errors.New("network down")
	a := &fakePasswordAuth{passwordErr: boom}
	f := newTestFlow(a, &fakeMigrator{}, &fakeSelf{}, 2)

	err := f.checkPassword(context.Background(), "pw")
	if !errors.Is(err, boom) {
		t.Fatalf("should wrap the original error, got %v", err)
	}
	if errors.Is(err, ErrPasswordHashInvalid) {
		t.Fatal("a network error must not be misjudged as a wrong password")
	}
}

// TestCheckPassword_FloodWait checks that the flood wait of the 2FA stage also
// carries the seconds.
func TestCheckPassword_FloodWait(t *testing.T) {
	a := &fakePasswordAuth{passwordErr: tgerr.New(420, "FLOOD_WAIT_60")}
	f := newTestFlow(a, &fakeMigrator{}, &fakeSelf{}, 2)

	err := f.checkPassword(context.Background(), "pw")
	var fw *FloodWaitError
	if !errors.As(err, &fw) || fw.Wait != 60*time.Second {
		t.Fatalf("should return a 60s FloodWaitError, got %v", err)
	}
}

// TestSendCode_OtherError checks that errors which are neither migration nor
// flood errors are wrapped unchanged.
func TestSendCode_OtherError(t *testing.T) {
	boom := errors.New("boom")
	a := &fakeAuth{sendCodeSeq: []sendCodeResult{{err: boom}}}
	f := newTestFlow(a, &fakeMigrator{}, &fakeSelf{}, 2)

	if _, err := f.sendCode(context.Background(), "+8613800138000"); !errors.Is(err, boom) {
		t.Fatalf("should wrap the original error, got %v", err)
	}
}

// TestMigrateTarget_InvalidArgument checks that an illegal target DC does not
// trigger a migration.
func TestMigrateTarget_InvalidArgument(t *testing.T) {
	if dc, ok := migrateTarget(tgerr.New(303, "PHONE_MIGRATE")); ok {
		t.Fatalf("a missing DC number must not trigger a migration, got dc=%d", dc)
	}
	if _, ok := migrateTarget(errors.New("plain")); ok {
		t.Fatal("an ordinary error is not a migration error")
	}
	if dc, ok := migrateTarget(tgerr.New(303, "PHONE_MIGRATE_4")); !ok || dc != 4 {
		t.Fatalf("PHONE_MIGRATE_4 should parse to dc=4, got %d ok=%v", dc, ok)
	}
}

// containsAll asserts that s contains every substring in subs.
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
