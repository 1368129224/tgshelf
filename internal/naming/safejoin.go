package naming

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrOutsideRoot means the joined result escapes the download root.
// It is a sentinel error, so callers can test it with errors.Is.
var ErrOutsideRoot = errors.New("naming: path escapes download root")

// SafeJoin safely joins the relative fragment rel under root and returns an absolute
// path.
// Any form of traversal (../, absolute path, NUL, Windows-style separator, symlink
// escape) is rejected.
//
// Steps:
//  1. rel/root contains a NUL byte -> reject (C APIs silently truncate at NUL, a
//     classic bypass).
//  2. rel is an absolute path (starts with /, or a Windows volume name like C:) ->
//     reject.
//  3. rel contains a Windows separator (e.g. ..\..\x) -> reject.
//  4. Resolve the real root: EvalSymlinks(root); when root does not exist it falls
//     back to Abs(root) (EvalSymlinks cannot resolve a nonexistent path; in that
//     case there is no target to escape through, and the joined result still has to
//     pass the prefix assertion in step 6).
//  5. final = Join(rootReal, rel) -> Abs -> Clean.
//  6. Assert final == rootReal or a rootReal+separator prefix, otherwise reject.
//  7. EvalSymlinks the longest existing ancestor of final: if it resolves outside
//     rootReal -> reject (blocking an intermediate directory that is a symlink
//     outside the root).
func SafeJoin(root, rel string) (string, error) {
	// 1. NUL check
	if strings.IndexByte(rel, 0) >= 0 {
		return "", fmt.Errorf("%w: rel contains a NUL byte: %q", ErrOutsideRoot, rel)
	}
	if strings.IndexByte(root, 0) >= 0 {
		return "", fmt.Errorf("%w: root contains a NUL byte: %q", ErrOutsideRoot, root)
	}

	// 2. Reject absolute paths (including Windows volume names)
	if strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, "\\") || hasWindowsDrivePrefix(rel) {
		return "", fmt.Errorf("%w: rel is an absolute path: %q", ErrOutsideRoot, rel)
	}

	// 3. Reject Windows-style backslashes outright. On Unix, Join treats "\" as an
	// ordinary character ("a\b" becomes a single directory name), which is harmless
	// but a suspicious naming intent; in binaries built for Windows, however, "\" is
	// a real separator and would create a subdirectory. Rejecting both closes the
	// platform gap.
	if strings.Contains(rel, "\\") {
		return "", fmt.Errorf("%w: rel contains a Windows separator: %q", ErrOutsideRoot, rel)
	}

	// 4. Real path of the root
	rootReal, err := evalRoot(root)
	if err != nil {
		return "", err
	}

	// 5. Join + normalise
	final, err := filepath.Abs(filepath.Join(rootReal, rel))
	if err != nil {
		return "", fmt.Errorf("naming.SafeJoin: abs %q: %w", filepath.Join(rootReal, rel), err)
	}
	final = filepath.Clean(final)

	// 6. Prefix assertion
	if !isWithinRoot(rootReal, final) {
		return "", fmt.Errorf("%w: %q is not inside root %q", ErrOutsideRoot, final, rootReal)
	}

	// 7. Symlink escape check on the existing ancestor
	if err := checkAncestorSymlinks(rootReal, final); err != nil {
		return "", err
	}

	return final, nil
}

// evalRoot returns the real (symlink-resolved) absolute path of root.
// When root does not exist it uses Abs: EvalSymlinks cannot resolve a nonexistent
// path; in that case root itself cannot escape anywhere and the prefix assertion
// still applies.
// Note that the absolute path must be resolved before EvalSymlinks: EvalSymlinks
// returns a relative result for relative input ("downloads" rather than
// "/abs/path/downloads"), so using it directly to assert a prefix of a later
// absolute path would judge any relatively configured root as an escape. The defect
// surfaced as real download failures after the default changed from "/downloads" to
// the relative "./downloads".
func evalRoot(root string) (string, error) {
	if root == "" {
		return "", fmt.Errorf("naming.SafeJoin: root is empty")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("naming.SafeJoin: resolve root %q: %w", root, err)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return filepath.Clean(real), nil
	}
	return filepath.Clean(abs), nil
}

// checkAncestorSymlinks EvalSymlinks the longest existing ancestor of final and
// requires it to resolve inside rootReal (or to rootReal itself), otherwise it is a
// symlink escape.
func checkAncestorSymlinks(rootReal, final string) error {
	existing := final
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		parent := filepath.Dir(existing)
		if parent == existing { // reached the filesystem root
			return nil
		}
		existing = parent
		if existing == rootReal || len(existing) < len(rootReal) {
			break
		}
	}

	// Now at rootReal itself: rootReal was already resolved in evalRoot, so there is
	// no need to check again. (When root does not exist evalRoot falls back to Abs, so
	// let it through here; the prefix assertion already covers it.)
	if existing == rootReal {
		return nil
	}

	resolved, err := filepath.EvalSymlinks(existing)
	if err != nil {
		if os.IsNotExist(err) {
			// Distinguish a vanished path from a dangling symlink: a dangling symlink
			// still exists according to Lstat, and creating a file through it would
			// follow the link target outside rootReal.
			if info, lerr := os.Lstat(existing); lerr == nil && info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("%w: %q is a dangling symlink", ErrOutsideRoot, existing)
			}
			return nil // vanished in a race with lstat: no link to escape through
		}
		return fmt.Errorf("naming.SafeJoin: resolve %q: %w", existing, err)
	}
	resolved = filepath.Clean(resolved)
	if !isWithinRoot(rootReal, resolved) {
		return fmt.Errorf("%w: %q resolves through a symlink to %q, outside root %q",
			ErrOutsideRoot, existing, resolved, rootReal)
	}
	return nil
}

// isWithinRoot reports whether candidate is equal to rootReal or strictly inside it.
// It handles filesystem roots ("/" or "C:\") that already end with a path separator.
func isWithinRoot(rootReal, candidate string) bool {
	if candidate == rootReal {
		return true
	}
	sep := string(filepath.Separator)
	if strings.HasSuffix(rootReal, sep) {
		return strings.HasPrefix(candidate, rootReal)
	}
	return strings.HasPrefix(candidate, rootReal+sep)
}

// hasWindowsDrivePrefix reports whether the path starts with a Windows volume name
// (such as "C:" / "C:\").
func hasWindowsDrivePrefix(p string) bool {
	if len(p) < 2 {
		return false
	}
	c := p[0]
	if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
		return false
	}
	return p[1] == ':'
}
