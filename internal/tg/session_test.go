// session_test.go - unit tests for SessionStorage (all offline, no network).
//
// Format convention: every legal byte sequence is produced by gotd session.Loader.Save
// (the {Version,Data} wrapper the application actually writes); hand-written JSON
// literals that mirror the implementation are forbidden.
package tg

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gotd/td/session"
)

// newAuthKey generates a fake 256-byte AuthKey and computes AuthKeyID with
// crypto.Key.ID() (bytes 12..19 of the SHA1), the same source as the production
// validation logic.
func newAuthKey(t *testing.T) (key, id []byte) {
	t.Helper()
	key = make([]byte, 256)
	for i := range key {
		key[i] = byte(i * 7 % 251)
	}
	var k cryptoKey
	copy(k[:], key)
	keyID := k.ID()
	id = keyID[:]
	return key, id
}

// saveViaLoader uses gotd session.Loader.Save to produce the bytes the application
// really writes to disk, and returns the file content. This is the only legal source
// of "legal session bytes".
func saveViaLoader(t *testing.T, store *SessionStorage, data *session.Data) []byte {
	t.Helper()
	loader := &session.Loader{Storage: store}
	if err := loader.Save(context.Background(), data); err != nil {
		t.Fatalf("Loader.Save: %v", err)
	}
	buf, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatalf("read persisted bytes: %v", err)
	}
	return buf
}

// rewriteWire rewrites the real bytes produced by Loader.Save in an
// implementation-independent way (map + RawMessage) and returns the new bytes. It never
// references the implementation's internal parse structures.
func rewriteWire(t *testing.T, buf []byte, mutate func(top map[string]json.RawMessage)) []byte {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(buf, &top); err != nil {
		t.Fatalf("the bytes to rewrite are not valid JSON: %v", err)
	}
	mutate(top)
	out, err := json.Marshal(top)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	return out
}

// tamperAuthKeyID replaces Data.AuthKeyID with 8 zero bytes (simulating tampering).
func tamperAuthKeyID(t *testing.T, buf []byte) []byte {
	t.Helper()
	return rewriteWire(t, buf, func(top map[string]json.RawMessage) {
		var dm map[string]any
		if err := json.Unmarshal(top["Data"], &dm); err != nil {
			t.Fatalf("unmarshal Data: %v", err)
		}
		dm["AuthKeyID"] = base64.StdEncoding.EncodeToString(make([]byte, 8))
		raw, err := json.Marshal(dm)
		if err != nil {
			t.Fatalf("marshal Data: %v", err)
		}
		top["Data"] = raw
	})
}

// tamperVersion changes the top-level Version to 2.
func tamperVersion(t *testing.T, buf []byte) []byte {
	t.Helper()
	return rewriteWire(t, buf, func(top map[string]json.RawMessage) {
		top["Version"] = json.RawMessage("2")
	})
}

// flipAuthKeyByte flips the first byte of Data.AuthKey.
func flipAuthKeyByte(t *testing.T, buf []byte) []byte {
	t.Helper()
	return rewriteWire(t, buf, func(top map[string]json.RawMessage) {
		var dm map[string]json.RawMessage
		if err := json.Unmarshal(top["Data"], &dm); err != nil {
			t.Fatalf("unmarshal Data: %v", err)
		}
		var key []byte
		if err := json.Unmarshal(dm["AuthKey"], &key); err != nil {
			t.Fatalf("unmarshal AuthKey: %v", err)
		}
		key[0] ^= 0xFF
		raw, err := json.Marshal(key)
		if err != nil {
			t.Fatalf("marshal AuthKey: %v", err)
		}
		dm["AuthKey"] = raw
		top["Data"] = raw2(dm)
	})
}

// raw2 serializes a map into a RawMessage (helper for flipAuthKeyByte).
func raw2(m map[string]json.RawMessage) json.RawMessage {
	b, _ := json.Marshal(m)
	return b
}

func validLoaderData(t *testing.T) *session.Data {
	t.Helper()
	key, id := newAuthKey(t)
	return &session.Data{DC: 2, Addr: "149.154.167.51:443", AuthKey: key, AuthKeyID: id, Salt: 42}
}

// openStoreAt opens a SessionStorage in a temporary directory.
func openStoreAt(t *testing.T, path string) *SessionStorage {
	t.Helper()
	store, err := OpenSession(path)
	if err != nil {
		t.Fatalf("OpenSession: %v", err)
	}
	return store
}

