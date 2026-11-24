// errors.go - the error translation layer: translates gotd RPC errors, filesystem
// errors, network errors, and context errors into a single actionable sentence for users.
//
// Design constraint: decide by type/constant first (tgerr.Is(err, "CODE"),
// errors.Is, net.Error, syscall.Errno); never use substring matching on English text as the
// primary mechanism.
//
// Sentinels added by this file are all named ErrXxx and can be matched with errors.Is;
// existing sentinels (ErrChannelInaccessible, ErrMessageDeleted, ErrNoMedia,
// ErrNotAuthorized, FloodWaitError, ...) keep their types and text and are passed through.
package tg

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/gotd/td/tgerr"
)

// Sentinels added by this file (all matchable with errors.Is).

// ErrChannelPrivate: the target channel/group is private (CHANNEL_PRIVATE / CHAT_FORBIDDEN),
// this account is not a member and has no access. Match with errors.Is.
var ErrChannelPrivate = errors.New("this channel/group is private: join it first, then send /sync")

// ErrPeerInvalid: the session has no such dialog entity (PEER_ID_INVALID / CHANNEL_INVALID /
// USERNAME_NOT_OCCUPIED and so on), usually because /sync has not cached that entity yet.
// Match with errors.Is.
var ErrPeerInvalid = errors.New("unknown dialog/channel: send /sync to sync the session, then retry")

// ErrFileRefExpired: the file reference has expired (FILE_REFERENCE_EXPIRED /
// FILE_REFERENCE_INVALID / FILEREF_UPGRADE_NEEDED); the message must be fetched again
// before retrying. Match with errors.Is.
var ErrFileRefExpired = errors.New("file reference expired: resend the message link to get a fresh reference, then retry")

// ErrTakeoutRequired: the operation must run inside a takeout session (TAKEOUT_REQUIRED /
// TAKEOUT_INVALID). Match with errors.Is.
var ErrTakeoutRequired = errors.New("this operation requires a takeout session: start a takeout session and retry")

// ErrAuthKeyInvalid: the authorization key is no longer valid (AUTH_KEY_UNREGISTERED /
// AUTH_KEY_INVALID / SESSION_REVOKED / USER_DEACTIVATED); logging in again is required.
// Match with errors.Is.
var ErrAuthKeyInvalid = errors.New("login session is no longer valid: log in again (CLI: tgshelf -login)")

// ErrChatAdminRequired: channel admin rights are required (CHAT_ADMIN_REQUIRED /
// USER_BANNED_IN_CHANNEL). Match with errors.Is.
var ErrChatAdminRequired = errors.New("permission denied: this operation requires channel admin rights")

// ErrStorageCheckFailed: the server-side file storage check failed (STORAGE_CHECK_FAILED
// and other STORAGE-class errors). Match with errors.Is.
var ErrStorageCheckFailed = errors.New("telegram file storage check failed: retry later, or fetch the media of that message again")

// ErrMsgIDInvalid: the message ID is invalid (MESSAGE_ID_INVALID / MSG_ID_INVALID).
// Match with errors.Is.
var ErrMsgIDInvalid = errors.New("invalid message ID: check that the message the link points to exists, then send /sync again")

// ErrDiskFull: not enough disk space (ENOSPC / no space left on device).
// Match with errors.Is.
var ErrDiskFull = errors.New("not enough disk space: free space on the disk holding the download directory and retry")

// ErrDirNotFound: the target directory or file does not exist (no such file or directory).
// Match with errors.Is.
var ErrDirNotFound = errors.New("download directory or file not found: check that the download directory path is correct")

// ErrTimeout: the network timed out. Match with errors.Is.
var ErrTimeout = errors.New("network timeout: check the network connection or proxy settings and retry")

// ErrConnRefused: the connection was refused. Match with errors.Is.
var ErrConnRefused = errors.New("connection refused: check that the network and the proxy address are reachable")

// errCodeMap: RPC error codes that cannot be normalized into a sentinel but still need
// user-facing text -> message.
var errCodeMap = map[string]string{
	"CHANNEL_INVALID":       "channel does not exist or is no longer valid: check the link, then send /sync to resync",
	"USERNAME_NOT_OCCUPIED": "username does not exist: check the username in the link for typos",
}

