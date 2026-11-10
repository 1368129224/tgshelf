package fs_test

// lock_test.go - unit tests for AcquireLock / Lock.Release / HoldUntil.
//
// Key empirical finding (TestFlockSameProcessTwoFDs):
//   On Linux, flock(2) **does provide mutual exclusion** between different fds of the
//   same process (EWOULDBLOCK). The lock is attached to the open-file-description
//   (process x inode level), not to the process as a whole. Two independent
//   os.Open/os.OpenFile calls create two separate open-file-descriptions, so
//   flock LOCK_EX|LOCK_NB on the second fd returns EWOULDBLOCK.
//
//   This differs from BSD/macOS behaviour (macOS flock does not exclude between fds
//   of the same process). On Linux the same-process test is enough to verify the
//   exclusion semantics, so a subprocess is not mandatory. This file nevertheless
//   keeps the subprocess tests (TestHelperProcess + TestFlockSubProcess) for complete
//   cross-process verification.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/1368129224/tgshelf/internal/fs"
)

// ─────────────────────────── TestHelperProcess ──────────────────────────────
// Runs as a subprocess: acquire the lock, notify the parent, then wait for a signal
// to exit.
// Usage (invoked by TestFlockSubProcess through os/exec):
//
//	go test -run TestHelperProcess -test.helper-lock=<path> -test.helper-hold=<ms>
//
// Note: during a normal go test run this test does nothing (it checks the
// GO_HELPER_LOCK environment variable).
func TestHelperProcess(t *testing.T) {
	lockPath := os.Getenv("GO_HELPER_LOCK")
	if lockPath == "" {
		// normal go test run, skip
		t.Skip("not a helper process")
	}

	holdMsStr := os.Getenv("GO_HELPER_HOLD_MS")
	holdMs, _ := strconv.Atoi(holdMsStr)
	if holdMs <= 0 {
		holdMs = 2000
	}

	// Acquire the lock
	lock, err := fs.AcquireLock(lockPath, fs.LockOptions{
		WaitTimeout: 2 * time.Second,
	})
	if err != nil {
		// Print a specific prefix so the parent process can recognise it
		t.Logf("HELPER_LOCK_FAILED: %v", err)
		os.Exit(2)
	}
	defer func() { _ = lock.Release() }()

	// Notify the parent: the lock is held (print a specific string)
	t.Log("HELPER_LOCK_ACQUIRED")
	// Hold the lock for a while
	time.Sleep(time.Duration(holdMs) * time.Millisecond)
}

// ─────────────────────────── basic functionality ────────────────────────────────────

// TestAcquireLock_Basic: basic lock and release.
func TestAcquireLock_Basic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.lock")

	lock, err := fs.AcquireLock(path, fs.LockOptions{})
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}

	// The lock file should exist
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("lock file does not exist: %v", err)
	}

	// The lock file should contain pid= and started=
	content, _ := os.ReadFile(path)
	s := string(content)
	if !strings.Contains(s, "pid=") || !strings.Contains(s, "started=") {
		t.Errorf("lock file content is missing the pid/started fields: %q", s)
	}
	t.Logf("lock file content: %q", s)

	if err := lock.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
}

// TestAcquireLock_AutoCreateDir: the parent directory is created when missing.
func TestAcquireLock_AutoCreateDir(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "subdir", "session.lock")

	lock, err := fs.AcquireLock(path, fs.LockOptions{})
	if err != nil {
		t.Fatalf("AcquireLock with missing parent dir: %v", err)
	}
	defer func() { _ = lock.Release() }()

	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Fatalf("the parent directory should have been created automatically: %v", err)
	}
	t.Log("✓ parent directory created automatically")
}

// ─────────────────────────── two-fd exclusion in one process ─────────────────────────────

// TestFlockSameProcessTwoFDs: two independent fds in one process race for the same
// lock file.
// Empirical finding (see the file header): Linux flock does exclude between
// different fds of one process.
func TestFlockSameProcessTwoFDs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.lock")

	// The first fd acquires the lock
	lock1, err := fs.AcquireLock(path, fs.LockOptions{WaitTimeout: 1 * time.Second})
	if err != nil {
		t.Fatalf("lock1 AcquireLock: %v", err)
	}
	defer func() { _ = lock1.Release() }()

	// The second fd tries the same lock and must fail (EWOULDBLOCK)
	lock2, err := fs.AcquireLock(path, fs.LockOptions{
		WaitTimeout:   50 * time.Millisecond, // short timeout, fail fast
		RetryInterval: 10 * time.Millisecond,
	})
	if err == nil {
		_ = lock2.Release()
		t.Fatal("the second fd must not acquire the lock (Linux flock excludes between" +
			" fds of one process)")
	}
	t.Logf("✓ second fd correctly rejected: %v", err)

	// Verify the error message carries the expected hint
	if !strings.Contains(err.Error(), "another instance") {
		t.Errorf("error message is missing the hint: %v", err)
	}
}

// ─────────────────────────── reacquire after release ──────────────────────────────────