// TestSessionStorage_LoaderSavedSessionLoads verifies with the real bytes written by
// Loader.Save: the session loads, and gotd Loader.Load reads back consistent data.
func TestSessionStorage_LoaderSavedSessionLoads(t *testing.T) {
	store := openStoreAt(t, filepath.Join(t.TempDir(), "session.json"))
	defer func() { _ = store.Close() }()

	want := validLoaderData(t)
	saveViaLoader(t, store, want)

	buf, err := store.LoadSession(context.Background())
	if err != nil {
		t.Fatalf("LoadSession(legal Loader.Save bytes) should not fail: %v", err)
	}

	loader := &session.Loader{Storage: store}
	got, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("gotd Loader.Load: %v", err)
	}
	if got.DC != want.DC || got.Addr != want.Addr || string(got.AuthKey) != string(want.AuthKey) {
		t.Fatalf("Load read back inconsistent data: DC=%d Addr=%q", got.DC, got.Addr)
	}
	_ = buf
}

// TestSessionStorage_TamperedAuthKeyIDDetected: tampering with the AuthKeyID inside the
// wrapper structure must be detected (this test was red before the fix).
func TestSessionStorage_TamperedAuthKeyIDDetected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	store := openStoreAt(t, path)
	defer func() { _ = store.Close() }()

	buf := saveViaLoader(t, store, validLoaderData(t))
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := os.WriteFile(path, tamperAuthKeyID(t, buf), 0o600); err != nil {
		t.Fatalf("write tampered: %v", err)
	}

	store2 := openStoreAt(t, path)
	defer func() { _ = store2.Close() }()
	_, err := store2.LoadSession(context.Background())
	if !errors.Is(err, ErrSessionCorrupted) {
		t.Fatalf("a tampered AuthKeyID should return ErrSessionCorrupted, got: %v", err)
	}
}

// TestSessionStorage_BadVersionRejected: Version != 1 is treated as corruption.
func TestSessionStorage_BadVersionRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	store := openStoreAt(t, path)

	buf := saveViaLoader(t, store, validLoaderData(t))
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := os.WriteFile(path, tamperVersion(t, buf), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	store2 := openStoreAt(t, path)
	defer func() { _ = store2.Close() }()
	_, err := store2.LoadSession(context.Background())
	if !errors.Is(err, ErrSessionCorrupted) {
		t.Fatalf("Version != 1 should return ErrSessionCorrupted, got: %v", err)
	}
}

// TestSessionStorage_LoaderSavedEmptyAuthKeyAllowed: the not-logged-in intermediate
// state - a session written by Loader.Save without an AuthKey should still load.
func TestSessionStorage_LoaderSavedEmptyAuthKeyAllowed(t *testing.T) {
	store := openStoreAt(t, filepath.Join(t.TempDir(), "session.json"))
	defer func() { _ = store.Close() }()

	saveViaLoader(t, store, &session.Data{DC: 2, Addr: "x:443"})
	if _, err := store.LoadSession(context.Background()); err != nil {
		t.Fatalf("a Loader.Save session without an AuthKey should not fail: %v", err)
	}
}

// TestSessionStorage_RoundTrip: writing then reading back is consistent (through the
// real Loader.Save format).
func TestSessionStorage_RoundTrip(t *testing.T) {
	store := openStoreAt(t, filepath.Join(t.TempDir(), "sub", "session.json"))
	defer func() { _ = store.Close() }()

	if _, err := store.LoadSession(context.Background()); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("should return ErrNotFound when it does not exist, got %v", err)
	}

	want := validLoaderData(t)
	saveViaLoader(t, store, want)

	loader := &session.Loader{Storage: store}
	got, err := loader.Load(context.Background())
	if err != nil {
		t.Fatalf("Loader.Load: %v", err)
	}
	if got.DC != want.DC || got.Addr != want.Addr || string(got.AuthKey) != string(want.AuthKey) || got.Salt != want.Salt {
		t.Fatalf("round-trip data mismatch: DC=%d Addr=%q Salt=%d", got.DC, got.Addr, got.Salt)
	}
}

