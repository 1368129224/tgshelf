package fs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// LockOptions controls the retry behavior of AcquireLock.
type LockOptions struct {
	// WaitTimeout is the maximum time to wait for the lock.
	// The zero value uses the default of 5 seconds.
	WaitTimeout time.Duration

	// RetryInterval is the wait between retries.
	// The zero value uses the default of 150ms.
	RetryInterval time.Duration
}

const (
	defaultWaitTimeout   = 5 * time.Second
	defaultRetryInterval = 150 * time.Millisecond
)

// Lock represents exclusive ownership of a lock file. Create it with AcquireLock and
// release it with Release.
//
// # Compatibility notes
//
// Availability of flock(2) on common filesystems:
//   - ext4, tmpfs, overlayfs (the Docker default storage driver): fully supported,
//     mutual exclusion works normally.
//   - NFS: known pitfall - flock depends on the server-side lockd daemon, and under
//     some NFS mount configurations (the nolock mount option, lockd not running) flock
//     may silently succeed while not actually excluding anyone. If the session file
//     lives on an NFS mount, prefer a liveness check based on a PID file (fallback:
//     write the PID to a file and, at startup, check whether the process recorded in
//     the PID file is still running via kill(pid, 0); that approach has a TOCTOU race
//     and is only a fallback).
//   - Other local filesystems (xfs, btrfs, f2fs): all support flock and mutual
//     exclusion works normally.
//
// # flock semantics and file contents
//
// flock's mutual exclusion is based on the lifetime of the fd (file descriptor) and is
// unrelated to the file contents:
//   - Writing the PID/time into the lock file content only helps manual troubleshooting;
//     it does not affect mutual exclusion semantics.
//   - Deleting the lock file (os.Remove) does not release the lock - the process holding
//     the fd still holds it; another process opening the same path gets a new inode, and
//     flock mutual exclusion still holds (at the inode level).
//   - This implementation therefore uses O_RDWR|O_CREATE (keeping the file) instead of
//     recreating the file every time.
type Lock struct {
	path     string
	mu       sync.Mutex
	file     *os.File
	released bool
}

// AcquireLock takes an exclusive flock on the file at path, preventing multiple
// processes from using the same Telegram session concurrently.
//
// Behavior:
//   - If the parent directory does not exist, it is created with mode 0700.
//   - Once the lock is held, the current PID and start time (RFC3339) are written to
//     the lock file to make identifying the holder easier.
//   - If another holder already locked the file (EWOULDBLOCK/EAGAIN), retry every
//     opts.RetryInterval (default 150ms) until opts.WaitTimeout (default 5s);
//     on timeout a human-readable error is returned.
//
// The returned *Lock must be released with Release() on exit, or released
// automatically by binding it to a context with HoldUntil(ctx).
func AcquireLock(path string, opts LockOptions) (*Lock, error) {
	return acquireLock(context.Background(), path, opts)
}

func acquireLock(ctx context.Context, path string, opts LockOptions) (*Lock, error) {
	if opts.WaitTimeout <= 0 {
		opts.WaitTimeout = defaultWaitTimeout
	}
	if opts.RetryInterval <= 0 {
		opts.RetryInterval = defaultRetryInterval
	}

	// Make sure the parent directory exists
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := EnsureDir(dir, 0o700); err != nil {
			return nil, fmt.Errorf("fs.AcquireLock: create lock dir: %w", err)
		}
	}

	// Open or create the lock file; keep the file to preserve flock semantics (inode
	// stability).
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) // #nosec G304 -- lock path comes from local configuration
	if err != nil {
		return nil, fmt.Errorf("fs.AcquireLock: open lock file %q: %w", path, err)
	}

	deadline := time.Now().Add(opts.WaitTimeout)
	for {
		if err := ctx.Err(); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("fs.AcquireLock: %w", err)
		}
		flockErr := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if flockErr == nil {
			// Lock acquired: write the PID and start time (truncate, then rewrite)
			pid := os.Getpid()
			startTime := time.Now().UTC().Format(time.RFC3339)
			info := fmt.Sprintf("pid=%d started=%s\n", pid, startTime)
			_ = f.Truncate(0)
			_, _ = f.WriteAt([]byte(info), 0)
			return &Lock{path: path, file: f}, nil
		}

		if errors.Is(flockErr, unix.EINTR) {
			continue
		}

		// Check whether this is a transient lock conflict
		if !errors.Is(flockErr, unix.EWOULDBLOCK) {
			_ = f.Close()
			return nil, fmt.Errorf("fs.AcquireLock: flock %q: %w", path, flockErr)
		}

		// The lock is taken; check whether any wait time remains
		if time.Now().After(deadline) {
			_ = f.Close()
			return nil, fmt.Errorf(
				"another instance is already using this session (container started twice?); "+
					"stop it first (lock file: %s)",
				path,
			)
		}

		timer := time.NewTimer(opts.RetryInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = f.Close()
			return nil, fmt.Errorf("fs.AcquireLock: %w", ctx.Err())
		case <-timer.C:
		}
	}
}

// Release releases the lock (Flock LOCK_UN + closing the file). It is idempotent and
// safe for concurrent calls (e.g. defer lock.Release() racing with HoldUntil).
//
// Note: Release does not delete the lock file, because flock semantics are based on
// the fd lifetime rather than the file's existence; other waiting processes retry
// after they observe the release.
func (l *Lock) Release() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return nil
	}
	l.released = true

	// LOCK_UN first, then Close (Close also releases implicitly, but an explicit unlock is
	// clearer)
	_ = unix.Flock(int(l.file.Fd()), unix.LOCK_UN)
	return l.file.Close()
}

// HoldUntil registers a callback on ctx so that Release is called automatically when ctx ends.
//
// Design note: this method does not use os/signal (it does not listen to
// SIGTERM/SIGINT directly); the reasons are:
//  1. Keeping the package testable: not coupling signal handling lets unit tests
//     control the release timing directly, without sending signals;
//  2. Single responsibility: signal handling belongs in main, where
//     signal.NotifyContext sets up the ctx cancellation; the lock package only reacts
//     to ctx cancellation and does not reimplement signal logic;
//  3. Flexibility: callers may pass any cancellation reason (timeout, graceful
//     shutdown, test control) instead of being tied to OS signal semantics.
//
// Usage:
//
//	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
//	defer cancel()
//	lock, _ := fs.AcquireLock(path, opts)
//	lock.HoldUntil(ctx)  // the signal cancels ctx, releasing the lock automatically
func (l *Lock) HoldUntil(ctx context.Context) {
	if l == nil {
		return
	}
	context.AfterFunc(ctx, func() {
		_ = l.Release()
	})
}

// AcquireLockWithContext is a convenience wrapper around AcquireLock: it acquires the
// lock (honoring ctx cancellation while waiting) and binds it to ctx immediately,
// releasing the lock when ctx ends.
//
// It suits a single call in main that ties the whole process lifetime to the lock:
//
//	lock, err := fs.AcquireLockWithContext(ctx, path, fs.LockOptions{})
func AcquireLockWithContext(ctx context.Context, path string, opts LockOptions) (*Lock, error) {
	l, err := acquireLock(ctx, path, opts)
	if err != nil {
		return nil, err
	}
	l.HoldUntil(ctx)
	return l, nil
}