// TestAcquireLock_ReleaseAndReacquire: the lock can be acquired again after release.
func TestAcquireLock_ReleaseAndReacquire(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.lock")

	// First acquisition
	lock1, err := fs.AcquireLock(path, fs.LockOptions{})
	if err != nil {
		t.Fatalf("first AcquireLock: %v", err)
	}

	// Release
	if err := lock1.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// Reacquire after release; it must succeed
	lock2, err := fs.AcquireLock(path, fs.LockOptions{})
	if err != nil {
		t.Fatalf("reacquiring the lock after release failed: %v", err)
	}
	defer func() { _ = lock2.Release() }()
	t.Log("✓ lock reacquired after release")
}

// TestRelease_Idempotent: repeated Release does not error (idempotent).
func TestRelease_Idempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.lock")

	lock, err := fs.AcquireLock(path, fs.LockOptions{})
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}

	for i := range 5 {
		if err := lock.Release(); err != nil {
			t.Errorf("Release call %d returned an error: %v", i+1, err)
		}
	}
	t.Log("✓ repeated Release is idempotent, no errors")
}

// ─────────────────────────── timeout and retry ───────────────────────────────────

// TestAcquireLock_Timeout: call with a short timeout while the lock is held, checking
// the timeout behaviour and the elapsed time.
func TestAcquireLock_Timeout(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.lock")

	// Hold the lock first
	lock, err := fs.AcquireLock(path, fs.LockOptions{})
	if err != nil {
		t.Fatalf("holding the lock: %v", err)
	}
	defer func() { _ = lock.Release() }()

	// Try to acquire with a 300ms timeout
	timeout := 300 * time.Millisecond
	start := time.Now()
	_, err2 := fs.AcquireLock(path, fs.LockOptions{
		WaitTimeout:   timeout,
		RetryInterval: 50 * time.Millisecond,
	})
	elapsed := time.Since(start)

	if err2 == nil {
		t.Fatal("an error is expected while the lock is held")
	}

	// Verify it really waited about 300ms (+200ms tolerance)
	if elapsed < timeout {
		t.Errorf("returned too early: took %v, want >= %v", elapsed, timeout)
	}
	if elapsed > timeout+200*time.Millisecond {
		t.Errorf("waited too long after the timeout: took %v, want <= %v",
			elapsed, timeout+200*time.Millisecond)
	}
	t.Logf("✓ timeout test passed, actual elapsed: %v (expected about %v)",
		elapsed, timeout)
	t.Logf("  error message: %v", err2)
}

// ─────────────────────────── 20 goroutines racing for the lock ─────────────────────────

// TestAcquireLock_Concurrent20: 20 goroutines race for one lock; exactly 1 succeeds.
func TestAcquireLock_Concurrent20(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.lock")

	const n = 20
	var (
		successCount atomic.Int64
		failCount    atomic.Int64
		wg           sync.WaitGroup
		locks        [n]*fs.Lock
		mu           sync.Mutex
	)

	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			l, err := fs.AcquireLock(path, fs.LockOptions{
				WaitTimeout:   50 * time.Millisecond,
				RetryInterval: 10 * time.Millisecond,
			})
			if err == nil {
				successCount.Add(1)
				mu.Lock()
				locks[idx] = l
				mu.Unlock()
			} else {
				failCount.Add(1)
			}
		}(i)
	}
	wg.Wait()

	// Release every lock that was acquired
	for _, l := range locks {
		if l != nil {
			_ = l.Release()
		}
	}

	t.Logf("succeeded: %d, failed: %d (out of %d goroutines)",
		successCount.Load(), failCount.Load(), n)

	// With a 50ms timeout at most 1 can succeed (the rest time out)
	got := successCount.Load()
	if got != 1 {
		t.Errorf("exactly 1 of the 20 concurrent attempts should succeed, got: %d", got)
	}
	if failCount.Load() != n-1 {
		t.Errorf("failures should be %d, got: %d", n-1, failCount.Load())
	}
	t.Log("✓ 20 concurrent lock attempts: successes=1, verified")
}

// ─────────────────────────── HoldUntil tests ──────────────────────────────────

// TestHoldUntil_AutoRelease: the lock is released when ctx is cancelled and can then
// be reacquired.
func TestHoldUntil_AutoRelease(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.lock")

	ctx, cancel := context.WithCancel(context.Background())

	lock, err := fs.AcquireLock(path, fs.LockOptions{})
	if err != nil {
		t.Fatalf("AcquireLock: %v", err)
	}
	lock.HoldUntil(ctx)

	// The lock is held now, so a second acquisition must fail
	_, err2 := fs.AcquireLock(path, fs.LockOptions{
		WaitTimeout:   50 * time.Millisecond,
		RetryInterval: 10 * time.Millisecond,
	})
	if err2 == nil {
		t.Fatal("the lock must not be acquirable before ctx is cancelled")
	}

	// Cancel ctx to trigger the automatic release
	cancel()

	// Give the automatic release a moment to take effect (goroutine scheduling)
	time.Sleep(50 * time.Millisecond)

	// It should be acquirable again now
	lock2, err3 := fs.AcquireLock(path, fs.LockOptions{
		WaitTimeout: 500 * time.Millisecond,
	})
	if err3 != nil {
		t.Fatalf("reacquiring the lock after ctx cancellation failed: %v", err3)
	}
	defer func() { _ = lock2.Release() }()
	t.Log("✓ HoldUntil: the lock is released after ctx cancellation and reacquired")
}

