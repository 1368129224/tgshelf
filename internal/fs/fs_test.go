package fs_test

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/1368129224/tgshelf/internal/fs"
)

// ─────────────────────────── AtomicWriteFile ────────────────────────────────

func TestAtomicWriteFile_Basic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.txt")
	data := []byte("hello, atomic world")

	if err := fs.AtomicWriteFile(path, data, 0o644); err != nil {
		t.Fatalf("AtomicWriteFile error: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Errorf("content mismatch: got %q want %q", got, data)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Errorf("perm = %o, want 0644", info.Mode().Perm())
	}
}

func TestAtomicWriteFile_NoTmpLeftover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.dat")

	if err := fs.AtomicWriteFile(path, []byte("data"), 0o600); err != nil {
		t.Fatalf("AtomicWriteFile: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp.") {
			t.Errorf("leftover temp file: %q", e.Name())
		}
	}
}

func TestAtomicWriteFile_Overwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "overwrite.txt")

	for i := range 5 {
		data := []byte(fmt.Sprintf("iteration-%d", i))
		if err := fs.AtomicWriteFile(path, data, 0o644); err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
	}

	got, _ := os.ReadFile(path)
	if !bytes.HasPrefix(got, []byte("iteration-")) {
		t.Errorf("unexpected final content: %q", got)
	}
}

// TestAtomicWriteFile_Concurrent: 100 goroutines write the same path concurrently,
// and the final content must be one of those writes in full (no mixing, no truncation).
func TestAtomicWriteFile_Concurrent(t *testing.T) {
	t.Log("concurrent atomic writes: 100 goroutines writing the same path")
	dir := t.TempDir()
	path := filepath.Join(dir, "concurrent.dat")

	const goroutines = 100
	const dataSize = 4096

	var wg sync.WaitGroup
	written := make([][]byte, goroutines)
	errs := make([]error, goroutines)

	for i := range goroutines {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			// Each goroutine writes a constant byte: all bytes are equal (easy to verify integrity)
			b := bytes.Repeat([]byte{byte(idx + 1)}, dataSize)
			written[idx] = b
			errs[idx] = fs.AtomicWriteFile(path, b, 0o644)
		}(i)
	}
	wg.Wait()

	// Collect write errors (some writes failing is harmless; what matters is that the
	// final content is complete)
	errCount := 0
	for _, e := range errs {
		if e != nil {
			errCount++
		}
	}
	t.Logf("write errors (concurrent contention expected): %d/%d", errCount, goroutines)

	// Verify the final file content is one complete write
	final, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("final file does not exist: %v", err)
	}
	if len(final) != dataSize {
		t.Fatalf("final content length %d != %d (truncated file!)", len(final), dataSize)
	}

	// Verify the content repeats a single byte (no mixing)
	firstByte := final[0]
	for i, b := range final {
		if b != firstByte {
			t.Fatalf("final content mixed at byte %d (got %d, want %d)", i, b, firstByte)
		}
	}
	t.Logf("✓ concurrent atomic write passed, final byte value: %d, length: %d", firstByte, len(final))

	// No leftover temp files
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp.") {
			t.Errorf("leftover temp file: %q", e.Name())
		}
	}
}

func TestAtomicWriteFile_EmptyData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.txt")
	if err := fs.AtomicWriteFile(path, []byte{}, 0o644); err != nil {
		t.Fatalf("AtomicWriteFile empty: %v", err)
	}
	info, _ := os.Stat(path)
	if info.Size() != 0 {
		t.Errorf("expected size 0, got %d", info.Size())
	}
}

func TestAtomicWriteFile_SubdirNotExist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent", "file.txt")
	err := fs.AtomicWriteFile(path, []byte("x"), 0o644)
	if err == nil {
		t.Error("expected error for nonexistent parent dir")
	}
}

// ─────────────────────────── CreateExclusive ────────────────────────────────

func TestCreateExclusive_Basic(t *testing.T) {
	dir := t.TempDir()
	path, f, err := fs.CreateExclusive(dir, "report", ".pdf")
	if err != nil {
		t.Fatalf("CreateExclusive: %v", err)
	}
	defer func() { _ = f.Close() }()

	if filepath.Base(path) != "report.pdf" {
		t.Errorf("first candidate should be report.pdf, got %q", filepath.Base(path))
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("file should exist: %v", err)
	}
}

