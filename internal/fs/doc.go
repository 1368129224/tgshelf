// Package fs provides filesystem helpers: atomic writes, atomic name reservation via
// O_EXCL, disk space checks and directory creation.
//
// Design constraints:
//   - This package does not depend on any Telegram client library and keeps a pure
//     filesystem responsibility.
//   - Disk space checks use golang.org/x/sys/unix.Statfs (not the syscall package).
//   - The flock single-instance lock is implemented in this package by lock.go
//     (AcquireLock/Lock), using golang.org/x/sys/unix.Flock.
package fs
