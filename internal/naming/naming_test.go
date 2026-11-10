package naming_test

// naming_test.go - unit tests for internal/naming (race-safe).
// Coverage: normal template rendering, errors for unknown fields/functions, the
// whitelisted functions, each Sanitize rule, the 6 SafeJoin traversal vectors,
// UniquePath cooperation, and end to end (../../evil.sh).

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/1368129224/tgshelf/internal/naming"
)

func sampleFields() naming.Fields {
	return naming.Fields{
		DialogID:     -1001234567890,
		MessageID:    42,
		MessageDate:  time.Date(2025, 6, 1, 12, 30, 0, 0, time.UTC),
		FileName:     "My Report v1.pdf",
		FileCaption:  "hello <b>world</b>",
		FileSize:     1536,
		DownloadDate: time.Date(2025, 6, 2, 8, 0, 0, 0, time.UTC),
	}
}

// ─────────────────────────── Render ─────────────────────────────────────────

func TestRender_Basic(t *testing.T) {
	got, err := naming.Render("{{ .FileName }}", sampleFields())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "My Report v1.pdf" {
		t.Errorf("got %q", got)
	}
}

func TestRender_AllFields(t *testing.T) {
	tpl := "{{ .DialogID }}-{{ .MessageID }}-{{ .FileName }}-{{ .FileSize }}"
	got, err := naming.Render(tpl, sampleFields())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	want := "-1001234567890-42-My Report v1.pdf-1536"
	if got != want {
		t.Errorf("got %q want %q", got, want)
	}
}

func TestRender_TimeFields(t *testing.T) {
	got, err := naming.Render("{{ date .MessageDate \"2006-01-02\" }}", sampleFields())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "2025-06-01" {
		t.Errorf("got %q", got)
	}
}

func TestRender_UnknownField(t *testing.T) {
	_, err := naming.Render("{{ .NoSuchField }}", sampleFields())
	if err == nil {
		t.Fatal("an unknown field must error (missingkey=error)")
	}
	t.Logf("unknown field error: %v", err)
}

func TestRender_UnknownFunc(t *testing.T) {
	_, err := naming.Render("{{ nosuchfunc .FileName }}", sampleFields())
	if err == nil {
		t.Fatal("an unknown function must error (whitelist)")
	}
	t.Logf("unknown function error: %v", err)
}

func TestRender_EmptyResult(t *testing.T) {
	for _, tpl := range []string{"", "   ", "{{ if false }}x{{ end }}"} {
		_, err := naming.Render(tpl, sampleFields())
		if err == nil {
			t.Errorf("empty template %q must error", tpl)
		} else if !strings.Contains(err.Error(), "empty") {
			t.Errorf("the error should say the result is empty: %v", err)
		}
	}
}

func TestRender_EmptyFieldStillEmptyResult(t *testing.T) {
	// The field exists but is empty -> the rendering is empty -> that must error too
	_, err := naming.Render("{{ .FileCaption }}", naming.Fields{})
	if err == nil {
		t.Fatal("an empty result must error")
	}
}

func TestRender_BadTemplateSyntax(t *testing.T) {
	_, err := naming.Render("{{ .FileName", sampleFields())
	if err == nil {
		t.Fatal("a syntax error must error")
	}
}

// ─────────────────────────── whitelisted functions ─────────────────────────────────────