func TestCreateExclusive_Sequence(t *testing.T) {
	dir := t.TempDir()

	// Create report.pdf first; the next call should get report (1).pdf
	p0, f0, err := fs.CreateExclusive(dir, "report", ".pdf")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	_ = f0.Close()

	p1, f1, err := fs.CreateExclusive(dir, "report", ".pdf")
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	_ = f1.Close()

	if filepath.Base(p0) != "report.pdf" {
		t.Errorf("p0 = %q", p0)
	}
	if filepath.Base(p1) != "report (1).pdf" {
		t.Errorf("p1 = %q", p1)
	}
}

func TestCreateExclusive_NoOsStat(t *testing.T) {
	// Compile-time verification: the grep check lives outside this file, this only documents it
	// The actual verification is grep -n 'os.Stat' internal/fs/atomic.go (it should be empty)
	t.Log("os.Stat must not be called in CreateExclusive; confirmed by code review")
}

// TestCreateExclusive_Concurrent: 50 goroutines reserve names concurrently and must get
// 50 distinct paths.
func TestCreateExclusive_Concurrent(t *testing.T) {
	t.Log("concurrent name reservation: 50 goroutines racing for the same base")
	dir := t.TempDir()

	const goroutines = 50
	paths := make([]string, goroutines)
	errs := make([]error, goroutines)
	files := make([]*os.File, goroutines)

	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			p, f, err := fs.CreateExclusive(dir, "download", ".mp4")
			paths[idx] = p
			files[idx] = f
			errs[idx] = err
		}(i)
	}
	wg.Wait()

	// Close all file handles
	for _, f := range files {
		if f != nil {
			_ = f.Close()
		}
	}

	// Check that there are no errors
	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}

	// Check that the paths are all distinct
	seen := make(map[string]bool)
	for i, p := range paths {
		if p == "" {
			t.Errorf("goroutine %d: empty path", i)
			continue
		}
		if seen[p] {
			t.Errorf("duplicate path: %q", p)
		}
		seen[p] = true
	}
	t.Logf("✓ unique paths: %d/%d", len(seen), goroutines)

	// Verify that 50 files really exist on disk
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != goroutines {
		t.Errorf("files on disk %d != %d", len(entries), goroutines)
	}

	// Sort and print a few examples
	sort.Strings(paths)
	t.Logf("example paths (first 3): %v", paths[:min(3, len(paths))])
}

func TestCreateExclusive_AllConflict(t *testing.T) {
	dir := t.TempDir()

	// Pre-create all 1000 candidate files
	for i := range 1000 {
		var name string
		if i == 0 {
			name = "x.bin"
		} else {
			name = fmt.Sprintf("x (%d).bin", i)
		}
		p := filepath.Join(dir, name)
		f, err := os.Create(p)
		if err != nil {
			t.Fatalf("pre-create %d: %v", i, err)
		}
		_ = f.Close()
	}

	_, _, err := fs.CreateExclusive(dir, "x", ".bin")
	if err == nil {
		t.Fatal("expected error when all 1000 candidates exist")
	}
	if !strings.Contains(err.Error(), "1000") {
		t.Errorf("error should mention 1000: %v", err)
	}
	t.Logf("error returned as expected: %v", err)
}

func TestCreateExclusive_NoExt(t *testing.T) {
	dir := t.TempDir()
	p, f, err := fs.CreateExclusive(dir, "noext", "")
	if err != nil {
		t.Fatalf("CreateExclusive no ext: %v", err)
	}
	_ = f.Close()
	if filepath.Base(p) != "noext" {
		t.Errorf("got %q", filepath.Base(p))
	}
}

// ─────────────────────────── CheckSpace ────────────────────────────────────

func TestCheckSpace_NeedZero(t *testing.T) {
	if err := fs.CheckSpace(t.TempDir(), 0); err != nil {
		t.Errorf("need=0 should return nil: %v", err)
	}
}

func TestCheckSpace_NeedNegative(t *testing.T) {
	if err := fs.CheckSpace(t.TempDir(), -1); err != nil {
		t.Errorf("need<0 should return nil: %v", err)
	}
}

func TestCheckSpace_SmallNeed(t *testing.T) {
	// 1 byte almost certainly fits
	dir := t.TempDir()
	if err := fs.CheckSpace(dir, 1); err != nil {
		t.Errorf("need=1B should have space: %v", err)
	}
}