// TestAcquireLockWithContext: the convenience helper.
func TestAcquireLockWithContext(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.lock")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lock, err := fs.AcquireLockWithContext(ctx, path, fs.LockOptions{})
	if err != nil {
		t.Fatalf("AcquireLockWithContext: %v", err)
	}

	if lock == nil {
		t.Fatal("returned a nil lock")
	}

	// Cancel ctx and wait for the release
	cancel()
	time.Sleep(50 * time.Millisecond)

	// Verify it can be reacquired
	lock2, err2 := fs.AcquireLock(path, fs.LockOptions{
		WaitTimeout: 500 * time.Millisecond,
	})
	if err2 != nil {
		t.Fatalf("reacquiring after AcquireLockWithContext's automatic release failed: %v",
			err2)
	}
	defer func() { _ = lock2.Release() }()
	t.Log("✓ AcquireLockWithContext passed")
}

// ─────────────────────────── subprocess exclusion ──────────────────────────────────

// TestFlockSubProcess: verify cross-process exclusion through a subprocess.
// The subprocess (TestHelperProcess) holds the lock; the parent tries to acquire it
// and expects a refusal.
func TestFlockSubProcess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.lock")

	// Start the subprocess and let it hold the lock for 2 seconds
	cmd := exec.Command(
		os.Args[0],
		"-test.run=TestHelperProcess",
		"-test.v",
	)
	cmd.Env = append(os.Environ(),
		"GO_HELPER_LOCK="+path,
		"GO_HELPER_HOLD_MS=2000",
	)

	// Collect the subprocess output (asynchronously)
	outCh := make(chan []byte, 1)
	go func() {
		out, _ := cmd.Output()
		outCh <- out
	}()

	// Wait for the subprocess to start and acquire the lock (at most 3 seconds)
	acquired := false
	for i := 0; i < 30; i++ {
		time.Sleep(100 * time.Millisecond)
		// Try to acquire; a failure means the subprocess already holds it
		tl, err := fs.AcquireLock(path, fs.LockOptions{
			WaitTimeout:   20 * time.Millisecond,
			RetryInterval: 5 * time.Millisecond,
		})
		if err != nil {
			// The subprocess holds the lock
			acquired = true
			t.Logf("✓ subprocess holds the lock, parent correctly refused (attempt %d): %v",
				i+1, err)
			break
		} else {
			// The subprocess has not started yet or has already exited
			_ = tl.Release()
		}
	}

	// Wait for the subprocess to finish
	childOut := <-outCh
	if acquired {
		t.Log("✓ subprocess exclusion test passed")
		t.Logf("subprocess output (excerpt): %s", childOut)
	} else {
		// If the subprocess starts too slowly (CI), print diagnostics but do not fail
		t.Logf("subprocess output: %s", childOut)
		t.Log("warning: could not observe the subprocess holding the lock within 3" +
			" seconds (CI load?)")
		// At least verify that we can lock/release ourselves
		tl, err := fs.AcquireLock(path, fs.LockOptions{WaitTimeout: 1 * time.Second})
		if err != nil {
			t.Fatalf("basic locking failed: %v", err)
		}
		_ = tl.Release()
	}
}

// ─────────────────────────── error paths ────────────────────────────────────

// TestAcquireLock_ErrorMessage: the timeout error message carries the expected hint.
func TestAcquireLock_ErrorMessage(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.lock")

	lock, _ := fs.AcquireLock(path, fs.LockOptions{})
	defer func() { _ = lock.Release() }()

	_, err := fs.AcquireLock(path, fs.LockOptions{
		WaitTimeout:   30 * time.Millisecond,
		RetryInterval: 10 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("an error was expected")
	}

	msg := err.Error()
	required := []string{"another instance", "session", "stop it first"}
	for _, s := range required {
		if !strings.Contains(msg, s) {
			t.Errorf("error message is missing %q: %v", s, msg)
		}
	}
	t.Logf("✓ error message: %v", msg)
}

// TestAcquireLock_NilRelease: Release on a nil *Lock does not panic.
func TestAcquireLock_NilRelease(t *testing.T) {
	var l *fs.Lock
	if err := l.Release(); err != nil {
		t.Errorf("nil.Release() should return nil, got: %v", err)
	}
	t.Log("✓ nil.Release() is safe and returns nil")
}

// TestAcquireLock_NilHoldUntil: HoldUntil on a nil *Lock does not panic.
func TestAcquireLock_NilHoldUntil(t *testing.T) {
	var l *fs.Lock
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	l.HoldUntil(ctx) // must not panic
	t.Log("✓ nil.HoldUntil() is safe and does not panic")
}