func TestRender_WhitelistFuncs(t *testing.T) {
	cases := []struct {
		tpl, want string
	}{
		{"{{ lower .FileName }}", "my report v1.pdf"},
		{"{{ upper .FileName }}", "MY REPORT V1.PDF"},
		{"{{ replace .FileName \" \" \"_\" }}", "My_Report_v1.pdf"},
		{"{{ truncate .FileName 5 }}", "My Re"},
		{"{{ size .FileSize }}", "1.50 KiB"},
		{"{{ filenamify .FileCaption }}", "hello bworldb"}, // <> is stripped by filenamify
	}
	for _, c := range cases {
		got, err := naming.Render(c.tpl, sampleFields())
		if err != nil {
			t.Errorf("%s: %v", c.tpl, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s = %q, want %q", c.tpl, got, c.want)
		}
	}
}

func TestRender_FilenamifySanitizes(t *testing.T) {
	f := naming.Fields{FileName: "../../evil.sh"}
	got, err := naming.Render("{{ filenamify .FileName }}", f)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	// After ../ is stripped only evil.sh remains; no leading/trailing dots are left
	if got != "evil.sh" {
		t.Errorf("filenamify(../../evil.sh) = %q, want evil.sh", got)
	}
}

func TestRender_NowFunc(t *testing.T) {
	got, err := naming.Render("{{ date now \"2006\" }}", sampleFields())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if len(got) != 4 {
		t.Errorf("the year printed by now should be 4 characters: %q", got)
	}
}

func TestRender_TruncateNegative(t *testing.T) {
	got, err := naming.Render("{{ truncate .FileName -1 }}x", sampleFields())
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if got != "x" {
		t.Errorf("truncate with a negative n should return an empty string: %q", got)
	}
}

// ─────────────────────────── Sanitize ───────────────────────────────────────

func TestSanitize_ForbiddenChars(t *testing.T) {
	got := naming.Sanitize("a/b\\c:d*e?f\"g<h>i|j")
	if got != "abcdefghij" {
		t.Errorf("got %q", got)
	}
}

func TestSanitize_ControlCharsAndBOM(t *testing.T) {
	got := naming.Sanitize("a\x00b\x01c\td\n.\x7F\uFEFF")

	if got != "abcd" {
		t.Errorf("got %q, want abcd", got)
	}
}

func TestSanitize_DotsAndSpaces(t *testing.T) {
	if got := naming.Sanitize("..hidden"); got != "hidden" {
		t.Errorf("leading dots should be removed: %q", got)
	}
	if got := naming.Sanitize("name..."); got != "name" {
		t.Errorf("trailing dots should be removed: %q", got)
	}
	if got := naming.Sanitize("  padded  "); got != "padded" {
		t.Errorf("leading/trailing whitespace should be trimmed: %q", got)
	}
}

func TestSanitize_CollapseSpaces(t *testing.T) {
	if got := naming.Sanitize("a    b"); got != "a b" {
		t.Errorf("runs of whitespace should be collapsed: %q", got)
	}
}

func TestSanitize_BytesLimitRuneBoundary(t *testing.T) {
	// 100 Chinese characters = 300 bytes > 200. Truncating to 200 bytes would split a
	// multi-byte character, so it must back off to a rune boundary: 200 = 66*3 + 2 ->
	// back off to 66 characters = 198 bytes.
	name := strings.Repeat("好", 100)
	got := naming.Sanitize(name)
	if len(got) != 198 {
		t.Errorf("length = %d, want 198 (66 three-byte CJK characters)", len(got))
	}
	if !strings.HasPrefix(name, got) {
		t.Error("the truncated result must be a prefix of the input")
	}
	for _, r := range got {
		if r != '好' {
			t.Errorf("found a non-Chinese character %q", r)
			break
		}
	}
}

func TestSanitize_BytesLimitExactBoundary(t *testing.T) {
	// Exactly 200 bytes (no truncation)
	name := strings.Repeat("a", 200)
	if got := naming.Sanitize(name); len(got) != 200 {
		t.Errorf("exactly 200 bytes must not be truncated: %d", len(got))
	}
	// 201 bytes of ASCII
	name2 := strings.Repeat("a", 201)
	if got := naming.Sanitize(name2); len(got) != 200 {
		t.Errorf("201 ASCII bytes should truncate to 200: %d", len(got))
	}
}

func TestSanitize_BytesLimitEmojiBoundary(t *testing.T) {
	// 4-byte emoji: 197*1 + 4 = 201 bytes -> truncating to 200 would split the emoji,
	// so it backs off to 197 bytes
	name := strings.Repeat("a", 197) + "😀"
	got := naming.Sanitize(name)
	if len(got) != 197 {
		t.Errorf("length = %d, want 197", len(got))
	}
	if strings.Contains(got, "�") {
		t.Error("must not contain a replacement character (a split multi-byte char)")
	}
}

func TestSanitize_WindowsReserved(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"CON", "_CON"},
		{"con", "_con"},
		{"CON.txt", "_CON.txt"},
		{"com1", "_com1"},
		{"LPT9.log", "_LPT9.log"},
		{"NUL.bin", "_NUL.bin"},
		{"aux", "_aux"},
		{"prn.dat", "_prn.dat"},
	} {
		if got := naming.Sanitize(c.in); got != c.want {
			t.Errorf("Sanitize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// Non-reserved names are unaffected
	for _, in := range []string{"console.txt", "control.log", "condition"} {
		if got := naming.Sanitize(in); strings.HasPrefix(got, "_") {
			t.Errorf("non-reserved name %q must not be prefixed: %q", in, got)
		}
	}
}

func TestSanitize_Empty(t *testing.T) {
	for _, in := range []string{"", "///", " * ? ", "\x00\x01\x02"} {
		if got := naming.Sanitize(in); got != "_" {
			t.Errorf("Sanitize(%q) = %q, want _", in, got)
		}
	}
}

func TestSanitize_TraversalNeutralized(t *testing.T) {
	// After Sanitize, "/" or ".." cannot remain
	for _, in := range []string{"../../etc/passwd", "..\\..\\x", "/etc/passwd"} {
		got := naming.Sanitize(in)
		if strings.Contains(got, "/") || strings.Contains(got, "..") || strings.HasPrefix(got, ".") {
			t.Errorf("Sanitize(%q) = %q still allows traversal", in, got)
		}
		t.Logf("Sanitize(%q) = %q", in, got)
	}
}

// ─────────────────────────── SafeJoin: the 6 traversal vectors ─────────────────────────

func TestSafeJoin_SixAttackVectors(t *testing.T) {
	root := t.TempDir()

	cases := []struct {
		name string
		rel  string
	}{
		{"dotdot-absolute", "../../etc/passwd"},
		{"absolute-unix", "/etc/passwd"},
		{"dotdot-mixed", "a/../../b"},
		{"nul", "file\x00name"},
		{"windows-backslash", "..\\..\\x"},
		{"symlink-escape", ""},
	}

	// Category 6: symlink escape - root/link -> /tmp (outside the root)
	outside := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	for _, c := range cases {
		rel := c.rel
		label := c.name
		if label == "symlink-escape" {
			rel = "link/escaped.txt"
		}
		final, err := naming.SafeJoin(root, rel)
		if err == nil {
			// Even when allowed it must stay strictly inside the root
			if !strings.HasPrefix(final, root+string(filepath.Separator)) {
				t.Errorf("[%s] %q was allowed as %q (outside the root!)", label, rel, final)
			} else {
				t.Errorf("[%s] %q should be rejected, got %q", label, rel, final)
			}
			continue
		}
		if !errors.Is(err, naming.ErrOutsideRoot) {
			t.Errorf("[%s] the error should be ErrOutsideRoot: %v", label, err)
		}
		t.Logf("[%s] %q -> rejected: %v", label, rel, err)
	}
}

func TestSafeJoin_Legit(t *testing.T) {
	root := t.TempDir()
	final, err := naming.SafeJoin(root, "sub/dir/file.mp4")
	if err != nil {
		t.Fatalf("a legal relative path should pass: %v", err)
	}
	if final != filepath.Join(root, "sub/dir/file.mp4") {
		t.Errorf("got %q", final)
	}
	// rel == "." (equal to the root)
	final2, err := naming.SafeJoin(root, ".")
	if err != nil {
		t.Fatalf("rel=. should pass: %v", err)
	}
	if final2 != root {
		t.Errorf("got %q want %q", final2, root)
	}
	// rel contains an internal .. but still normalises inside the root
	final3, err := naming.SafeJoin(root, "a/../file.txt")
	if err != nil {
		t.Fatalf("a path that normalises inside the root should pass: %v", err)
	}
	if final3 != filepath.Join(root, "file.txt") {
		t.Errorf("got %q", final3)
	}

}
func TestSafeJoin_SymlinkInsideRoot(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	if err := os.Mkdir(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	final, err := naming.SafeJoin(root, "alias/x.txt")
	if err != nil {
		t.Fatalf("a symlink inside the root should pass: %v", err)
	}
	if final != filepath.Join(root, "alias/x.txt") {
		t.Errorf("got %q", final)
	}
}

func TestSafeJoin_RootNotExist(t *testing.T) {
	base := t.TempDir()
	missing := filepath.Join(base, "no/such/root")
	final, err := naming.SafeJoin(missing, "file.txt")
	if err != nil {
		t.Fatalf("a nonexistent root should fall back to Abs and pass: %v", err)
	}
	if final != filepath.Join(missing, "file.txt") {
		t.Errorf("got %q", final)
	}
}

func TestSafeJoin_EmptyRoot(t *testing.T) {
	if _, err := naming.SafeJoin("", "x"); err == nil {
		t.Error("an empty root should error")
	}
}

func TestSafeJoin_RootNul(t *testing.T) {
	if _, err := naming.SafeJoin("ro\x00ot", "x"); err == nil {
		t.Error("a root containing NUL should error")
	}
}

func TestSafeJoin_DriveLetter(t *testing.T) {
	root := t.TempDir()
	if _, err := naming.SafeJoin(root, "C:\\Windows\\evil"); err == nil {
		t.Error("a Windows volume name should be rejected")
	}
}

// ─────────────────────────── UniquePath ─────────────────────────────────────

func TestUniquePath_BasicAndSequence(t *testing.T) {
	dir := t.TempDir()
	p1, f1, err := naming.UniquePath(dir, "report", ".pdf")
	if err != nil {
		t.Fatalf("UniquePath: %v", err)
	}
	_ = f1.Close()
	if filepath.Base(p1) != "report.pdf" {
		t.Errorf("p1 = %q", p1)
	}
	p2, f2, err := naming.UniquePath(dir, "report", ".pdf")
	if err != nil {
		t.Fatalf("UniquePath: %v", err)
	}
	_ = f2.Close()
	if filepath.Base(p2) != "report (1).pdf" {
		t.Errorf("p2 = %q", p2)
	}
}

func TestUniquePath_Concurrent(t *testing.T) {
	dir := t.TempDir()
	const n = 20
	paths := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			p, f, err := naming.UniquePath(dir, "dl", ".mp4")
			if err != nil {
				t.Errorf("goroutine %d: %v", idx, err)
				return
			}
			_ = f.Close()
			paths[idx] = p
		}(i)
	}
	wg.Wait()
	seen := map[string]bool{}
	for _, p := range paths {
		if seen[p] {
			t.Fatalf("duplicate path %q", p)
		}
		seen[p] = true
	}
}

// ─────────────────────────── end to end: ../../evil.sh ─────────────────────────

// TestEndToEnd_EvilFileName the full pipeline (real temp directory):
// when {{ .FileName }} renders "../../evil.sh", the final file must land inside the
// root or be rejected.
func TestEndToEnd_EvilFileName(t *testing.T) {
	root := t.TempDir()

	// 1. Render (the template result may contain a traversal fragment - the typical
	//    path traversal shape)
	f := naming.Fields{DialogID: 1, MessageID: 2, FileName: "../../evil.sh"}
	rendered, err := naming.Render("{{ .FileName }}", f)
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if rendered != "../../evil.sh" {
		t.Fatalf("the rendering should be ../../evil.sh, got %q", rendered)
	}

	// 2. Sanitise: the traversal fragment is stripped
	safe := naming.Sanitize(rendered)
	if safe != "evil.sh" {
		t.Errorf("Sanitize(../../evil.sh) = %q, want evil.sh", safe)
	}

	// 3. SafeJoin: the sanitised result must land inside the root
	final, err := naming.SafeJoin(root, safe)
	if err != nil {
		t.Fatalf("SafeJoin: %v", err)
	}
	if !strings.HasPrefix(final, root+string(filepath.Separator)) {
		t.Fatalf("final %q is not inside root %q", final, root)
	}

	// 4. Atomic name reservation + write
	path, file, err := naming.UniquePath(root, safe, "")
	if err != nil {
		t.Fatalf("UniquePath: %v", err)
	}
	if _, err := file.WriteString("data"); err != nil {
		_ = file.Close()
		t.Fatalf("write: %v", err)
	}
	_ = file.Close()

	// Assertion A: the write path is inside the root
	if !strings.HasPrefix(path, root+string(filepath.Separator)) {
		t.Fatalf("write path %q is not inside root %q", path, root)
	}
	// Assertion B: no file was created outside the root
	outside, err := filepath.EvalSymlinks(filepath.Join(root, ".."))
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "evil") && filepath.Join(outside, e.Name()) != root {
			t.Fatalf("a file escaped outside the root: %q", e.Name())
		}
	}
	t.Logf("✓ end to end: ../../evil.sh -> Sanitize=%q -> lands inside root as %q",
		safe, path)
}

// TestEndToEnd_RawRenderMustNotBeUsedDirectly proves that an unsanitised rendering
// is rejected by SafeJoin - the evidence that path traversal is blocked.
func TestEndToEnd_RawRenderMustNotBeUsedDirectly(t *testing.T) {
	root := t.TempDir()
	for _, rel := range []string{"../../evil.sh", "/etc/passwd", "a/../../b"} {
		_, err := naming.SafeJoin(root, rel)
		if err == nil {
			t.Fatalf("the unsanitised rendering %q must not pass", rel)
		}
		if !errors.Is(err, naming.ErrOutsideRoot) {
			t.Errorf("expected ErrOutsideRoot: %v", err)
		}
	}
}