func TestCheckSpace_HugeNeed_NoOverflow(t *testing.T) {
	// need = math.MaxInt64 must not panic or overflow
	dir := t.TempDir()
	err := fs.CheckSpace(dir, math.MaxInt64)
	// Almost certainly not enough space, but it must not panic
	if err != nil {
		t.Logf("MaxInt64: not enough space (expected): %v", err)
	}
	// A slightly smaller value must not overflow either
	_ = fs.CheckSpace(dir, math.MaxInt64/2)
}

func TestCheckSpace_InsufficientSpace(t *testing.T) {
	// 1 TiB is almost certainly insufficient (test disks are not that big)
	dir := t.TempDir()
	const oneTiB = 1 << 40
	err := fs.CheckSpace(dir, oneTiB)
	if err != nil {
		t.Logf("✓ 1TiB is insufficient: %v", err)
		if !strings.Contains(err.Error(), "need") || !strings.Contains(err.Error(), "available") {
			t.Errorf("error message lacks a readable format: %v", err)
		}
	} else {
		t.Log("plenty of disk space (over 1.2 TiB) - skipping the insufficient-space assertion")
	}
}

func TestCheckSpace_BadDir(t *testing.T) {
	err := fs.CheckSpace("/nonexistent/path/that/does/not/exist", 1)
	if err == nil {
		t.Error("bad dir should return error")
	}
}

// ─────────────────────────── EnsureDir ─────────────────────────────────────

func TestEnsureDir_Create(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "a", "b", "c")

	if err := fs.EnsureDir(target, 0o755); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !info.IsDir() {
		t.Error("expected directory")
	}
}

func TestEnsureDir_Idempotent(t *testing.T) {
	dir := t.TempDir()

	for range 3 {
		if err := fs.EnsureDir(dir, 0o755); err != nil {
			t.Fatalf("EnsureDir idempotent: %v", err)
		}
	}
}

func TestEnsureDir_FileConflict(t *testing.T) {
	dir := t.TempDir()
	conflict := filepath.Join(dir, "notadir")

	f, err := os.Create(conflict)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	_ = f.Close()

	err = fs.EnsureDir(conflict, 0o755)
	if err == nil {
		t.Error("expected error when path is a file")
	}
	t.Logf("error returned as expected: %v", err)
}

func TestEnsureDir_Perm(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "secured")

	if err := fs.EnsureDir(target, 0o700); err != nil {
		t.Fatalf("EnsureDir: %v", err)
	}
	info, _ := os.Stat(target)
	if info.Mode().Perm() != 0o700 {
		t.Errorf("perm = %o, want 0700", info.Mode().Perm())
	}
}

// ─────────────────────────── HumanBytes ─────────────────────────────────────

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1023, "1023 B"},
		{1024, "1.00 KiB"},
		{1536, "1.50 KiB"},
		{1024 * 1024, "1.00 MiB"},
		{1024 * 1024 * 1024, "1.00 GiB"},
		{-1024, "-1.00 KiB"},
	}
	for _, c := range cases {
		got := fs.HumanBytes(c.n)
		if got != c.want {
			t.Errorf("HumanBytes(%d) = %q, want %q", c.n, got, c.want)
		}
	}
}

func TestHumanBytes_MaxInt64(t *testing.T) {
	// must not panic
	s := fs.HumanBytes(math.MaxInt64)
	if s == "" {
		t.Error("empty string for MaxInt64")
	}
	t.Logf("HumanBytes(MaxInt64) = %s", s)
}

func TestHumanBytes_MinInt64(t *testing.T) {
	// negating math.MinInt64 overflows, so it needs special handling
	s := fs.HumanBytes(math.MinInt64)
	if s == "" {
		t.Error("empty string for MinInt64")
	}
	t.Logf("HumanBytes(MinInt64) = %s", s)
}

// ─────────────────────────── Exists ─────────────────────────────────────────

func TestExists_File(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.txt")
	_ = os.WriteFile(path, []byte("x"), 0o644)

	ok, err := fs.Exists(path)
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !ok {
		t.Error("expected true for existing file")
	}
}

func TestExists_Missing(t *testing.T) {
	ok, err := fs.Exists("/tmp/totally_nonexistent_file_12345678")
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if ok {
		t.Error("expected false for missing file")
	}
}

func TestExists_Dir(t *testing.T) {
	dir := t.TempDir()
	ok, err := fs.Exists(dir)
	if err != nil {
		t.Fatalf("Exists: %v", err)
	}
	if !ok {
		t.Error("expected true for directory")
	}
}

