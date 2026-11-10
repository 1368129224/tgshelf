// Package fs white-box tests: cover the AtomicWriteFile error paths by replacing the OS
// call hooks.
package fs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var errInjected = errors.New("injected error")

// withHooks replaces the hooks temporarily and restores them when the test ends.
func withHooks(t *testing.T, apply func()) {
	t.Helper()
	savedSync := osSyncFile
	savedClose := osCloseFile
	savedChmod := osChmod
	savedRename := osRename
	savedOpenDir := osOpenDir
	t.Cleanup(func() {
		osSyncFile = savedSync
		osCloseFile = savedClose
		osChmod = savedChmod
		osRename = savedRename
		osOpenDir = savedOpenDir
	})
	apply()
}

func TestAtomicWriteFile_SyncError(t *testing.T) {
	withHooks(t, func() {
		callCount := 0
		osSyncFile = func(f *os.File) error {
			callCount++
			// The first call is the file Sync, the second is the directory Sync
			if callCount == 1 {
				return errInjected
			}
			return f.Sync()
		}

		dir := t.TempDir()
		path := filepath.Join(dir, "out.txt")
		err := AtomicWriteFile(path, []byte("data"), 0o644)
		if err == nil || !strings.Contains(err.Error(), "sync") {
			t.Errorf("expected sync error, got: %v", err)
		}

		// No leftover temp files
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.Contains(e.Name(), ".tmp.") {
				t.Errorf("leftover temp file: %q", e.Name())
			}
		}
	})
}

func TestAtomicWriteFile_CloseError(t *testing.T) {
	withHooks(t, func() {
		callCount := 0
		osCloseFile = func(f *os.File) error {
			callCount++
			if callCount == 1 {
				// Inject an error on the first Close (the file Close)
				_ = f.Close() // really close, to prevent an fd leak
				return errInjected
			}
			return f.Close()
		}

		dir := t.TempDir()
		path := filepath.Join(dir, "out.txt")
		err := AtomicWriteFile(path, []byte("data"), 0o644)
		if err == nil || !strings.Contains(err.Error(), "close") {
			t.Errorf("expected close error, got: %v", err)
		}

		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.Contains(e.Name(), ".tmp.") {
				t.Errorf("leftover temp file: %q", e.Name())
			}
		}
	})
}

func TestAtomicWriteFile_ChmodError(t *testing.T) {
	withHooks(t, func() {
		osChmod = func(name string, mode os.FileMode) error {
			return errInjected
		}

		dir := t.TempDir()
		path := filepath.Join(dir, "out.txt")
		err := AtomicWriteFile(path, []byte("data"), 0o644)
		if err == nil || !strings.Contains(err.Error(), "chmod") {
			t.Errorf("expected chmod error, got: %v", err)
		}

		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.Contains(e.Name(), ".tmp.") {
				t.Errorf("leftover temp file: %q", e.Name())
			}
		}
	})
}

func TestAtomicWriteFile_RenameError(t *testing.T) {
	withHooks(t, func() {
		osRename = func(oldpath, newpath string) error {
			// Make the real rename fail: skip it and return the error
			return errInjected
		}

		dir := t.TempDir()
		path := filepath.Join(dir, "out.txt")
		err := AtomicWriteFile(path, []byte("data"), 0o644)
		if err == nil || !strings.Contains(err.Error(), "rename") {
			t.Errorf("expected rename error, got: %v", err)
		}

		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if strings.Contains(e.Name(), ".tmp.") {
				t.Errorf("leftover temp file: %q", e.Name())
			}
		}
	})
}

func TestAtomicWriteFile_DirOpenError(t *testing.T) {
	withHooks(t, func() {
		osOpenDir = func(dir string) (*os.File, error) {
			return nil, errInjected
		}

		dir := t.TempDir()
		path := filepath.Join(dir, "out.txt")
		err := AtomicWriteFile(path, []byte("data"), 0o644)
		if err == nil || !strings.Contains(err.Error(), "open dir") {
			t.Errorf("expected dir open error, got: %v", err)
		}
	})
}

func TestAtomicWriteFile_DirSyncError(t *testing.T) {
	withHooks(t, func() {
		callCount := 0
		osSyncFile = func(f *os.File) error {
			callCount++
			if callCount == 1 {
				// The file Sync succeeds
				return f.Sync()
			}
			// The directory Sync fails
			return errInjected
		}

		dir := t.TempDir()
		path := filepath.Join(dir, "out.txt")
		err := AtomicWriteFile(path, []byte("data"), 0o644)
		if err == nil || !strings.Contains(err.Error(), "dir fsync") {
			t.Errorf("expected dir fsync error, got: %v", err)
		}
	})
}
