// task_test.go - offline unit tests for the task orchestration.
//
// Every test uses the fake getFileRPC (reusing fakeRPC from download_test.go) and issues
// no real network request.
package tg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/gotd/td/tg"

	"github.com/1368129224/tgshelf/internal/fs"
	"github.com/1368129224/tgshelf/internal/state"
)

// taskFakeItem builds a test TaskItem (the content comes from the deterministic seedData
// of fakeRPC).
func taskFakeItem(seed []byte, rel string, msgID int) TaskItem {
	return TaskItem{
		Item: MediaItem{
			Kind:      MediaDocument,
			Name:      filepath.Base(rel),
			Size:      int64(len(seed)),
			DCID:      2,
			Mime:      "application/octet-stream",
			FileRef:   []byte{1, 2, 3},
			Location:  &tg.InputDocumentFileLocation{ID: int64(msgID), AccessHash: 7, FileReference: []byte{1, 2, 3}},
			MessageID: msgID,
			DialogID:  100,
		},
		RelPath: rel,
		Mtime:   time.Unix(1700000000, 0),
	}
}

// newTaskRunner creates a TaskRunner with zero backoff (to speed the tests up).
func newTaskRunner(t *testing.T, rpc getFileRPC, cfg RunnerConfig) *TaskRunner {
	t.Helper()
	r, err := NewTaskRunner(rpc, zap.NewNop(), cfg)
	if err != nil {
		t.Fatalf("NewTaskRunner: %v", err)
	}
	return r
}

// taskSeed builds deterministic data of size bytes.
func taskSeed(size int64) []byte {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i * 7)
	}
	return data
}