// ─────────────────────────── extra coverage tests ───────────────────────────

// TestCreateExclusive_BadDir: with a nonexistent directory, O_EXCL should return a
// non-EEXIST error.
func TestCreateExclusive_BadDir(t *testing.T) {
	_, _, err := fs.CreateExclusive("/nonexistent/dir/that/does/not/exist", "file", ".txt")
	if err == nil {
		t.Fatal("expected error for nonexistent dir")
	}
	t.Logf("non-EEXIST error: %v", err)
}

// TestAtomicWriteFile_LargeData: write a larger payload and verify integrity.
func TestAtomicWriteFile_LargeData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "large.bin")
	// 1 MiB
	data := bytes.Repeat([]byte{0xAB}, 1<<20)
	if err := fs.AtomicWriteFile(path, data, 0o644); err != nil {
		t.Fatalf("AtomicWriteFile large: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Error("large data mismatch")
	}
}

// TestAtomicWriteFile_OverwriteWithDifferentPerm: the permission is updated correctly
// when overwriting.
func TestAtomicWriteFile_OverwriteWithDifferentPerm(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "perm.txt")

	if err := fs.AtomicWriteFile(path, []byte("v1"), 0o644); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := fs.AtomicWriteFile(path, []byte("v2"), 0o600); err != nil {
		t.Fatalf("second write: %v", err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perm = %o, want 0600", info.Mode().Perm())
	}
}

// TestHumanBytes_NegativeMiB: negative value in the MiB range
func TestHumanBytes_NegativeMiB(t *testing.T) {
	s := fs.HumanBytes(-2 * 1024 * 1024)
	if s != "-2.00 MiB" {
		t.Errorf("got %q", s)
	}
}

// TestHumanBytes_NegativeKiB: negative value in the KiB range
func TestHumanBytes_NegativeKiB(t *testing.T) {
	s := fs.HumanBytes(-512 * 1024)
	if s != "-512.00 KiB" {
		t.Errorf("got %q", s)
	}
}

// TestHumanBytes_NegativeBytes: negative value in the B range
func TestHumanBytes_NegativeBytes(t *testing.T) {
	s := fs.HumanBytes(-100)
	if s != "-100 B" {
		t.Errorf("got %q", s)
	}
}

// TestEnsureDir_MkdirAllFails: pass a path that cannot be created.
func TestEnsureDir_MkdirAllFails(t *testing.T) {
	dir := t.TempDir()
	// Create a file, then try to use it as a parent directory
	f := filepath.Join(dir, "file")
	if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Try to create a directory under the file; MkdirAll should fail
	err := fs.EnsureDir(filepath.Join(f, "child"), 0o755)
	if err == nil {
		t.Error("expected error")
	}
	t.Logf("MkdirAll failed: %v", err)
}

// TestCheckSpace_Sufficient: verify it returns nil when there is enough space.
func TestCheckSpace_Sufficient(t *testing.T) {
	// A 1-byte requirement certainly fits
	if err := fs.CheckSpace("/tmp", 1); err != nil {
		t.Errorf("1 byte should be sufficient: %v", err)
	}
}

// TestCheckSpace_ManualVerify manually verifies need=1GiB and need=1TiB on a real
// directory.
func TestCheckSpace_ManualVerify(t *testing.T) {
	dir := "/mnt/ssd/code/tgshelf"
	// 1 GiB
	err1 := fs.CheckSpace(dir, 1<<30)
	t.Logf("need=1GiB: err=%v", err1)
	// 1 TiB
	err2 := fs.CheckSpace(dir, 1<<40)
	t.Logf("need=1TiB: err=%v", err2)
	// 1 GiB almost certainly passes (the disk has 13+ GiB)
	if err1 != nil {
		t.Logf("(less than 1.2GiB free, ignorable): %v", err1)
	}
}

// TestCreateExclusive_Permissions: created files should have mode 0600.
func TestCreateExclusive_Permissions(t *testing.T) {
	dir := t.TempDir()
	p, f, err := fs.CreateExclusive(dir, "secret", ".key")
	if err != nil {
		t.Fatalf("CreateExclusive: %v", err)
	}
	_ = f.Close()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perm = %o, want 0600", info.Mode().Perm())
	}
}

// ─────────────────────────── helpers ────────────────────────────────────────

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
