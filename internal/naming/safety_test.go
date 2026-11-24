package naming_test

// safety_test.go - security edge cases (race-safe).
// Concentrates on the acceptance gaps and review points: stripping every control
// character, the validity of UTF-8 truncation, trailing spaces in Windows reserved
// stems, additional SafeJoin traversal vectors (pure backslash, deep-ancestor
// symlinks, the root itself being a symlink, a nonexistent intermediate directory
// below the root), and proof that UniquePath does no Stat pre-check and does not
// overwrite existing files.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/1368129224/tgshelf/internal/naming"
)

// TestSanitize_AllControlBytes every C0 control character 0x00-0x1F and 0x7F (DEL)
// must be stripped, and the result must not contain a 0x00 byte (full version of
// acceptance item 3).
func TestSanitize_AllControlBytes(t *testing.T) {
	var in []byte
	for b := 0; b <= 0x1F; b++ {
		in = append(in, byte(b))
	}
	in = append(in, 0x7F, 'x')
	got := naming.Sanitize(string(in))
	if got != "x" {
		t.Errorf("Sanitize(all control characters+x) = %q, want \"x\"", got)
	}
	if strings.ContainsRune(got, 0x00) {
		t.Error("the result contains a NUL byte")
	}
	for _, r := range got {
		if r < 0x20 || r == 0x7F {
			t.Errorf("a control character remains in the result: %q", r)
		}
	}
}

// TestSanitize_BytesLimitValidUTF8 a 300-byte Chinese name is truncated to <= 200
// bytes, utf8.ValidString holds, and no multi-byte character is cut (acceptance
// item 4).
func TestSanitize_BytesLimitValidUTF8(t *testing.T) {
	name := strings.Repeat("中", 100) // 300 bytes
	got := naming.Sanitize(name)
	if len(got) > 200 {
		t.Errorf("truncated length %d exceeds 200 bytes", len(got))
	}
	if !utf8.ValidString(got) {
		t.Errorf("truncated result is not valid UTF-8: %q", got)
	}
	if !strings.HasPrefix(name, got) {
		t.Error("the truncated result must be a prefix of the input")
	}
}

// TestSanitize_ExactLimitValidUTF8 at exactly the limit (200 bytes) nothing is
// truncated and the result is valid UTF-8.
func TestSanitize_ExactLimitValidUTF8(t *testing.T) {
	// 66 three-byte CJK characters + 1 two-byte character = 200 bytes, exactly the limit
	name := strings.Repeat("中", 66) + "é" // 198 + 2 = 200
	got := naming.Sanitize(name)
	if got != name {
		t.Errorf("exactly 200 bytes must not be truncated: got %d bytes, want %d",
			len(got), len(name))
	}
	if !utf8.ValidString(got) {
		t.Errorf("result is not valid UTF-8: %q", got)
	}
}

// TestSanitize_TrailingSpaceReservedStem Windows ignores trailing spaces in the stem
// (before the first '.') of a reserved name, so "CON .txt" must be prefixed just
// like "CON.txt".
func TestSanitize_TrailingSpaceReservedStem(t *testing.T) {
	for _, in := range []string{"CON .txt", "COM1 .log", "NUL  .bin"} {
		if got := naming.Sanitize(in); !strings.HasPrefix(got, "_") {
			t.Errorf("Sanitize(%q) = %q, want a prefix (reserved name with a trailing space)",
				in, got)
		}
	}
}

// TestSafeJoin_BackslashNoDotdot a pure backslash separator (no ..) is rejected too:
// Windows separators are rejected outright, leaving no room for variants beyond
// "..\..\x".
// TestSafeJoin_RelativeRootExisting regression: a relative root that **exists** must
// pass.
//
// Cause of the defect: EvalSymlinks returns a relative result for relative input
// ("downloads"); asserting it as the prefix of an absolute path misjudged any
// relatively configured root as an escape. After the default changed from
// "/downloads" to the relative "./downloads", that made every download fail.
func TestSafeJoin_RelativeRootExisting(t *testing.T) {
	dir := t.TempDir()
	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	// The directory really exists - exactly the condition that triggered the old defect.
	if err := os.MkdirAll("downloads/video", 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		realDir = dir
	}

	for _, root := range []string{"./downloads", "downloads", "./downloads/"} {
		got, err := naming.SafeJoin(root, "video/a.mp4")
		if err != nil {
			t.Fatalf("root=%q should pass, but errored: %v", root, err)
		}
		if !filepath.IsAbs(got) {
			t.Fatalf("root=%q should return an absolute path, got %q", root, got)
		}
		want := filepath.Join(realDir, "downloads", "video", "a.mp4")
		if got != want {
			t.Fatalf("root=%q returned %q, want %q", root, got, want)
		}
	}

	// Traversal under a relative root must still be rejected.
	if _, err := naming.SafeJoin("./downloads", "../evil.mp4"); !errors.Is(err, naming.ErrOutsideRoot) {
		t.Fatalf("../ under a relative root should be rejected, got err=%v", err)
	}
}

