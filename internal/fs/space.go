package fs

import (
	"errors"
	"fmt"
	"math"
	"os"

	"golang.org/x/sys/unix"
)

// CheckSpace checks whether the filesystem holding dir has available space for 1.2
// times the need bytes.
//
// Available space = Bavail * Bsize (blocks available to unprivileged processes x
// block size). It requires available >= need * 1.2 and otherwise returns an error
// with human-readable units.
//
// Special cases:
//   - need <= 0: return nil immediately (nothing to check).
//   - need is huge (close to math.MaxInt64): compute in floating point to avoid integer
//     overflow.
func CheckSpace(dir string, need int64) error {
	if need <= 0 {
		return nil
	}

	var st unix.Statfs_t
	if err := unix.Statfs(dir, &st); err != nil {
		return fmt.Errorf("fs.CheckSpace: statfs %q: %w", dir, err)
	}

	// Compute with float64 to avoid int64 overflow in Bavail*Bsize or need*1.2
	avail := float64(st.Bavail) * float64(st.Bsize)
	required := float64(need) * 1.2

	if avail < required {
		return fmt.Errorf(
			"fs.CheckSpace: not enough disk space (need %s / available %s)",
			HumanBytes(need),
			HumanBytes(int64(avail)),
		)
	}
	return nil
}

// HumanBytes formats a byte count as a human-readable string (KiB/MiB/GiB).
// For n < 0 it formats the absolute value and prefixes a minus sign.
func HumanBytes(n int64) string {
	const (
		kib = 1024
		mib = 1024 * kib
		gib = 1024 * mib
	)

	neg := ""
	v := n
	if v < 0 {
		neg = "-"
		// Handle the math.MinInt64 overflow: convert to float64 first
		if v == math.MinInt64 {
			return fmt.Sprintf("-%.2f GiB", float64(math.MaxInt64)/gib+1)
		}
		v = -v
	}

	switch {
	case v >= gib:
		return fmt.Sprintf("%s%.2f GiB", neg, float64(v)/gib)
	case v >= mib:
		return fmt.Sprintf("%s%.2f MiB", neg, float64(v)/mib)
	case v >= kib:
		return fmt.Sprintf("%s%.2f KiB", neg, float64(v)/kib)
	default:
		return fmt.Sprintf("%s%d B", neg, v)
	}
}

// EnsureDir idempotently creates the directory path (including all parent directories)
// and sets the permission with an explicit Chmod.
//
// If path already exists but is not a directory, it returns a clear error.
func EnsureDir(path string, perm os.FileMode) error {
	if err := os.MkdirAll(path, perm); err != nil {
		return fmt.Errorf("fs.EnsureDir: mkdir %q: %w", path, err)
	}
	// Verify the type of an already existing directory
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("fs.EnsureDir: stat %q: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("fs.EnsureDir: %q exists but is not a directory", path)
	}
	// Explicit Chmod so umask cannot shrink the intended permission
	if err = os.Chmod(path, perm); err != nil {
		return fmt.Errorf("fs.EnsureDir: chmod %q: %w", path, err)
	}
	return nil
}

// Exists checks whether the path exists (either a file or a directory).
//
// Note: this function has an inherent TOCTOU race and is only for non-security-critical
// uses (e.g. logging/debugging).
// Use O_EXCL when an atomicity guarantee is needed (see CreateExclusive).
func Exists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, fmt.Errorf("fs.Exists: stat %q: %w", path, err)
}
