// errors_test.go - table-driven tests for the Translate coverage list.
// Every error code on the coverage list has at least one assertion (one row each);
// the assertions check the key substring of the output text.
package tg

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gotd/td/tgerr"
)

func TestTranslate_RPC_Coverage(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		err     error
		wantSub string // key substring of the output text
	}{
		// Coverage list: one row per entry.
		{"MESSAGE_ID_INVALID", tgerr.New(400, "MESSAGE_ID_INVALID"), "invalid message ID"},
		{"MSG_ID_INVALID", tgerr.New(400, "MSG_ID_INVALID"), "invalid message ID"},
		{"CHANNEL_PRIVATE", tgerr.New(400, "CHANNEL_PRIVATE"), "join it first"},
		{"CHANNEL_INVALID", tgerr.New(400, "CHANNEL_INVALID"), "/sync"},
		{"CHAT_FORBIDDEN", tgerr.New(400, "CHAT_FORBIDDEN"), "private"},
		{"USER_BANNED_IN_CHANNEL", tgerr.New(400, "USER_BANNED_IN_CHANNEL"), "banned"},
		{"CHAT_ADMIN_REQUIRED", tgerr.New(400, "CHAT_ADMIN_REQUIRED"), "admin"},
		{"FILE_REFERENCE_EXPIRED", tgerr.New(400, "FILE_REFERENCE_EXPIRED"), "file reference expired"},
		{"FILE_REFERENCE_INVALID", tgerr.New(400, "FILE_REFERENCE_INVALID"), "file reference expired"},
		{"FILEREF_UPGRADE_NEEDED", tgerr.New(400, "FILEREF_UPGRADE_NEEDED"), "file reference expired"},
		{"TAKEOUT_REQUIRED", tgerr.New(400, "TAKEOUT_REQUIRED"), "takeout"},
		{"TAKEOUT_INVALID", tgerr.New(400, "TAKEOUT_INVALID"), "takeout"},
		{"AUTH_KEY_UNREGISTERED", tgerr.New(401, "AUTH_KEY_UNREGISTERED"), "log in again"},
		{"AUTH_KEY_INVALID", tgerr.New(401, "AUTH_KEY_INVALID"), "log in again"},
		{"SESSION_REVOKED", tgerr.New(401, "SESSION_REVOKED"), "log in again"},
		{"USER_DEACTIVATED", tgerr.New(401, "USER_DEACTIVATED"), "log in again"},
		{"PEER_ID_INVALID", tgerr.New(400, "PEER_ID_INVALID"), "/sync"},
		{"USERNAME_NOT_OCCUPIED", tgerr.New(400, "USERNAME_NOT_OCCUPIED"), "username does not exist"},
		{"FLOOD_WAIT_42", tgerr.New(420, "FLOOD_WAIT_42"), "42"},
		{"FLOOD_PREMIUM_WAIT_7", tgerr.New(420, "FLOOD_PREMIUM_WAIT_7"), "7"},
		{"STORAGE_CHECK_FAILED", tgerr.New(500, "STORAGE_CHECK_FAILED"), "storage check failed"},
		{"STORAGE_INVALID", tgerr.New(500, "STORAGE_INVALID"), "storage check failed"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := Translate(tt.err)
			if !strings.Contains(got, tt.wantSub) {
				t.Errorf("Translate(%v) = %q, want substring %q", tt.err, got, tt.wantSub)
			}
		})
	}
}

func TestTranslate_FloodWait_Seconds(t *testing.T) {
	t.Parallel()
	got := Translate(tgerr.New(420, "FLOOD_WAIT_42"))
	if !strings.Contains(got, "42") || !strings.Contains(got, "seconds") {
		t.Errorf("FLOOD_WAIT_42 => %q, want 42+seconds", got)
	}
	gotP := Translate(tgerr.New(420, "FLOOD_PREMIUM_WAIT_7"))
	if !strings.Contains(gotP, "7") {
		t.Errorf("FLOOD_PREMIUM_WAIT_7 => %q, want contain 7", gotP)
	}
}

func TestTranslate_Fallback(t *testing.T) {
	t.Parallel()
	// Unknown RPC code: output contains the raw CODE
	got := Translate(tgerr.New(400, "SOME_NEW_CODE"))
	if !strings.Contains(got, "SOME_NEW_CODE") {
		t.Errorf("unknown RPC code => %q, want contain SOME_NEW_CODE", got)
	}
	// Non-RPC unknown error: keep the original text
	boom := errors.New("boom")
	if got := Translate(boom); !strings.Contains(got, "boom") {
		t.Errorf("errors.New(boom) => %q, want contain boom", got)
	}
	// nil: empty string
	if got := Translate(nil); got != "" {
		t.Errorf("Translate(nil) = %q, want empty", got)
	}
}