// Translate turns any error into a single actionable sentence for users.
// When an existing sentinel or a known error code matches, it returns the actionable
// message; unknown errors keep their original text and no detail is ever swallowed.
// Translate returns an empty string when err is nil.
func Translate(err error) string {
	if err == nil {
		return ""
	}

	// Layer 0: existing sentinels of this package; return their established text unchanged.
	if errors.Is(err, ErrChannelInaccessible) {
		return ErrChannelInaccessible.Error()
	}
	if errors.Is(err, ErrMessageDeleted) {
		return ErrMessageDeleted.Error()
	}
	if errors.Is(err, ErrNoMedia) {
		return ErrNoMedia.Error()
	}
	if errors.Is(err, ErrNotAuthorized) {
		return ErrNotAuthorized.Error()
	}
	if errors.Is(err, ErrSessionCorrupted) {
		return ErrSessionCorrupted.Error()
	}
	if fw := (*FloodWaitError)(nil); errors.As(err, &fw) && fw != nil {
		return fmt.Sprintf("rate limited: wait %d seconds and retry", int(fw.Wait/time.Second))
	}

	// Layer 1: sentinels added by this file (checked before raw code parsing, more specific).
	if errors.Is(err, ErrChannelPrivate) {
		return ErrChannelPrivate.Error()
	}
	// Check ErrRefreshTooFrequent before ErrFileRefExpired since it wraps ErrFileRefExpired.
	if errors.Is(err, ErrRefreshTooFrequent) {
		return ErrRefreshTooFrequent.Error()
	}
	if errors.Is(err, ErrFileRefExpired) {
		return ErrFileRefExpired.Error()
	}
	if errors.Is(err, ErrTakeoutRequired) {
		return ErrTakeoutRequired.Error()
	}
	if errors.Is(err, ErrAuthKeyInvalid) {
		return ErrAuthKeyInvalid.Error()
	}
	if errors.Is(err, ErrChatAdminRequired) {
		return ErrChatAdminRequired.Error()
	}
	if errors.Is(err, ErrStorageCheckFailed) {
		return ErrStorageCheckFailed.Error()
	}
	if errors.Is(err, ErrPeerInvalid) {
		return ErrPeerInvalid.Error()
	}
	if errors.Is(err, ErrMsgIDInvalid) {
		return ErrMsgIDInvalid.Error()
	}
	if errors.Is(err, ErrDiskFull) {
		return ErrDiskFull.Error()
	}
	if errors.Is(err, ErrDirNotFound) {
		return ErrDirNotFound.Error()
	}
	if errors.Is(err, ErrTimeout) {
		return ErrTimeout.Error()
	}
	if errors.Is(err, ErrConnRefused) {
		return ErrConnRefused.Error()
	}

	// Layer 2: context errors.
	if errors.Is(err, context.Canceled) {
		return "operation cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "operation timed out: check the network connection and retry"
	}

	// Layer 3: gotd RPC errors (from specific to general, following the coverage list).
	if rpcErr := (*tgerr.Error)(nil); errors.As(err, &rpcErr) && rpcErr != nil {
		if s, ok := translateRPC(rpcErr); ok {
			return s
		}
		// Fallback: the error returned by Telegram, <CODE>
		return fmt.Sprintf("Telegram returned error %s (HTTP %d)", rpcErr.Message, rpcErr.Code)
	}

	// Layer 4: filesystem errors.
	if s, ok := translateFS(err); ok {
		return s
	}

	// Layer 5: network errors.
	if s, ok := translateNet(err); ok {
		return s
	}

	// Fallback: unknown errors keep their original text, no information is swallowed.
	return err.Error()
}