func TestSafeJoin_BackslashNoDotdot(t *testing.T) {
	root := t.TempDir()
	if _, err := naming.SafeJoin(root, "sub\\file.txt"); !errors.Is(err, naming.ErrOutsideRoot) {
		t.Errorf("a pure backslash separator should be rejected (ErrOutsideRoot), got %v", err)
	}
}

// TestSafeJoin_DeepAncestorSymlinkEscape an intermediate symlink below several levels
// of existing directories points outside the root - deeper than the single-level
// case, exercising checkAncestorSymlinks walking up level by level.
func TestSafeJoin_DeepAncestorSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "a", "b", "jump")); err != nil {
		t.Fatal(err)
	}
	_, err := naming.SafeJoin(root, "a/b/jump/file.txt")
	if !errors.Is(err, naming.ErrOutsideRoot) {
		t.Errorf("a deep-ancestor symlink escape should be rejected (ErrOutsideRoot), got %v",
			err)
	}
}

// TestSafeJoin_DanglingSymlinkEscape a dangling symlink pointing to a nonexistent
// path outside root must be rejected rather than allowed through.
func TestSafeJoin_DanglingSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outsideTarget := filepath.Join(t.TempDir(), "nonexistent")
	if err := os.Symlink(outsideTarget, filepath.Join(root, "evil")); err != nil {
		t.Fatal(err)
	}
	if _, err := naming.SafeJoin(root, "evil"); !errors.Is(err, naming.ErrOutsideRoot) {
		t.Errorf("dangling symlink target should be rejected (ErrOutsideRoot), got %v", err)
	}
	if _, err := naming.SafeJoin(root, "evil/sub/file.txt"); !errors.Is(err, naming.ErrOutsideRoot) {
		t.Errorf("path through dangling symlink should be rejected (ErrOutsideRoot), got %v", err)
	}
}

// TestSafeJoin_MissingAncestorBelowRoot a nonexistent intermediate directory below
// the root must not be reported:
// EvalSymlinks only checks existing ancestors, and an uncreated directory has no
// link target to escape through.
func TestSafeJoin_MissingAncestorBelowRoot(t *testing.T) {
	root := t.TempDir()
	final, err := naming.SafeJoin(root, "no/such/deep/file.txt")
	if err != nil {
		t.Fatalf("a nonexistent intermediate directory below the root should pass: %v", err)
	}
	if final != filepath.Join(root, "no/such/deep/file.txt") {
		t.Errorf("got %q", final)
	}
}

// TestSafeJoin_RootIsSymlink the root itself is a symlink: the resolved real path is
// the prefix baseline, and a legal rel passes and lands inside the real root (not the
// link path).
func TestSafeJoin_RootIsSymlink(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "rootlink")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	final, err := naming.SafeJoin(link, "file.txt")
	if err != nil {
		t.Fatalf("a symlinked root should pass: %v", err)
	}
	if final != filepath.Join(real, "file.txt") {
		t.Errorf("got %q, want a path based on the real root %q",
			final, filepath.Join(real, "file.txt"))
	}
}

// TestUniquePath_ExistingFileNotClobbered proves there is no Stat pre-check (O_EXCL
// asks the kernel directly): an existing file of the same name is kept intact and the
// reservation lands on the " (1)" candidate.
func TestUniquePath_ExistingFileNotClobbered(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "report.pdf")
	if err := os.WriteFile(existing, []byte("ORIGINAL"), 0o600); err != nil {
		t.Fatal(err)
	}
	p, f, err := naming.UniquePath(dir, "report", ".pdf")
	if err != nil {
		t.Fatalf("UniquePath: %v", err)
	}
	_ = f.Close()
	if p == existing {
		t.Fatal("UniquePath returned an existing path (it would overwrite!)")
	}
	data, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "ORIGINAL" {
		t.Errorf("the existing file was damaged: %q", data)
	}
}

// TestEndToEnd_RenderedFileNameRejectedBySafeJoin end to end (acceptance item 7):
// a ../ fragment rendered from {{ .FileName }} and passed unsanitised into SafeJoin
// must be rejected.
func TestEndToEnd_RenderedFileNameRejectedBySafeJoin(t *testing.T) {
	root := t.TempDir()
	f := naming.Fields{DialogID: 1, MessageID: 2, FileName: "../sneak.sh"}
	rendered, err := naming.Render("{{ .FileName }}", f)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	_, err = naming.SafeJoin(root, rendered)
	if !errors.Is(err, naming.ErrOutsideRoot) {
		t.Errorf("the unsanitised rendering %q should be rejected by SafeJoin"+
			" (ErrOutsideRoot), got %v", rendered, err)
	}
}
