// session.go - a single-file JSON implementation of gotd SessionStorage.
//
// Design notes:
//   - The data format matches gotd session.Loader's wire format ({Version,Data}
//     JSON); this file only persists and validates, encoding and decoding are
//     fully left to session.Loader's format contract.
//   - Disk writes go through internal/fs.AtomicWriteFile (temp file in the same
//     directory + fsync + rename + directory fsync), so a half-written JSON
//     file cannot happen.
//   - Permissions are fixed at 0600: the file contains the AuthKey, which is
//     equivalent to the account password.
//   - On load, crypto.Key.ID() (bytes 12..19 of the SHA1, i.e. the "low 8 bytes"
//     of the Telegram spec) validates AuthKeyID; a mismatch means a corrupted
//     file and returns a clear error asking the user to run /login again, and
//     never silently drops the key and generates a new one.
package tg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gotd/td/crypto"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"

	"github.com/1368129224/tgshelf/internal/fs"
)

// Compile-time proof: this type also satisfies telegram.SessionStorage (an alias
// of session.Storage), so it can be passed straight to telegram.Options.SessionStorage.
var _ telegram.SessionStorage = (*SessionStorage)(nil)

// cryptoKey is a local alias for crypto.Key.
type cryptoKey = crypto.Key

// jsonUnmarshal is the package's single wrapper around encoding/json.Unmarshal
// (session.Loader also uses encoding/json, so the encodings are compatible).
func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// ErrSessionCorrupted reports that the session file exists but its content cannot
// be trusted (broken JSON, or an AuthKey that does not match AuthKeyID). Callers
// should ask the user to log in again (CLI: tgshelf -login; bot: /login) instead
// of deleting the file and logging in automatically.
var ErrSessionCorrupted = errors.New("session is corrupted, please log in again (CLI: tgshelf -login)")

// sessionFilePerm is the on-disk permission of the session file: owner read/write only.
const sessionFilePerm os.FileMode = 0o600

// SessionStorage persists gotd's session data as a single JSON file. It implements
// the session.Storage (telegram.SessionStorage) interface and provides process-level
// mutual exclusion through flock.
type SessionStorage struct {
	path string
	lock *fs.Lock
}

// OpenSession opens the session file storage at path and takes the process lock.
//
// Mutual exclusion: an exclusive flock on path+".lock". While another process
// holds the lock it retries at the default cadence and returns a readable error
// once the timeout expires. The returned storage must be closed to release the lock.
func OpenSession(path string) (*SessionStorage, error) {
	if path == "" {
		return nil, fmt.Errorf("tg.OpenSession: session path is empty")
	}
	if err := fs.EnsureDir(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("tg.OpenSession: %w", err)
	}

	lock, err := fs.AcquireLock(path+".lock", fs.LockOptions{})
	if err != nil {
		return nil, err // fs.AcquireLock already returns a readable error
	}

	return &SessionStorage{path: path, lock: lock}, nil
}

// Path returns the on-disk path of the session file.
func (s *SessionStorage) Path() string { return s.path }

// Compile-time proof: this type satisfies gotd's session.Storage interface.
var _ session.Storage = (*SessionStorage)(nil)

// LoadSession implements session.Storage: it reads and validates the session file.
//
// Return contract:
//   - file missing / empty file: session.ErrNotFound (gotd then takes the
//     not-logged-in path);
//   - broken JSON or a failed AuthKeyID check: an error wrapping ErrSessionCorrupted;
//   - normal (including a not-logged-in session without an AuthKey): the session
//     JSON bytes.
func (s *SessionStorage) LoadSession(ctx context.Context) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, session.ErrNotFound
		}
		return nil, fmt.Errorf("tg: read session file %q: %w", s.path, err)
	}

	if len(data) == 0 {
		return nil, session.ErrNotFound
	}

	if err := validateSessionJSON(data); err != nil {
		return nil, err
	}
	return data, nil
}

// StoreSession implements session.Storage: it atomically writes the session JSON
// with 0600 permissions. It validates before writing so corrupt data never reaches
// the disk.
func (s *SessionStorage) StoreSession(ctx context.Context, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(data) == 0 {
		return nil // empty data is not persisted (defensive; gotd never emits it)
	}

	if err := validateSessionJSON(data); err != nil {
		return fmt.Errorf("tg: refuse to store untrusted session data: %w", err)
	}

	return fs.AtomicWriteFile(s.path, data, sessionFilePerm)
}

// Close releases the process lock. Idempotent.
func (s *SessionStorage) Close() error {
	if s == nil || s.lock == nil {
		return nil
	}
	lock := s.lock
	s.lock = nil
	if err := lock.Release(); err != nil {
		return fmt.Errorf("tg: release session lock: %w", err)
	}
	return nil
}

// sessionJSON is gotd session.Loader's on-disk format: an outer version wrapper
// with the real session data in the inner Data field (see session.Loader.Load/Save).
// Note: unmarshalling straight into session.Data would skip validation forever -
// the top level has no AuthKey field, its length is always 0, so validation would
// look like it "passes" while never running.
type sessionJSON struct {
	Version int
	Data    session.Data
}

// sessionLatestVersion is the only version number gotd session.Loader currently
// accepts (it matches its internal latestVersion constant). A different version
// means the file was written by an incompatible implementation and cannot be trusted.
const sessionLatestVersion = 1

// validateSessionJSON validates the session JSON:
//   - it must parse as session.Loader's wrapper format ({Version,Data});
//   - Version must equal sessionLatestVersion;
//   - when the inner Data.AuthKey is non-empty, AuthKeyID must equal crypto.Key(AuthKey).ID().
//
// No AuthKey is a legal intermediate state meaning "not logged in yet".
func validateSessionJSON(data []byte) error {
	var parsed sessionJSON
	if err := jsonUnmarshal(data, &parsed); err != nil {
		return fmt.Errorf("%w: JSON parse failed: %w", ErrSessionCorrupted, err)
	}

	if parsed.Version != sessionLatestVersion {
		return fmt.Errorf("%w: format version %d is not supported (want %d)",
			ErrSessionCorrupted, parsed.Version, sessionLatestVersion)
	}

	if len(parsed.Data.AuthKey) == 0 {
		return nil
	}

	var key cryptoKey
	copy(key[:], parsed.Data.AuthKey)
	id := key.ID()
	if len(parsed.Data.AuthKeyID) != len(id) {
		return fmt.Errorf("%w: AuthKeyID has an unexpected length", ErrSessionCorrupted)
	}
	for i := range id {
		if parsed.Data.AuthKeyID[i] != id[i] {
			return fmt.Errorf("%w: AuthKeyID does not match AuthKey (file tampered with or corrupted)",
				ErrSessionCorrupted)
		}
	}
	return nil
}