// TestSessionStorage_FilePerm0600 asserts the on-disk permissions.
func TestSessionStorage_FilePerm0600(t *testing.T) {
	store := openStoreAt(t, filepath.Join(t.TempDir(), "session.json"))
	defer func() { _ = store.Close() }()

	saveViaLoader(t, store, validLoaderData(t))
	info, err := os.Stat(store.Path())
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("file permission = %o, want 0600", perm)
	}
}

// TestSessionStorage_CorruptedJSON detects corrupted JSON.
func TestSessionStorage_CorruptedJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")
	if err := os.WriteFile(path, []byte("this is not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	store := openStoreAt(t, path)
	defer func() { _ = store.Close() }()

	_, err := store.LoadSession(context.Background())
	if !errors.Is(err, ErrSessionCorrupted) {
		t.Fatalf("corrupted JSON should return ErrSessionCorrupted, got: %v", err)
	}
	if !strings.Contains(err.Error(), "log in") {
		t.Fatalf("the error message should ask the user to log in again, got: %v", err)
	}
}

// TestSessionStorage_KeyIDMismatch detects AuthKeyID tampering (real format).
func TestSessionStorage_KeyIDMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	store := openStoreAt(t, path)

	buf := saveViaLoader(t, store, validLoaderData(t))
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := os.WriteFile(path, tamperAuthKeyID(t, buf), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	store2 := openStoreAt(t, path)
	defer func() { _ = store2.Close() }()

	if _, err := store2.LoadSession(context.Background()); !errors.Is(err, ErrSessionCorrupted) {
		t.Fatalf("a mismatched KeyID should return ErrSessionCorrupted, got: %v", err)
	}
}

// TestSessionStorage_AuthKeyByteFlip: flipping a single AuthKey byte (the wrapper JSON
// stays valid) must be detected as well.
func TestSessionStorage_AuthKeyByteFlip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.json")
	store := openStoreAt(t, path)

	buf := saveViaLoader(t, store, validLoaderData(t))
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := os.WriteFile(path, flipAuthKeyByte(t, buf), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	store2 := openStoreAt(t, path)
	defer func() { _ = store2.Close() }()

	if _, err := store2.LoadSession(context.Background()); !errors.Is(err, ErrSessionCorrupted) {
		t.Fatalf("the AuthKey flip should be detected, got: %v", err)
	}
}

// TestSessionStorage_Mutex: two-instance mutual exclusion - the second open must fail,
// and opening works again after the release.
func TestSessionStorage_Mutex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "session.json")

	first, err := OpenSession(path)
	if err != nil {
		t.Fatalf("first OpenSession: %v", err)
	}

	second, err := OpenSession(path)
	if err == nil {
		_ = second.Close()
		_ = first.Close()
		t.Fatal("the second instance opening the same path should fail")
	}
	if !strings.Contains(err.Error(), "session") {
		t.Fatalf("the lock error should be readable and mention the session, got: %v", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	third, err := OpenSession(path)
	if err != nil {
		t.Fatalf("re-open after release: %v", err)
	}
	if err := third.Close(); err != nil {
		t.Fatalf("third Close: %v", err)
	}
}

// TestSessionStorage_StoreRejectsBadData refuses to persist corrupted data.
func TestSessionStorage_StoreRejectsBadData(t *testing.T) {
	store := openStoreAt(t, filepath.Join(t.TempDir(), "session.json"))
	defer func() { _ = store.Close() }()

	if err := store.StoreSession(context.Background(), []byte("not json")); err == nil {
		t.Error("storing non-JSON should return an error")
	}
	buf := saveViaLoader(t, store, validLoaderData(t))
	if err := store.StoreSession(context.Background(), tamperAuthKeyID(t, buf)); err == nil {
		t.Error("storing a session with a mismatched KeyID should return an error")
	}
	if err := store.StoreSession(context.Background(), tamperVersion(t, buf)); err == nil {
		t.Error("storing a session with an invalid Version should return an error")
	}
	got, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatalf("read the persisted file: %v", err)
	}
	if !bytes.Equal(got, buf) {
		t.Error("rejected data must not overwrite what is already on disk")
	}
}

// TestSessionStorage_PathAndIdempotentClose covers Path and Close idempotency.
func TestSessionStorage_PathAndIdempotentClose(t *testing.T) {
	store := openStoreAt(t, filepath.Join(t.TempDir(), "s.json"))
	if got := store.Path(); got == "" {
		t.Fatal("Path should not be empty")
	}
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("repeated Close should be idempotent: %v", err)
	}
}