func TestTranslate_ExistingSentinels(t *testing.T) {
	t.Parallel()
	// Existing sentinels still yield their established text through Translate (errors.Is/As assertions).
	checks := []struct {
		name string
		err  error
		want string
	}{
		{"ErrChannelInaccessible", fmt.Errorf("tg: %w: underlying", ErrChannelInaccessible), ErrChannelInaccessible.Error()},
		{"ErrMessageDeleted", fmt.Errorf("tg: message %d: %w", 1, ErrMessageDeleted), ErrMessageDeleted.Error()},
		{"ErrNoMedia", fmt.Errorf("tg: %w", ErrNoMedia), ErrNoMedia.Error()},
		{"ErrNotAuthorized", fmt.Errorf("tg: query: %w", ErrNotAuthorized), ErrNotAuthorized.Error()},
		{"ErrSessionCorrupted", fmt.Errorf("tg: %w", ErrSessionCorrupted), ErrSessionCorrupted.Error()},
	}
	for _, c := range checks {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_ = sentinelOf(c.err, c.want) // self-check placeholder; the real assertion is the Translate comparison below
			got := Translate(c.err)
			if got != c.want {
				t.Errorf("Translate(%v) = %q, want sentinel text %q", c.err, got, c.want)
			}
		})
	}
}

// sentinelOf returns the sentinel that errors.Is can match in the error chain (test helper).
func sentinelOf(err error, _ string) error {
	switch {
	case errors.Is(err, ErrChannelInaccessible):
		return ErrChannelInaccessible
	case errors.Is(err, ErrMessageDeleted):
		return ErrMessageDeleted
	case errors.Is(err, ErrNoMedia):
		return ErrNoMedia
	case errors.Is(err, ErrNotAuthorized):
		return ErrNotAuthorized
	case errors.Is(err, ErrSessionCorrupted):
		return ErrSessionCorrupted
	}
	return nil
}

func TestTranslate_FloodWaitErrorType(t *testing.T) {
	t.Parallel()
	fw := &FloodWaitError{Op: "send login code", Wait: 42 * time.Second}
	got := Translate(fw)
	if !strings.Contains(got, "42") || !strings.Contains(got, "seconds") {
		t.Errorf("FloodWaitError => %q, want contain 42+seconds", got)
	}
}

func TestTranslate_FS_Errors(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		err     error
		wantSub string
	}{
		{"EACCES", &os.PathError{Op: "open", Path: "/x", Err: syscall.EACCES}, "permission"},
		{"ENOSPC", &os.PathError{Op: "write", Path: "/x", Err: syscall.ENOSPC}, "disk space"},
		{"ENOENT", &os.PathError{Op: "open", Path: "/x", Err: syscall.ENOENT}, "not found"},
		{"osErrPermission", fmt.Errorf("wrap: %w", os.ErrPermission), "permission"},
		{"osErrNotExist", fmt.Errorf("wrap: %w", os.ErrNotExist), "not found"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Translate(tt.err); !strings.Contains(got, tt.wantSub) {
				t.Errorf("Translate(%v) = %q, want substring %q", tt.err, got, tt.wantSub)
			}
		})
	}
}

func TestTranslate_Net_And_Context(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		err     error
		wantSub string
	}{
		{"timeout", &net.OpError{Op: "dial", Err: errTimeoutFake{}}, "timeout"},
		{"connRefused", &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, "refused"},
		{"ctxCanceled", context.Canceled, "cancelled"},
		{"deadline", context.DeadlineExceeded, "timed out"},
		{"rawTimeout", fmt.Errorf("dial: %w", errTimeoutFake{}), "timeout"},
	}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Translate(tt.err); !strings.Contains(got, tt.wantSub) {
				t.Errorf("Translate(%v) = %q, want substring %q", tt.err, got, tt.wantSub)
			}
		})
	}
}

// errTimeoutFake implements net.Error with Timeout()=true, for offline timeout-branch tests.
type errTimeoutFake struct{}

func (errTimeoutFake) Error() string   { return "i/o timeout" }
func (errTimeoutFake) Timeout() bool   { return true }
func (errTimeoutFake) Temporary() bool { return false }
func (errTimeoutFake) Unwrap() error   { return nil }