// translateRPC translates a known *tgerr.Error into user-facing text; ok=false means no hit.
func translateRPC(e *tgerr.Error) (string, bool) {
	// FLOOD_WAIT_X / FLOOD_PREMIUM_WAIT_X: extract the number of seconds.
	if d, ok := tgerr.AsFloodWait(e); ok {
		if e.IsType(tgerr.ErrPremiumFloodWait) {
			return fmt.Sprintf("rate limited on a non-Premium account: wait %d seconds and retry, "+
				"or upgrade to Telegram Premium to lift the limit",
				int(d/time.Second)), true
		}
		return fmt.Sprintf("rate limited: wait %d seconds and retry", int(d/time.Second)), true
	}

	switch {
	case tgerr.Is(e, "MESSAGE_ID_INVALID", "MSG_ID_INVALID"):
		return ErrMsgIDInvalid.Error(), true
	case tgerr.Is(e, "CHANNEL_PRIVATE", "CHAT_FORBIDDEN"):
		return ErrChannelPrivate.Error(), true
	case tgerr.Is(e, "USER_BANNED_IN_CHANNEL"):
		return "you are banned in this channel: its content cannot be fetched", true
	case tgerr.Is(e, "CHAT_ADMIN_REQUIRED"):
		return ErrChatAdminRequired.Error(), true
	case tgerr.Is(e, "FILE_REFERENCE_EXPIRED", "FILE_REFERENCE_INVALID", "FILEREF_UPGRADE_NEEDED"):
		return ErrFileRefExpired.Error(), true
	case tgerr.Is(e, "TAKEOUT_REQUIRED", "TAKEOUT_INVALID"):
		return ErrTakeoutRequired.Error(), true
	case tgerr.Is(e, "AUTH_KEY_UNREGISTERED", "AUTH_KEY_INVALID", "SESSION_REVOKED", "USER_DEACTIVATED"):
		return ErrAuthKeyInvalid.Error(), true
	case tgerr.Is(e, "PEER_ID_INVALID", "CHANNEL_INVALID"):
		return ErrPeerInvalid.Error(), true
	case tgerr.Is(e, "USERNAME_NOT_OCCUPIED"):
		return errCodeMap["USERNAME_NOT_OCCUPIED"], true
	case tgerr.Is(e, "STORAGE_CHECK_FAILED", "STORAGE_INVALID"):
		return ErrStorageCheckFailed.Error(), true
	}
	return "", false
}

// translateFS translates filesystem errors; ok=false means no hit.
// Filesystem errors carry no uniform type field, so the contract allows errors.Is plus
// syscall.Errno checks.
func translateFS(err error) (string, bool) {
	// syscall.Errno first (covers ENOSPC / ENOENT / EACCES and so on).
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.ENOSPC:
			return ErrDiskFull.Error(), true
		case syscall.ENOENT:
			return ErrDirNotFound.Error(), true
		case syscall.EACCES, syscall.EPERM:
			return "no permission to access the download directory: check its read/write permissions", true
		}
	}
	// errors.Is fallback (cross-platform path: os.ErrPermission / os.ErrNotExist).
	if errors.Is(err, os.ErrPermission) {
		return "no permission to access the download directory: check its read/write permissions", true
	}
	if errors.Is(err, os.ErrNotExist) {
		return ErrDirNotFound.Error(), true
	}
	// String fallback only when every type check failed (lowest priority allowed by the contract).
	msg := err.Error()
	switch {
	case strings.Contains(msg, "no space left on device"):
		return ErrDiskFull.Error(), true
	case strings.Contains(msg, "permission denied"):
		return "no permission to access the download directory: check its read/write permissions", true
	case strings.Contains(msg, "no such file or directory"):
		return ErrDirNotFound.Error(), true
	}
	return "", false
}

// translateNet translates network errors; ok=false means no hit.
func translateNet(err error) (string, bool) {
	var ne net.Error
	if errors.As(err, &ne) {
		if ne.Timeout() {
			return ErrTimeout.Error(), true
		}
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		if errors.Is(oe.Err, syscall.ECONNREFUSED) {
			return ErrConnRefused.Error(), true
		}
	}
	// syscall.Errno fallback (ECONNREFUSED may arrive without an OpError wrapper).
	var errno syscall.Errno
	if errors.As(err, &errno) && errno == syscall.ECONNREFUSED {
		return ErrConnRefused.Error(), true
	}
	// String fallback only when every type check failed.
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection refused"):
		return ErrConnRefused.Error(), true
	case strings.Contains(msg, "i/o timeout"), strings.Contains(msg, "timeout"):
		return ErrTimeout.Error(), true
	}
	return "", false
}