// waitFor waits until cond is true (for fake blocking scenarios).
func waitFor(t *testing.T, cond func() bool, timeout time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

// table-driven test of the bestThreads tiers (including truncation by max, size=0 and
// the max=0 fallback).
func TestBestThreadsTable(t *testing.T) {
	cases := []struct {
		size int64
		max  int
		want int
	}{
		{0, 8, 1},         // size=0 -> tier 1
		{1<<20 - 1, 8, 1}, // <1MiB
		{1 << 20, 8, 2},   // ==1MiB -> the <5MiB tier
		{5<<20 - 1, 8, 2},
		{5 << 20, 8, 4}, // ==5MiB -> the <20MiB tier
		{20<<20 - 1, 8, 4},
		{20 << 20, 8, 8},                        // ==20MiB -> 8
		{50 << 20, 8, 8},                        // >=50MiB -> 8
		{100 << 20, 4, 4},                       // truncated by max=4
		{100 << 20, 1, 1},                       // truncated by max=1
		{100 << 20, 0, defaultDownloadThreads},  // max=0 fallback
		{100 << 20, -3, defaultDownloadThreads}, // max<0 fallback
		{0, 0, 1},                               // both zero fallback
	}
	for _, c := range cases {
		if got := bestThreads(c.size, c.max); got != c.want {
			t.Errorf("bestThreads(size=%d, max=%d) = %d, want %d", c.size, c.max, got, c.want)
		}
	}
}

// Extra for NewTaskRunner argument validation.
func TestNewTaskRunnerValidation(t *testing.T) {
	rpc := &fakeRPC{}
	if _, err := NewTaskRunner(nil, nil, RunnerConfig{RootDir: t.TempDir()}); err == nil {
		t.Error("nil rpc must fail")
	}
	if _, err := NewTaskRunner(rpc, nil, RunnerConfig{RootDir: ""}); err == nil {
		t.Error("empty RootDir must fail")
	}
	r := newTaskRunner(t, rpc, RunnerConfig{RootDir: t.TempDir(), FileConcurrency: 0})
	if r.workers != 1 {
		t.Errorf("with FileConcurrency<=0, workers must be 1, got %d", r.workers)
	}
}

// with 3 files and the 2nd one failing, files 1 and 3 land on disk, the overall result
// is FAILED, and Err mentions "2/3 succeeded".
func TestTaskFailureIsolation(t *testing.T) {
	root := t.TempDir()
	rpc := &fakeRPC{seedData: taskSeed(3 << 20)}
	// Chunk indexes start at 0: the 0th chunk of the 2nd file (1MiB) always fails.
	rpc.failMap = map[int64]int{}
	// failMap cannot distinguish files, so switch to a per-file fake that rejects by file.
	items := []TaskItem{
		taskFakeItem(taskSeed(1<<20), "a.bin", 1),
		taskFakeItem(taskSeed(1<<20), "b.bin", 2),
		taskFakeItem(taskSeed(1<<20), "c.bin", 3),
	}
	// Use the multi-file fake: reject requests for the 2nd file by message ID.
	multi := &fileSelectiveRPC{inner: rpc, failMsgID: 2}
	r := newTaskRunner(t, multi, RunnerConfig{RootDir: root, Threads: 2, FileConcurrency: 2})
	res, err := r.Run(context.Background(), "t1", items, nil)
	if err != nil {
		t.Fatalf("Run returned an error (it should return a task-level result): %v", err)
	}
	if res.State != state.StateFailed {
		t.Errorf("State = %s, want FAILED", res.State)
	}
	if res.Err == nil || !strings.Contains(res.Err.Error(), "2/3") || !strings.Contains(res.Err.Error(), "succeeded") {
		t.Errorf("Err must mention 2/3 succeeded, got %v", res.Err)
	}
	// Files 1 and 3 land on disk with the correct content.
	for _, rel := range []string{"a.bin", "c.bin"} {
		data, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatalf("read %s: %v", rel, err)
		}
		if string(data) != string(taskSeed(1<<20)) {
			t.Errorf("%s content mismatch", rel)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "b.bin")); !os.IsNotExist(err) {
		t.Errorf("b.bin must not exist: %v", err)
	}
	// The .part/.state of the failed file is kept (so it can be resumed).
	if _, err := os.Stat(filepath.Join(root, ".parts", "b.bin.part")); err != nil {
		t.Errorf("b.bin.part must be kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".parts", "b.bin.state")); err != nil {
		t.Errorf("b.bin.state must be kept: %v", err)
	}

}

// fileSelectiveRPC is a fake that fails selectively by DocID (= message ID).
type fileSelectiveRPC struct {
	inner     getFileRPC
	failMsgID int64
	calls     atomic.Int64
}

func (f *fileSelectiveRPC) UploadGetFile(ctx context.Context, req *tg.UploadGetFileRequest) (tg.UploadFileClass, error) {
	f.calls.Add(1)
	if f.failMsgID != 0 && inputDocID(req.Location) == f.failMsgID {
		return nil, fmt.Errorf("fake: file %d always fails", f.failMsgID)
	}
	return f.inner.UploadGetFile(ctx, req)
}

// a failed disk precheck returns an error immediately and leaves no artifact under
// rootDir.
func TestTaskDiskSpacePrecheck(t *testing.T) {
	// statfs on a nonexistent directory must fail (CheckSpace reports an error).
	root := filepath.Join(t.TempDir(), "no-such-dir")
	rpc := &fakeRPC{seedData: taskSeed(1 << 20)}
	r := newTaskRunner(t, rpc, RunnerConfig{RootDir: root})
	items := []TaskItem{taskFakeItem(taskSeed(1<<20), "x.bin", 1)}
	res, err := r.Run(context.Background(), "t-space", items, nil)
	if err == nil {
		t.Fatalf("a failed disk precheck must return an error")
	}
	if res != nil {
		t.Errorf("a failed precheck must not return a result, got %+v", res)
	}
	if !strings.Contains(err.Error(), "disk") && !strings.Contains(err.Error(), "precheck") {
		t.Errorf("the error must mention disk/precheck, got %v", err)
	}
	// The nonexistent directory itself was not created (no file may be produced).
	if _, serr := os.Stat(root); !os.IsNotExist(serr) {
		t.Errorf("rootDir must not be created: %v", serr)
	}
}

// when skip_same hits, no getFile is issued and Skipped=true.
func TestTaskSkipSame(t *testing.T) {
	root := t.TempDir()
	seed := taskSeed(1 << 20)
	rpc := &fakeRPC{seedData: seed}
	// Pre-place a final file with the same name and size.
	if err := fs.AtomicWriteFile(filepath.Join(root, "same.bin"), seed, 0o600); err != nil {
		t.Fatal(err)
	}
	r := newTaskRunner(t, rpc, RunnerConfig{RootDir: root, SkipSame: true})
	items := []TaskItem{taskFakeItem(seed, "same.bin", 1)}
	res, err := r.Run(context.Background(), "t-skip", items, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.State != state.StateDone {
		t.Errorf("State = %s, want DONE", res.State)
	}
	if res.Items[0].Skipped != true {
		t.Errorf("Skipped = false, want true")
	}
	if n := rpc.callCount(); n != 0 {
		t.Errorf("getFile calls = %d, want 0", n)
	}
}

// peak file concurrency <= FileConcurrency (counter plus peak recording).
func TestTaskFileConcurrencyPeak(t *testing.T) {
	root := t.TempDir()
	rpc := &fakeRPC{seedData: taskSeed(1 << 20)}
	// Use a blocking fake so that every file stalls on its first request, exposing the
	// concurrency peak.
	gate := &blockingRPC{tb: t, inner: rpc, limit: 3, entered: make(chan struct{}, 16), release: make(chan struct{}), seen: make(map[int64]struct{})}
	const fc = 3
	r := newTaskRunner(t, gate, RunnerConfig{RootDir: root, Threads: 2, FileConcurrency: fc})
	items := make([]TaskItem, 0, 8)
	for i := 0; i < 8; i++ {
		// 1MiB-1: a single-chunk file -> threads=1, so that the gate's active count
		// only reflects file concurrency.
		items = append(items, taskFakeItem(taskSeed(1<<20-1), fmt.Sprintf("f%d.bin", i), i+1))
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = r.Run(context.Background(), "t-conc", items, nil)
	}()
	// Wait for the first request of fc files (the entered buffer holds 16, so workers do
	// not block).
	for i := 0; i < fc; i++ {
		select {
		case <-gate.entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("the first request of file %d never arrived", i+1)
		}
	}
	if peak := gate.peak.Load(); peak > fc {
		t.Errorf("peak concurrency %d > FileConcurrency %d", peak, fc)
	}
	close(gate.release)
	<-done
	if peak := gate.peak.Load(); peak > fc {
		t.Errorf("final peak concurrency %d > FileConcurrency %d", peak, fc)
	}
}

// blockingRPC limits the number of concurrently entering requests and records the peak.
type blockingRPC struct {
	tb      *testing.T
	inner   getFileRPC
	limit   int
	active  atomic.Int64
	peak    atomic.Int64
	entered chan struct{}
	release chan struct{}
	mu      sync.Mutex
	seen    map[int64]struct{}
}

func (b *blockingRPC) UploadGetFile(ctx context.Context, req *tg.UploadGetFileRequest) (tg.UploadFileClass, error) {
	// Count per file: later chunk requests of the same file are not counted again (chunk
	// threads are not file concurrency, and the concurrency test asserts the file-level peak).
	// req.Location is a *tg.InputFileLocation (converted by fetchChunk from
	// InputDocumentFileLocation), and files are told apart by volume_id+local_id; step
	// back here and discriminate by the pair (ID, VolumeID): the ID field of
	// InputFileLocation is the document ID.
	// req.Location may be a *tg.InputFileLocation (converted by fetchChunk from
	// InputDocumentFileLocation, with LocalID derived from the document ID), or it may
	// still be a *tg.InputDocumentFileLocation; both must distinguish files.
	doc := inputDocID(req.Location)
	if doc == 0 {
		if l, ok := req.Location.(*tg.InputFileLocation); ok {
			doc = int64(l.LocalID)
		}
	}
	// The decision happens inside the lock (registering seen / counting), while blocking
	// and forwarding all happen outside it -- b.mu is never held while waiting for
	// release or calling inner.
	b.mu.Lock()
	_, firstReq := b.seen[doc]
	if !firstReq {
		b.seen[doc] = struct{}{}
	}
	b.mu.Unlock()

	if firstReq {
		return b.inner.UploadGetFile(ctx, req)
	}
	n := b.active.Add(1)
	defer b.active.Add(-1)
	for {
		p := b.peak.Load()
		if n <= p || b.peak.CompareAndSwap(p, n) {
			break
		}
	}
	b.entered <- struct{}{}
	b.tb.Logf("blockingRPC: first request of file %d, n=%d active=%d loc=%T", doc, n, b.active.Load(), req.Location)
	if n <= int64(b.limit) {
		select {
		case <-b.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return b.inner.UploadGetFile(ctx, req)
}

// after cancellation the completed files are intact, unfinished files keep their
// .part and .state, and a fresh Open resumes.
func TestTaskCancelPropagationAndResume(t *testing.T) {
	root := t.TempDir()
	seed := taskSeed(3 << 20)
	rpc := &fakeRPC{seedData: seed}
	// gate: the request of the 2nd file blocks and returns through ctx on cancellation.
	gate := &cancelGateRPC{inner: rpc, entered: make(chan struct{}, 16)}
	items := []TaskItem{
		taskFakeItem(taskSeed(1<<20), "done1.bin", 1),
		taskFakeItem(seed, "big.bin", 2),
		taskFakeItem(taskSeed(1<<20), "never.bin", 3),
	}
	r := newTaskRunner(t, gate, RunnerConfig{RootDir: root, Threads: 2, FileConcurrency: 1})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan *TaskResult, 1)
	runErr := make(chan error, 1)
	go func() {
		res, err := r.Run(ctx, "t-cancel", items, nil)
		done <- res
		runErr <- err
	}()
	// Cancel once the 2nd file has a request in flight.
	waitFor(t, func() bool { return gate.enteredCount.Load() > 0 }, 5*time.Second, "the 2nd file starts downloading")
	cancel()
	res := <-done
	if err := <-runErr; err != nil {
		t.Fatalf("Run returned an error: %v", err)
	}
	if res.State != state.StateCancelled {
		t.Errorf("State = %s, want CANCELLED", res.State)
	}
	if res.Err == nil || !errors.Is(res.Err, context.Canceled) {
		t.Errorf("Err must satisfy errors.Is(err, context.Canceled), got %v", res.Err)
	}
	// The completed file 1 is intact.
	data, err := os.ReadFile(filepath.Join(root, "done1.bin"))
	if err != nil || string(data) != string(taskSeed(1<<20)) {
		t.Errorf("done1.bin must land intact: err=%v len=%d", err, len(data))
	}
	// The unfinished file 2 keeps its .part and .state (runItem pre-Flushes once after
	// Open, which makes sure .state lands on disk).
	if _, err := os.Stat(filepath.Join(root, ".parts", "big.bin.part")); err != nil {
		t.Errorf("big.bin.part must be kept: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, ".parts", "big.bin.state")); err != nil {
		t.Errorf("big.bin.state must be kept: %v", err)
	}
	// The unfinished file 3: still queued and never started -> no artifact.
	if _, err := os.Stat(filepath.Join(root, "never.bin")); !os.IsNotExist(err) {
		t.Errorf("never.bin must not exist: %v", err)
	}

	// Run again (new ctx): big.bin resumes to completion.
	res2, err := r.Run(context.Background(), "t-cancel-2", []TaskItem{taskFakeItem(seed, "big.bin", 2)}, nil)
	if err != nil {
		t.Fatalf("resume Run: %v", err)
	}
	if res2.State != state.StateDone {
		t.Errorf("resume State = %s, want DONE (Err=%v)", res2.State, res2.Err)
	}
	data2, err := os.ReadFile(filepath.Join(root, "big.bin"))
	if err != nil || string(data2) != string(seed) {
		t.Errorf("big.bin content mismatch after resume: err=%v len=%d", err, len(data2))
	}
}

// cancelGateRPC blocks the first request until ctx is canceled (simulating a long
// download).
type cancelGateRPC struct {
	inner        getFileRPC
	entered      chan struct{}
	enteredCount atomic.Int64
}

func (g *cancelGateRPC) UploadGetFile(ctx context.Context, req *tg.UploadGetFileRequest) (tg.UploadFileClass, error) {
	// Only block requests for the target file (DocID=2): the first request belongs to
	// file 1 and must not be caught by mistake (otherwise the cancellation would happen
	// before file 1 finishes and the semantics would be entirely wrong).
	if inputDocID(req.Location) == 2 {
		if g.enteredCount.Add(1) == 1 {
			g.entered <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		}
	}
	return g.inner.UploadGetFile(ctx, req)
}

// Progress aggregation: with several files running concurrently the progress callback is
// invoked serially (a mutex detects data races), and the final ItemsDone/BytesDone values
// are correct.
func TestTaskProgressAggregation(t *testing.T) {
	root := t.TempDir()
	seed := taskSeed(1 << 20)
	rpc := &fakeRPC{seedData: seed}
	r := newTaskRunner(t, rpc, RunnerConfig{RootDir: root, Threads: 2, FileConcurrency: 4})
	items := make([]TaskItem, 0, 4)
	for i := 0; i < 4; i++ {
		items = append(items, taskFakeItem(seed, fmt.Sprintf("p%d.bin", i), i+1))
	}
	var calls atomic.Int64
	var lastBytes atomic.Int64
	var lastItems atomic.Int64
	var cbMu sync.Mutex
	res, err := r.Run(context.Background(), "t-prog", items, func(p TaskProgress) {
		cbMu.Lock()
		defer cbMu.Unlock()
		calls.Add(1)
		if p.BytesDone > lastBytes.Load() {
			lastBytes.Store(p.BytesDone)
		}
		if int64(p.ItemsDone) > lastItems.Load() {
			lastItems.Store(int64(p.ItemsDone))
		}
		if p.ItemsTotal != 4 {
			t.Errorf("ItemsTotal = %d, want 4", p.ItemsTotal)
		}
		if p.TaskID != "t-prog" {
			t.Errorf("TaskID = %q", p.TaskID)
		}
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.State != state.StateDone {
		t.Errorf("State = %s, want DONE", res.State)
	}
	if got := lastBytes.Load(); got != int64(len(items))*int64(len(seed)) {
		t.Errorf("final BytesDone = %d, want %d", got, int64(len(items))*int64(len(seed)))
	}
	if got := lastItems.Load(); got < 1 || got > 4 {
		t.Errorf("observed ItemsDone is out of range: %d", got)
	}
	if calls.Load() == 0 {
		t.Error("the progress callback was never invoked")
	}
}

// With skip_same off the same file is downloaded normally.
func TestTaskNoSkipSameFlag(t *testing.T) {
	root := t.TempDir()
	seed := taskSeed(512 << 10)
	rpc := &fakeRPC{seedData: seed}
	if err := fs.AtomicWriteFile(filepath.Join(root, "s.bin"), seed, 0o600); err != nil {
		t.Fatal(err)
	}
	r := newTaskRunner(t, rpc, RunnerConfig{RootDir: root, SkipSame: false})
	items := []TaskItem{taskFakeItem(seed, "s.bin", 1)}
	res, err := r.Run(context.Background(), "t-noskip", items, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Items[0].Skipped {
		t.Error("SkipSame=false must not skip")
	}
	if n := rpc.callCount(); n == 0 {
		t.Error("getFile must be issued")
	}
}

// An empty task list -> DONE with no progress event.
func TestTaskEmptyItems(t *testing.T) {
	root := t.TempDir()
	rpc := &fakeRPC{}
	r := newTaskRunner(t, rpc, RunnerConfig{RootDir: root})
	res, err := r.Run(context.Background(), "t-empty", nil, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.State != state.StateDone {
		t.Errorf("State = %s, want DONE", res.State)
	}
	if len(res.Items) != 0 {
		t.Errorf("Items must be empty, got %d", len(res.Items))
	}
}

// KeepParts: keep .state after success.
func TestTaskKeepParts(t *testing.T) {
	root := t.TempDir()
	seed := taskSeed(512 << 10)
	rpc := &fakeRPC{seedData: seed}
	r := newTaskRunner(t, rpc, RunnerConfig{RootDir: root, KeepParts: true})
	items := []TaskItem{taskFakeItem(seed, "k.bin", 1)}
	res, err := r.Run(context.Background(), "t-keep", items, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.State != state.StateDone {
		t.Fatalf("State = %s, want DONE", res.State)
	}
	if _, err := os.Stat(filepath.Join(root, ".parts", "k.bin.state")); err != nil {
		t.Errorf("with KeepParts=true .state must be kept: %v", err)
	}
}

// Mtime: the mtime is written to the final file after Commit.
func TestTaskMtimeApplied(t *testing.T) {
	root := t.TempDir()
	seed := taskSeed(512 << 10)
	rpc := &fakeRPC{seedData: seed}
	want := time.Unix(1234567890, 0)
	items := []TaskItem{taskFakeItem(seed, "m.bin", 1)}
	items[0].Mtime = want
	r := newTaskRunner(t, rpc, RunnerConfig{RootDir: root})
	if _, err := r.Run(context.Background(), "t-mtime", items, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	info, err := os.Stat(filepath.Join(root, "m.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if info.ModTime().Unix() != want.Unix() {
		t.Errorf("mtime = %v, want %v", info.ModTime(), want)
	}
}

// inputDocID extracts the ID of an InputDocumentFileLocation from an
// InputFileLocationClass.
func inputDocID(loc tg.InputFileLocationClass) int64 {
	if d, ok := loc.(*tg.InputDocumentFileLocation); ok {
		return d.ID
	}
	return 0
}

// TestBestThreadsLargeMax verifies that for very large files (>=50MB) the thread count can
// scale with the configured ratio up to max (e.g. 16 or 32), matching the original tdl
// tutil.BestThreads policy.
func TestBestThreadsLargeMax(t *testing.T) {
	cases := []struct {
		size int64
		max  int
		want int
	}{
		{100 << 20, 16, 16},
		{500 << 20, 32, 32},
		{2000 << 20, 16, 16},
		{10 << 20, 16, 4}, // the <20MB tier is capped at 4
		{40 << 20, 16, 8}, // the <50MB tier is capped at 8
	}
	for _, c := range cases {
		if got := bestThreads(c.size, c.max); got != c.want {
			t.Errorf("bestThreads(size=%d, max=%d) = %d, want %d", c.size, c.max, got, c.want)
		}
	}
}
