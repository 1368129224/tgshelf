package state

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// helper: open a fresh Session (root=dir, relPath="dir/base.ext").
func openNew(t *testing.T, dir string, size, chunk, docID int64, opts ...Option) *Session {
	t.Helper()
	s, err := Open(dir, filepath.Join("sub", "file.bin"), ResumeMeta{
		Size: size, ChunkSize: chunk, DocID: docID, Peer: 42, MsgID: 7,
	}, opts...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func mustOpenExisting(t *testing.T, dir string, size, chunk, docID int64) *Session {
	t.Helper()
	s, err := Open(dir, filepath.Join("sub", "file.bin"), ResumeMeta{
		Size: size, ChunkSize: chunk, DocID: docID, Peer: 42, MsgID: 7,
	})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// ---- basic layout ----

func TestOpenLayoutPathsAndPerms(t *testing.T) {
	dir := t.TempDir()
	s := openNew(t, dir, 1000, 100, 1)

	wantPart := filepath.Join(dir, "sub", ".parts", "file.bin.part")
	wantState := filepath.Join(dir, "sub", ".parts", "file.bin.state")
	if s.PartPath() != wantPart {
		t.Errorf("PartPath = %q, want %q", s.PartPath(), wantPart)
	}
	if s.StatePath() != wantState {
		t.Errorf("StatePath = %q, want %q", s.StatePath(), wantState)
	}
	info, err := os.Stat(filepath.Join(dir, "sub", ".parts"))
	if err != nil {
		t.Fatalf("stat .parts: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf(".parts perm = %o, want 700", perm)
	}
	if info, err := os.Stat(wantPart); err == nil {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("part perm = %o, want 600", perm)
		}
	}
	if s.ChunkCount() != 10 {
		t.Errorf("ChunkCount = %d, want 10", s.ChunkCount())
	}
}

// ---- sparseness ----

func TestSparseFileBlocks(t *testing.T) {
	dir := t.TempDir()
	const size = 1 << 30 // 1 GiB
	s := openNew(t, dir, size, 512*1024, 1)

	// write only 4 chunks (2 MiB of real data)
	buf := make([]byte, 512*1024)
	for i := int64(0); i < 4; i++ {
		if _, err := s.WriteAt(buf, i*512*1024); err != nil {
			t.Fatalf("WriteAt: %v", err)
		}
		if err := s.MarkDone(i); err != nil {
			t.Fatalf("MarkDone: %v", err)
		}
	}
	if err := s.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	st, err := os.Stat(s.PartPath())
	if err != nil {
		t.Fatalf("stat part: %v", err)
	}
	if st.Size() != size {
		t.Errorf("logical size = %d, want %d", st.Size(), size)
	}
	statT, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no Stat_t")
	}
	if used := statT.Blocks * 512; used >= 32<<20 {
		t.Errorf("disk usage = %d bytes, want < 32 MiB (sparse not honored?)", used)
	}
}

// ---- three checks: size mismatch restarts the download ----

func TestResetOnSizeMismatch(t *testing.T) {
	dir := t.TempDir()
	s := openNew(t, dir, 1000, 100, 1)
	if err := s.MarkDone(0); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := mustOpenExisting(t, dir, 2000, 100, 1) // size changed
	if s2.ResetReason() == "" {
		t.Fatal("expected non-empty ResetReason for size mismatch")
	}
	if got := s2.ResetReason(); !contains(got, "size mismatch") {
		t.Errorf("ResetReason = %q, want contains %q", got, "size mismatch")
	}
	if n := s2.Done().Count(); n != 0 {
		t.Errorf("after reset done count = %d, want 0", n)
	}
	// the old .state/.part were deleted and recreated
	if _, err := os.Stat(s2.PartPath()); err != nil {
		t.Errorf("part should exist after reset: %v", err)
	}
	if st, err := os.Stat(s2.PartPath()); err == nil && st.Size() != 2000 {
		t.Errorf("part size = %d, want 2000", st.Size())
	}
}

// ---- three checks: doc_id mismatch restarts the download ----

func TestResetOnDocIDMismatch(t *testing.T) {
	dir := t.TempDir()
	s := openNew(t, dir, 1000, 100, 1)
	if err := s.MarkDone(3); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := mustOpenExisting(t, dir, 1000, 100, 99) // doc_id changed
	if !contains(s2.ResetReason(), "doc_id mismatch") {
		t.Errorf("ResetReason = %q, want contains %q", s2.ResetReason(), "doc_id mismatch")
	}
	if n := s2.Done().Count(); n != 0 {
		t.Errorf("after reset done count = %d, want 0", n)
	}
}

// ---- three checks: chunk mismatch restarts the download ----

func TestResetOnChunkMismatch(t *testing.T) {
	dir := t.TempDir()
	s := openNew(t, dir, 1000, 100, 1)
	if err := s.MarkDone(3); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2 := mustOpenExisting(t, dir, 1000, 200, 1) // chunk changed
	if !contains(s2.ResetReason(), "chunk mismatch") {
		t.Errorf("ResetReason = %q, want contains %q", s2.ResetReason(), "chunk mismatch")
	}
	if n := s2.Done().Count(); n != 0 {
		t.Errorf("after reset done count = %d, want 0", n)
	}
	if s2.ChunkCount() != 5 {
		t.Errorf("ChunkCount = %d, want 5", s2.ChunkCount())
	}
}

// ---- corrupt .state restarts the download ----

func TestResetOnCorruptState(t *testing.T) {
	dir := t.TempDir()
	s := openNew(t, dir, 1000, 100, 1)
	if err := s.MarkDone(1); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	corrupt := []byte("this is { not valid json base64 ]]")
	if err := os.WriteFile(s.StatePath(), corrupt, 0o600); err != nil {
		t.Fatal(err)
	}

	s2 := mustOpenExisting(t, dir, 1000, 100, 1)
	if !contains(s2.ResetReason(), "state corrupt") {
		t.Errorf("ResetReason = %q, want contains %q", s2.ResetReason(), "state corrupt")
	}
	if n := s2.Done().Count(); n != 0 {
		t.Errorf("after reset done count = %d, want 0", n)
	}
}

// invalid base64 bitmap (valid JSON but done is not valid base64) restarts as well.
func TestResetOnBadBase64(t *testing.T) {
	dir := t.TempDir()
	s := openNew(t, dir, 1000, 100, 1)
	_ = s
	raw := `{"v":1,"size":1000,"chunk":100,"done":"!!!not-base64!!!","doc_id":1,"peer":42,"msg_id":7,"updated_at":1}`
	if err := os.WriteFile(filepath.Join(dir, "sub", ".parts", "file.bin.state"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	s2 := mustOpenExisting(t, dir, 1000, 100, 1)
	if !contains(s2.ResetReason(), "state corrupt") {
		t.Errorf("ResetReason = %q, want contains %q", s2.ResetReason(), "state corrupt")
	}
}

// truncated JSON restarts the download.
func TestResetOnTruncatedJSON(t *testing.T) {
	dir := t.TempDir()
	s := openNew(t, dir, 1000, 100, 1)
	if err := s.MarkDone(0); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(s.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.StatePath(), b[:len(b)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	s2 := mustOpenExisting(t, dir, 1000, 100, 1)
	if !contains(s2.ResetReason(), "state corrupt") {
		t.Errorf("ResetReason = %q, want contains %q", s2.ResetReason(), "state corrupt")
	}
}

// ---- reuse: all three checks match → keep the bitmap ----

func TestResumeReusesBitmap(t *testing.T) {
	dir := t.TempDir()
	s := openNew(t, dir, 1000, 100, 1)
	for i := int64(0); i < 4; i++ {
		if err := s.MarkDone(i); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.Abort(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	// both .part and .state exist after Abort
	if _, err := os.Stat(s.PartPath()); err != nil {
		t.Errorf("expected .part to exist after Abort: %v", err)
	}
	if _, err := os.Stat(s.StatePath()); err != nil {
		t.Errorf("expected .state to exist after Abort: %v", err)
	}

	s2 := mustOpenExisting(t, dir, 1000, 100, 1)
	if s2.ResetReason() != "" {
		t.Errorf("expected reuse, ResetReason = %q", s2.ResetReason())
	}
	for i := 0; i < 4; i++ {
		v, err := s2.Done().Get(i)
		if err != nil || !v {
			t.Errorf("chunk %d should still be done (err=%v v=%v)", i, err, v)
		}
	}
	if n := s2.Done().Count(); n != 4 {
		t.Errorf("done count = %d, want 4", n)
	}
}

// ---- Commit ----

func TestCommitDefaultDeletesState(t *testing.T) {
	dir := t.TempDir()
	s := openNew(t, dir, 300, 100, 1)
	payload := []byte("hello world this is chunk data!!")
	for i := int64(0); i < 3; i++ {
		if _, err := s.WriteAt(payload, i*100); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkDone(i); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	final := filepath.Join(dir, "sub", "file.bin")
	info, err := os.Stat(final)
	if err != nil {
		t.Fatalf("final file missing: %v", err)
	}
	if info.Size() != 300 {
		t.Errorf("final size = %d, want 300", info.Size())
	}
	if _, err := os.Stat(s.PartPath()); !os.IsNotExist(err) {
		t.Errorf(".part should be gone after Commit, stat err = %v", err)
	}
	if _, err := os.Stat(s.StatePath()); !os.IsNotExist(err) {
		t.Errorf(".state should be deleted by default after Commit, stat err = %v", err)
	}
	data, err := os.ReadFile(final)
	if err != nil {
		t.Fatal(err)
	}
	want := make([]byte, 300) // unwritten regions are zero (sparse zero fill)
	for i := 0; i < 3; i++ {
		copy(want[i*100:], payload)
	}
	if string(data) != string(want) {
		t.Errorf("final content mismatch")
	}
}

func TestCommitKeepState(t *testing.T) {
	dir := t.TempDir()
	s := openNew(t, dir, 300, 100, 1, WithKeepState(true))
	if err := s.MarkDone(0); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.StatePath()); err != nil {
		t.Errorf(".state should be kept with WithKeepState(true): %v", err)
	}
	if _, err := os.Stat(s.PartPath()); !os.IsNotExist(err) {
		t.Errorf(".part should still be renamed away: %v", err)
	}
}

func TestCommitWithMtime(t *testing.T) {
	dir := t.TempDir()
	when := time.Date(2020, 5, 1, 12, 0, 0, 0, time.UTC)
	s := openNew(t, dir, 100, 100, 1, WithMtime(when))
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "sub", "file.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(when) {
		t.Errorf("mtime = %v, want %v", info.ModTime(), when)
	}
}

// ---- zero-byte file and a non-divisible tail chunk (Constraints 6) ----

func TestZeroSizeFile(t *testing.T) {
	dir := t.TempDir()
	s := openNew(t, dir, 0, 100, 1)
	if s.ChunkCount() != 0 {
		t.Errorf("ChunkCount(0 size) = %d, want 0", s.ChunkCount())
	}
	if err := s.MarkDone(0); err == nil {
		t.Error("MarkDone on empty file should fail")
	}
	if err := s.Commit(); err != nil {
		t.Fatalf("Commit empty file: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "sub", "file.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Errorf("size = %d, want 0", info.Size())
	}
}

func TestTailChunkNotDivisible(t *testing.T) {
	dir := t.TempDir()
	// 1000 bytes with chunk=300 gives 4 chunks: 0,1,2 are full and chunk 3 holds 100 bytes.
	s := openNew(t, dir, 1000, 300, 1)
	if s.ChunkCount() != 4 {
		t.Fatalf("ChunkCount = %d, want 4", s.ChunkCount())
	}
	buf := make([]byte, 300)
	for i := int64(0); i < 3; i++ {
		if _, err := s.WriteAt(buf, i*300); err != nil {
			t.Fatal(err)
		}
		if err := s.MarkDone(i); err != nil {
			t.Fatal(err)
		}
	}
	// the tail chunk holds only 100 bytes
	if _, err := s.WriteAt(make([]byte, 100), 900); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkDone(3); err != nil {
		t.Fatalf("MarkDone tail chunk: %v", err)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, "sub", "file.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 1000 {
		t.Errorf("size = %d, want 1000", info.Size())
	}
}

// ---- ShouldFlush semantics ----

func TestShouldFlushThresholds(t *testing.T) {
	dir := t.TempDir()
	s := openNew(t, dir, 100000, 100, 1)
	defer func() { _ = s.Close() }()

	if s.ShouldFlush(0) {
		t.Error("fresh session should not need flush (chunks=0, just opened)")
	}
	for i := int64(0); i < 31; i++ {
		if err := s.MarkDone(i); err != nil {
			t.Fatal(err)
		}
	}
	if s.ShouldFlush(0) {
		t.Error("31 chunks should not trigger")
	}
	if err := s.MarkDone(31); err != nil {
		t.Fatal(err)
	}
	if !s.ShouldFlush(0) {
		t.Error("32 chunks via MarkDone should trigger")
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if s.ShouldFlush(0) {
		t.Error("right after Flush should not trigger")
	}

	// time threshold: backdate lastFlush by 6s (built by hand on a new session)
	s2 := &Session{numChunks: 100, lastFlush: time.Now().Add(-6 * time.Second)}
	if !s2.ShouldFlush(0) {
		t.Error("≥5s since last flush should trigger")
	}
	s3 := &Session{numChunks: 100, lastFlush: time.Now()}
	if s3.ShouldFlush(0) {
		t.Error("fresh timestamp should not trigger")
	}
	// caller count path
	s4 := &Session{numChunks: 100, lastFlush: time.Now()}
	if !s4.ShouldFlush(32) {
		t.Error("caller-provided 32 completed chunks should trigger")
	}
}

// ---- concurrency safety ----

func TestConcurrentMarkDone(t *testing.T) {
	dir := t.TempDir()
	const chunks = 256
	s := openNew(t, dir, int64(chunks)*100, 100, 1)
	defer func() { _ = s.Close() }()

	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := g; i < chunks; i += 8 {
				if err := s.MarkDone(int64(i)); err != nil {
					t.Errorf("MarkDone(%d): %v", i, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()

	if n := s.Done().Count(); n != chunks {
		t.Errorf("done count = %d, want %d", n, chunks)
	}
}

// mixed MarkDone + Flush contention (verified under -race).
func TestConcurrentMarkDoneAndFlush(t *testing.T) {
	dir := t.TempDir()
	const chunks = 128
	s := openNew(t, dir, int64(chunks)*100, 100, 1)
	defer func() { _ = s.Close() }()

	var wg, flusher sync.WaitGroup
	stop := make(chan struct{})
	flusher.Add(1)
	go func() {
		defer flusher.Done()
		for {
			select {
			case <-stop:
				return
			default:
				if err := s.Flush(); err != nil {
					t.Errorf("Flush: %v", err)
					return
				}
				time.Sleep(time.Millisecond)
			}
		}
	}()
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := g; i < chunks; i += 8 {
				if err := s.MarkDone(int64(i)); err != nil {
					t.Errorf("MarkDone(%d): %v", i, err)
					return
				}
			}
		}(g)
	}
	wg.Wait()
	close(stop)
	flusher.Wait()
}

// ---- path traversal guard (Constraints 3) ----

func TestPathTraversalRejected(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"../escape.bin", "a/../../escape.bin", "..", "sub/../../../etc/passwd"} {
		if _, err := Open(dir, bad, ResumeMeta{Size: 10, ChunkSize: 10, DocID: 1}); err == nil {
			t.Errorf("relPath %q should be rejected", bad)
		}
	}
	// legal paths are unaffected
	s := openNew(t, dir, 10, 10, 1)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// under an absolute root, odd but legal relPath parts such as backslashes must not escape
	s2, err := Open(dir, "sub/./file2.bin", ResumeMeta{Size: 10, ChunkSize: 10, DocID: 1})
	if err != nil {
		t.Fatalf("normal path rejected: %v", err)
	}
	_ = s2.Close()
}

// ---- part truncation guard: bitmap claims done but part lacks data → restart ----

func TestResetWhenPartTruncated(t *testing.T) {
	dir := t.TempDir()
	s := openNew(t, dir, 1000, 100, 1)
	if err := s.MarkDone(5); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// truncate the part to 100 bytes (the data of chunk 5 is gone)
	if err := os.Truncate(s.PartPath(), 100); err != nil {
		t.Fatal(err)
	}
	s2 := mustOpenExisting(t, dir, 1000, 100, 1)
	if !contains(s2.ResetReason(), "part truncated") {
		t.Errorf("ResetReason = %q, want contains %q", s2.ResetReason(), "part truncated")
	}
	if n := s2.Done().Count(); n != 0 {
		t.Errorf("after reset done count = %d, want 0", n)
	}
}

// .state exists but part is missing → restart.
func TestResetWhenPartMissing(t *testing.T) {
	dir := t.TempDir()
	s := openNew(t, dir, 1000, 100, 1)
	if err := s.MarkDone(2); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.PartPath()); err != nil {
		t.Fatal(err)
	}
	s2 := mustOpenExisting(t, dir, 1000, 100, 1)
	if !contains(s2.ResetReason(), "part file missing") {
		t.Errorf("ResetReason = %q, want contains %q", s2.ResetReason(), "part file missing")
	}
}

// the .state JSON written by Flush matches the contract format.
func TestStateJSONShape(t *testing.T) {
	dir := t.TempDir()
	s := openNew(t, dir, 1000, 100, 1)
	if err := s.MarkDone(0); err != nil {
		t.Fatal(err)
	}
	if err := s.Flush(); err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
	b, err := os.ReadFile(s.StatePath())
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("state not valid JSON: %v", err)
	}
	for _, k := range []string{"v", "size", "chunk", "done", "doc_id", "peer", "msg_id", "updated_at"} {
		if _, ok := m[k]; !ok {
			t.Errorf("state JSON missing key %q: %s", k, b)
		}
	}
	if v, _ := m["v"].(float64); v != 1 {
		t.Errorf("v = %v, want 1", m["v"])
	}
	done, _ := m["done"].(string)
	if done == "" {
		t.Errorf("done should be base64 string, got %q", m["done"])
	}
}

// mismatched version restarts the download (guards against old formats).
func TestResetOnVersionMismatch(t *testing.T) {
	dir := t.TempDir()
	s := openNew(t, dir, 1000, 100, 1)
	_ = s
	raw := fmt.Sprintf(`{"v":99,"size":1000,"chunk":100,"done":"%s","doc_id":1,"peer":42,"msg_id":7,"updated_at":1}`, base64Empty(10))
	if err := os.WriteFile(filepath.Join(dir, "sub", ".parts", "file.bin.state"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	s2 := mustOpenExisting(t, dir, 1000, 100, 1)
	if !contains(s2.ResetReason(), "version mismatch") {
		t.Errorf("ResetReason = %q, want contains %q", s2.ResetReason(), "version mismatch")
	}
}

func base64Empty(n int) string {
	b := make([]byte, n)
	return base64.StdEncoding.EncodeToString(b)
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
