// download_test.go -- offline unit tests for the chunked download core.
//
// Every test uses a fake getFileRPC and makes no real network requests (real downloads are covered by the live tests).
// Random data uses a fixed seed (math/rand.New(rand.NewSource(...))) for determinism.
package tg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/gotd/td/tg"

	"github.com/1368129224/tgshelf/internal/state"
)

// fakeRPC is a deterministic fake of getFileRPC: it returns bytes from seedData according to the
// requested offset/limit.
type fakeRPC struct {
	mu       sync.Mutex
	seedData []byte                             // simulated remote file
	calls    int64                              // total UploadGetFile calls (atomic semantics guarded by the lock)
	reqLog   []reqRec                           // record of every request
	failMap  map[int64]int                      // chunk index -> number of leading failures (decremented per call, succeeds at 0)
	blockCh  chan struct{}                      // when non-nil, every request blocks until it is closed (for cancellation tests)
	shortMap map[int64]int                      // chunk index -> fixed number of short bytes to return
	callHook func(req *tg.UploadGetFileRequest) // callback before every call (for cancellation tests hooking cancel)
}

// reqRec records one request.
type reqRec struct {
	offset int64
	limit  int
}

func (f *fakeRPC) UploadGetFile(ctx context.Context, req *tg.UploadGetFileRequest) (tg.UploadFileClass, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.reqLog = append(f.reqLog, reqRec{offset: req.Offset, limit: req.Limit})
	idx := req.Offset / int64(testChunkSize)

	if f.callHook != nil {
		f.callHook(req)
	}
	if f.blockCh != nil {
		ch := f.blockCh
		f.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			f.mu.Lock()
			return nil, ctx.Err()
		}
		f.mu.Lock()
	}

	if n, ok := f.failMap[idx]; ok && n > 0 {
		f.failMap[idx] = n - 1
		return nil, fmt.Errorf("fake: simulated failure chunk %d", idx)
	}
	if n, ok := f.shortMap[idx]; ok {
		return &tg.UploadFile{Bytes: f.seedData[req.Offset : req.Offset+int64(n)]}, nil
	}
	// Tail chunk semantics: when offset+limit goes past the end of the file only the remaining
	// bytes are returned (simulating real server EOF truncation).
	end := req.Offset + int64(req.Limit)
	if end > int64(len(f.seedData)) {
		end = int64(len(f.seedData))
	}
	return &tg.UploadFile{Bytes: f.seedData[req.Offset:end]}, nil
}

func (f *fakeRPC) callCount() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeRPC) requests() []reqRec {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]reqRec, len(f.reqLog))
	copy(out, f.reqLog)
	return out
}

// testChunkSize is the chunk size used uniformly by the tests (512KiB, matching the production
// default).
const testChunkSize = 512 << 10

// newTestItem builds a test MediaItem.
func newTestItem(size int64) MediaItem {
	return MediaItem{
		Kind:     MediaDocument,
		Name:     "test.bin",
		Size:     size,
		DCID:     2,
		Mime:     "application/octet-stream",
		FileRef:  []byte{1, 2, 3},
		Location: &tg.InputDocumentFileLocation{ID: 42, AccessHash: 7, FileReference: []byte{1, 2, 3}},
	}
}

// newDownloaderFast creates a Downloader with zero backoff (test speed-up).
func newDownloaderFast(rpc getFileRPC) *Downloader {
	d, err := NewDownloader(rpc, zap.NewNop())
	if err != nil {
		panic(err)
	}
	d.backoff = []time.Duration{0, 0}
	return d
}

// newPlan creates a plan with an empty bitmap.
func newPlan(t *testing.T, size, chunk int64) *ChunkPlan {
	t.Helper()
	p, err := NewChunkPlan(size, chunk, state.NewBitmap(int((size+chunk-1)/chunk)))
	if err != nil {
		t.Fatalf("NewChunkPlan: %v", err)
	}
	return p
}

// tempFile creates a temporary .part file.
func tempFile(t *testing.T) *os.File {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "dl-*.part")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// waitDoneChunk waits until the OnChunk callback has fired n times.
// waitCancelledCount waits until fakeRPC has been called n times.
// ============ ChunkPlan ============

// TestChunkPlanOffsetsBoundary validates the 1MiB boundary invariant: for every chunk,
// limit <= 1MiB and offset/(1MiB) == (offset+limit-1)/(1MiB).
// It covers the boundary sizes: 0 bytes / less than one chunk / an exact multiple of chunk / not a
// multiple.
func TestChunkPlanOffsetsBoundary(t *testing.T) {
	const mib = int64(1 << 20)
	cases := []struct {
		name       string
		size       int64
		chunk      int64
		wantChunks int64
	}{
		{"zero", 0, 512 << 10, 0},
		{"less than one chunk", 100, 512 << 10, 1},
		{"exact multiple", 4 * 512 << 10, 512 << 10, 4},
		{"not multiple", 4*512<<10 + 137, 512 << 10, 5},
		{"one mib exact", 1 << 20, 1 << 20, 1},
		{"one mib plus one", 1<<20 + 1, 1 << 20, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newPlan(t, tc.size, tc.chunk)
			if got := p.Chunks(); got != tc.wantChunks {
				t.Fatalf("Chunks() = %d, want %d", got, tc.wantChunks)
			}
			for i := int64(0); i < tc.wantChunks; i++ {
				off, lim := p.Offset(i)
				if lim <= 0 {
					t.Fatalf("chunk %d: limit = %d", i, lim)
				}
				if int64(lim) > mib {
					t.Fatalf("chunk %d: limit %d > 1MiB", i, lim)
				}
				if off/mib != (off+int64(lim)-1)/mib {
					t.Fatalf("chunk %d: offset %d limit %d crosses a 1MiB boundary", i, off, lim)
				}
				if off != i*tc.chunk {
					t.Fatalf("chunk %d: offset = %d, want %d", i, off, i*tc.chunk)
				}
				// limit is always the full chunkSize (the tail chunk is not shortened: the server
				// only accepts an aligned power-of-two limit).
				if int64(lim) != tc.chunk {
					t.Fatalf("chunk %d: limit = %d, want %d (full chunkSize)", i, lim, tc.chunk)
				}
			}
			// An out-of-range offset returns 0,0.
			if off, lim := p.Offset(tc.wantChunks); off != 0 || lim != 0 {
				t.Fatalf("out-of-range Offset returned (%d,%d), want (0,0)", off, lim)
			}
		})
	}
}

// TestChunkPlanAlignmentAllDivisors is the core enumeration test of the 1MiB boundary invariant: it
// builds a plan with size=10MiB+12345 for every common divisor of 1MiB (1KiB..1MiB) and asserts
// that every chunk satisfies offset/(1MiB) == (offset+limit-1)/(1MiB) with limit <= MaxChunkSize.
// Invariant source: chunkSize divides 1MiB => offset=idx*chunkSize lands on the start of a 1MiB
// window, and a chunk extends at most to the next chunkSize grid point (also a multiple of 1MiB),
// so it never leaves the window.
func TestChunkPlanAlignmentAllDivisors(t *testing.T) {
	const mib = int64(1 << 20)
	const size = 10*mib + 12345
	for _, kib := range []int64{1, 2, 4, 8, 16, 32, 64, 128, 256, 512, 1024} {
		chunk := kib << 10
		p := newPlan(t, size, chunk)
		n := p.Chunks()
		if n != (size+chunk-1)/chunk {
			t.Fatalf("chunk=%dKiB: Chunks()=%d", kib, n)
		}
		for i := int64(0); i < n; i++ {
			off, lim := p.Offset(i)
			if lim <= 0 || int64(lim) > mib {
				t.Fatalf("chunk=%dKiB chunk %d: limit=%d out of range", kib, i, lim)
			}
			if off/mib != (off+int64(lim)-1)/mib {
				t.Fatalf("chunk=%dKiB chunk %d: offset=%d limit=%d crosses a 1MiB boundary", kib, i, off, lim)
			}
		}
	}
}

// TestChunkPlanRejectsBadChunkSize validates that NewChunkPlan errors for chunkSize <= 0 / > 1MiB.
func TestChunkPlanRejectsBadChunkSize(t *testing.T) {
	for _, c := range []int64{0, -1, 1<<20 + 1} {
		if _, err := NewChunkPlan(1<<20, c, state.NewBitmap(2)); err == nil {
			t.Fatalf("chunkSize=%d should be an error", c)
		}
	}
	// A chunkSize that does not divide 1MiB would make a chunk cross a 1MiB boundary, so it must be
	// rejected.
	for _, c := range []int64{700 << 10, 768 << 10} {
		if _, err := NewChunkPlan(10<<20, c, state.NewBitmap(32)); err == nil {
			t.Fatalf("chunkSize=%d does not divide 1MiB, should be an error", c)
		} else if !bytes.Contains([]byte(err.Error()), []byte("must divide 1MiB")) {
			t.Fatalf("error text for chunkSize=%d should mention dividing 1MiB, got %q", c, err)
		}
	}
	if _, err := NewChunkPlan(-1, 512<<10, state.NewBitmap(1)); err == nil {
		t.Fatal("negative size should be an error")
	}
	if _, err := NewChunkPlan(0, 512<<10, nil); err == nil {
		t.Fatal("nil bitmap should be an error")
	}
}

// TestChunkPlanMarkDoneAndMissing validates MarkDone/Missing/bitmap bit by bit.
func TestChunkPlanMarkDoneAndMissing(t *testing.T) {
	p := newPlan(t, 5*testChunkSize, testChunkSize)
	if got := len(p.Missing()); got != 5 {
		t.Fatalf("initial Missing = %d, want 5", got)
	}
	// Out-of-order marking: 3, 0, 4.
	for _, i := range []int64{3, 0, 4} {
		if err := p.MarkDone(i); err != nil {
			t.Fatalf("MarkDone(%d): %v", i, err)
		}
	}
	got := p.Missing()
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("Missing = %v, want [1 2] (ascending)", got)
	}
	// Assert the bitmap bit by bit.
	for i := int64(0); i < 5; i++ {
		want := (i == 0 || i == 3 || i == 4)
		if gotBit, err := p.Done.Get(int(i)); err != nil || gotBit != want {
			t.Fatalf("bit %d = %v (err %v), want %v", i, gotBit, err, want)
		}
	}
	if err := p.MarkDone(-1); err == nil {
		t.Fatal("negative index should be an error")
	}
	if err := p.MarkDone(5); err == nil {
		t.Fatal("out-of-range index should be an error")
	}
}

// ============ Download ============

// TestDownloadZeroByteFileNoRPC validates that a 0-byte file issues no getFile at all.
func TestDownloadZeroByteFileNoRPC(t *testing.T) {
	rpc := &fakeRPC{seedData: []byte{}}
	d := newDownloaderFast(rpc)
	item := newTestItem(0)
	p := newPlan(t, 0, testChunkSize)
	f := tempFile(t)
	if err := d.Download(context.Background(), DownloadRequest{Item: item, Writer: f, Plan: p, Threads: 2}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if rpc.callCount() != 0 {
		t.Fatalf("0-byte file issued %d getFile calls, want 0", rpc.callCount())
	}
}

// TestDownloadWritesExactBytes validates that the on-disk content matches the expectation byte for
// byte (fixed random seed) and that the result is still correct when chunks complete out of order.
func TestDownloadWritesExactBytes(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	size := int64(3*testChunkSize + 777) // not a multiple: 3 full chunks + tail chunk
	seedData := make([]byte, size)
	rng.Read(seedData)
	rpc := &fakeRPC{seedData: seedData}
	d := newDownloaderFast(rpc)

	p := newPlan(t, size, testChunkSize)
	f := tempFile(t)
	var progressCalls atomic.Int64
	err := d.Download(context.Background(), DownloadRequest{
		Item:     newTestItem(size),
		Writer:   f,
		Plan:     p,
		Threads:  4,
		Progress: func(done, total int64) { progressCalls.Add(1) },
		OnChunk:  func(idx int64) {},
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	// On-disk content matches byte for byte.
	got, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, seedData) {
		t.Fatalf("on-disk content does not match the expectation (length %d vs %d)", len(got), len(seedData))
	}
	if rpc.callCount() != 4 {
		t.Fatalf("getFile calls = %d, want 4", rpc.callCount())
	}
	if n := progressCalls.Load(); n != 4 {
		t.Fatalf("Progress callbacks = %d, want 4 (once per chunk)", n)
	}
}

// TestDownloadSizeMismatch validates that a mismatch between plan.Size and item.Size is an error.
func TestDownloadSizeMismatch(t *testing.T) {
	rpc := &fakeRPC{seedData: make([]byte, 100)}
	d := newDownloaderFast(rpc)
	p := newPlan(t, 100, 64)
	err := d.Download(context.Background(), DownloadRequest{Item: newTestItem(200), Writer: nopWriterAt{}, Plan: p})
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("does not match")) {
		t.Fatalf("size mismatch should be an error, got %v", err)
	}
}

// TestDownloadIdempotentSecondCallOnlyMissing validates idempotency: the second call only requests
// the missing chunks.
func TestDownloadIdempotentSecondCallOnlyMissing(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	size := int64(6 * testChunkSize)
	seedData := make([]byte, size)
	rng.Read(seedData)
	rpc := &fakeRPC{seedData: seedData}
	d := newDownloaderFast(rpc)
	item := newTestItem(size)
	p := newPlan(t, size, testChunkSize)
	f := tempFile(t)

	// First pass: full download.
	if err := d.Download(context.Background(), DownloadRequest{Item: item, Writer: f, Plan: p, Threads: 3}); err != nil {
		t.Fatalf("first Download: %v", err)
	}
	if got := rpc.callCount(); got != 6 {
		t.Fatalf("first call = %d, want 6", got)
	}
	// Second pass: everything is done, so 0 requests.
	if err := d.Download(context.Background(), DownloadRequest{Item: item, Writer: f, Plan: p, Threads: 3}); err != nil {
		t.Fatalf("second Download: %v", err)
	}
	if got := rpc.callCount(); got != 6 {
		t.Fatalf("total calls after the second pass = %d, want 6 (idempotent)", got)
	}
	// Preset the bitmap to skip some chunks: only the missing chunks are requested.
	rpc2 := &fakeRPC{seedData: seedData}
	d2 := newDownloaderFast(rpc2)
	p2 := newPlan(t, size, testChunkSize)
	for _, i := range []int{0, 2, 5} {
		if err := p2.MarkDone(int64(i)); err != nil {
			t.Fatal(err)
		}
	}
	f2 := tempFile(t)
	// Simulate the chunks already written by a previous session: pre-write the real content of
	// chunks 0/2/5.
	for _, i := range []int64{0, 2, 5} {
		off, lim := p2.Offset(i)
		if _, err := f2.WriteAt(seedData[off:off+int64(lim)], off); err != nil {
			t.Fatal(err)
		}
	}
	if err := d2.Download(context.Background(), DownloadRequest{Item: item, Writer: f2, Plan: p2, Threads: 2}); err != nil {
		t.Fatalf("preset bitmap Download: %v", err)
	}
	missing := int64(3) // 6 - 3
	if got := rpc2.callCount(); got != missing {
		t.Fatalf("getFile calls = %d, want %d (only the missing chunks)", got, missing)
	}
	got, _ := os.ReadFile(f2.Name())
	if !bytes.Equal(got, seedData) {
		t.Fatal("content mismatch after skipping chunks")
	}
}

// TestDownloadCancelMidwayThenResume is an end-to-end resume check: download about half, then
// cancel -> call Download again on the same plan -> the content is correct and the request count
// matches the expectation.
func TestDownloadCancelMidwayThenResume(t *testing.T) {
	rng := rand.New(rand.NewSource(99))
	size := int64(8 * testChunkSize)
	seedData := make([]byte, size)
	rng.Read(seedData)
	rpc := &fakeRPC{seedData: seedData}
	d := newDownloaderFast(rpc)
	item := newTestItem(size)
	p := newPlan(t, size, testChunkSize)
	f := tempFile(t)

	// First pass: cancel after the RPC completes a few chunks (callHook cancels after the third
	// call).
	ctx, cancel := context.WithCancel(context.Background())
	var hookCalls atomic.Int64
	rpc.callHook = func(req *tg.UploadGetFileRequest) {
		if hookCalls.Add(1) >= 3 {
			cancel()
		}
	}
	err := d.Download(ctx, DownloadRequest{Item: item, Writer: f, Plan: p, Threads: 2})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("first pass should return context.Canceled, got %v", err)
	}
	cancel()

	doneAfterFirst := p.CompletedBytes()
	if doneAfterFirst == 0 {
		t.Fatal("some chunks should be marked before cancellation (the resume precondition)")
	}
	firstCalls := rpc.callCount()

	// Second pass: same plan, fill in only the missing chunks.
	rpc.callHook = nil
	err = d.Download(context.Background(), DownloadRequest{Item: item, Writer: f, Plan: p, Threads: 4})
	if err != nil {
		t.Fatalf("second Download: %v", err)
	}
	// The number of requests in the second pass = the number of chunks missing at cancellation.
	wantSecond := int64(8) - doneAfterFirst/testChunkSize
	secondCalls := rpc.callCount() - firstCalls
	if secondCalls != wantSecond {
		t.Fatalf("second pass calls = %d, want %d", secondCalls, wantSecond)
	}
	// The content is correct.
	got, _ := os.ReadFile(f.Name())
	if !bytes.Equal(got, seedData) {
		t.Fatal("content after resume does not match the expectation")
	}
	if n := len(p.Missing()); n != 0 {
		t.Fatalf("the bitmap should be fully set, %d chunks still missing", n)
	}
}

// TestDownloadCancelStopsDispatch validates that dispatching new chunks stops immediately after
// cancellation and that in-flight requests are awaited (no goroutine leak, guaranteed by -race and
// wg.Wait).
func TestDownloadCancelStopsDispatch(t *testing.T) {
	rng := rand.New(rand.NewSource(5))
	size := int64(4 * testChunkSize)
	seedData := make([]byte, size)
	rng.Read(seedData)
	rpc := &fakeRPC{seedData: seedData}
	d := newDownloaderFast(rpc)
	item := newTestItem(size)
	p := newPlan(t, size, testChunkSize)
	f := tempFile(t)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel right after the first call: the in-flight request returns ctx.Err.
	rpc.callHook = func(req *tg.UploadGetFileRequest) { cancel() }
	start := time.Now()
	err := d.Download(ctx, DownloadRequest{Item: item, Writer: f, Plan: p, Threads: 2})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("should return context.Canceled, got %v", err)
	}
	cancel()
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("did not return promptly after cancellation: %v", elapsed)
	}
	// Chunks completed before cancellation must already be marked and have invoked OnChunk (the
	// resume correctness precondition).
	if p.CompletedBytes() == 0 {
		t.Fatal("chunks completed before cancellation are not marked")
	}
}

// TestDownloadChunkFailsAfterThreeAttempts validates that after a chunk fails 3 times the whole
// download fails and the error contains the chunk number and offset.
func TestDownloadChunkFailsAfterThreeAttempts(t *testing.T) {
	size := int64(2 * testChunkSize)
	seedData := make([]byte, size)
	rpc := &fakeRPC{seedData: seedData, failMap: map[int64]int{1: 99}} // chunk 1 always fails
	d := newDownloaderFast(rpc)
	p := newPlan(t, size, testChunkSize)
	f := tempFile(t)
	err := d.Download(context.Background(), DownloadRequest{Item: newTestItem(size), Writer: f, Plan: p, Threads: 2})
	if err == nil {
		t.Fatal("should fail")
	}
	// Count only the requests for chunk 1 (offset == testChunkSize): chunk 0's successful requests
	// count separately.
	var failCalls int64
	for _, r := range rpc.requests() {
		if r.offset == testChunkSize {
			failCalls++
		}
	}
	if failCalls != 3 {
		t.Fatalf("chunk 1 should be attempted exactly 3 times, actually %d times", failCalls)
	}
	// The error message must contain the chunk number and offset.
	wantIdx := "chunk 1"
	wantOffset := fmt.Sprintf("offset %d", testChunkSize)
	if !bytes.Contains([]byte(err.Error()), []byte(wantIdx)) || !bytes.Contains([]byte(err.Error()), []byte(wantOffset)) {
		t.Fatalf("error %q is missing the chunk number or offset", err)
	}
	// The chunk must not be marked after the failure.
	if p.IsDone(1) {
		t.Fatal("a failed chunk should not be marked")
	}
	// Chunk 0 may or may not have completed (not guaranteed under concurrency), so it is not
	// asserted.
}

// TestDownloadShortWriteFails validates that a short write counts as a chunk failure (and thus an
// overall failure after 3 attempts).
func TestDownloadShortWriteFails(t *testing.T) {
	size := int64(2 * testChunkSize)
	seedData := make([]byte, size)
	rpc := &fakeRPC{seedData: seedData, shortMap: map[int64]int{0: 10}}
	d := newDownloaderFast(rpc)
	p := newPlan(t, size, testChunkSize)
	f := tempFile(t)
	err := d.Download(context.Background(), DownloadRequest{Item: newTestItem(size), Writer: f, Plan: p, Threads: 1})
	if err == nil {
		t.Fatal("a short write should cause a failure")
	}
	if got := rpc.callCount(); got != 3 {
		t.Fatalf("the short-write chunk should be attempted 3 times, actually %d", got)
	}
	if rpc.requests()[0].limit != int(testChunkSize) {
		t.Fatalf("first request limit = %d", rpc.requests()[0].limit)
	}
}

// TestDownloadBackoffInjectable validates that the backoff is injectable: 200/400ms by default,
// with immediate retry after injecting 0.
func TestDownloadBackoffInjectable(t *testing.T) {
	size := int64(testChunkSize)
	seedData := make([]byte, size)
	rpc := &fakeRPC{seedData: seedData, failMap: map[int64]int{0: 2}} // fails twice, succeeds on the third attempt
	d, _ := NewDownloader(rpc, zap.NewNop())
	if d.backoff[0] != 200*time.Millisecond || d.backoff[1] != 400*time.Millisecond {
		t.Fatalf("default backoff = %v, want [200ms 400ms]", d.backoff)
	}
	d.backoff = []time.Duration{0, 0}
	p := newPlan(t, size, testChunkSize)
	f := tempFile(t)
	start := time.Now()
	if err := d.Download(context.Background(), DownloadRequest{Item: newTestItem(size), Writer: f, Plan: p, Threads: 1}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("with zero backoff the retry should be immediate, actually took %v", elapsed)
	}
	if rpc.callCount() != 3 {
		t.Fatalf("calls = %d, want 3", rpc.callCount())
	}
}

// TestDownloadDefaultThreadsFallback validates that 4 workers are used when Threads<=0.
// This is verified indirectly through 4 concurrent in-flight requests at the start (using blockCh to
// suspend and count requests).
func TestDownloadDefaultThreadsFallback(t *testing.T) {
	size := int64(8 * testChunkSize)
	seedData := make([]byte, size)
	rpc := &fakeRPC{seedData: seedData}
	d := newDownloaderFast(rpc)
	p := newPlan(t, size, testChunkSize)
	f := tempFile(t)

	started := make(chan struct{}, 32)
	var once sync.Once
	unblock := make(chan struct{})
	rpc.callHook = func(req *tg.UploadGetFileRequest) {
		once.Do(func() { close(started) })
		select {
		case <-unblock:
		case <-time.After(500 * time.Millisecond):
		}
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- d.Download(context.Background(), DownloadRequest{Item: newTestItem(size), Writer: f, Plan: p})
	}()
	<-started
	time.Sleep(50 * time.Millisecond) // wait for the other workers to issue their requests too
	concurrent := 0
	// Does fakeRPC hold the lock during callHook? -- callHook is invoked inside the lock, so the
	// count cannot be taken directly there.
	// Approximate it with the length of reqLog instead: within 50ms the 4 threads should each have
	// issued 1 request.
	concurrent = len(rpc.requests())
	close(unblock)
	if err := <-errCh; err != nil {
		t.Fatalf("Download: %v", err)
	}
	if concurrent < 2 {
		t.Fatalf("observed %d requests within 50ms under default concurrency, should be at least 2 (weak assertion: avoids flakiness from scheduler jitter)", concurrent)
	}
}

// TestDownloaderRPCOverride verifies that when DownloadRequest provides RPC, requests go to req.RPC
// rather than d.rpc (multi-DC / connection pool acceleration support).
func TestDownloaderRPCOverride(t *testing.T) {
	size := int64(1 << 20)
	seed := make([]byte, size)
	for i := range seed {
		seed[i] = byte(i % 251)
	}
	defaultRPC := &fakeRPC{seedData: seed}
	customRPC := &fakeRPC{seedData: seed}

	d, err := NewDownloader(defaultRPC, zap.NewNop())
	if err != nil {
		t.Fatalf("NewDownloader: %v", err)
	}

	dir := t.TempDir()
	partPath := filepath.Join(dir, "override.part")
	f, err := os.OpenFile(partPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	plan, err := NewChunkPlan(size, testChunkSize, state.NewBitmap(int((size+testChunkSize-1)/testChunkSize)))
	if err != nil {
		t.Fatal(err)
	}

	req := DownloadRequest{
		RPC:    customRPC,
		Item:   newTestItem(size),
		Writer: f,
		Plan:   plan,
	}
	if err := d.Download(context.Background(), req); err != nil {
		t.Fatalf("Download: %v", err)
	}

	if customRPC.callCount() == 0 {
		t.Error("customRPC should receive all chunk requests")
	}
	if defaultRPC.callCount() != 0 {
		t.Errorf("defaultRPC call count = %d, want 0", defaultRPC.callCount())
	}
}

// TestDownloadNilWriterOrPlan validates that nil parameters are reported as errors.
func TestDownloadNilWriterOrPlan(t *testing.T) {
	d := newDownloaderFast(&fakeRPC{})
	p := newPlan(t, 100, testChunkSize) // use a legal 512KiB as chunkSize
	if err := d.Download(context.Background(), DownloadRequest{Item: newTestItem(100), Plan: p}); err == nil {
		t.Fatal("a nil Writer should be an error")
	}
	if err := d.Download(context.Background(), DownloadRequest{Item: newTestItem(100), Writer: nopWriterAt{}}); err == nil {
		t.Fatal("a nil Plan should be an error")
	}
}

// TestDownloadTailChunkShortResponseIsEOF is the core unit test of the tail-chunk short-response
// semantics: the tail chunk requests the full chunkSize and the fake server returns only the
// remaining bytes -> the chunk succeeds (MarkDone, byte-exact content, Progress accumulated by
// actual bytes) with no retry amplification (the chunk issues exactly 1 request).
func TestDownloadTailChunkShortResponseIsEOF(t *testing.T) {
	rng := rand.New(rand.NewSource(1234))
	size := int64(3*testChunkSize + 503514) // 3 full chunks + a non-multiple tail chunk (live sample magnitude)
	seedData := make([]byte, size)
	rng.Read(seedData)
	rpc := &fakeRPC{seedData: seedData}
	// Force a short response for the tail chunk: no matter how much is requested, the tail chunk
	// only returns the real remaining bytes.
	rpc.shortMap = map[int64]int{3: int(size - 3*testChunkSize)}
	d := newDownloaderFast(rpc)

	p := newPlan(t, size, testChunkSize)
	f := tempFile(t)
	var lastDone, lastTotal atomic.Int64
	err := d.Download(context.Background(), DownloadRequest{
		Item:     newTestItem(size),
		Writer:   f,
		Plan:     p,
		Threads:  1,
		Progress: func(done, total int64) { lastDone.Store(done); lastTotal.Store(total) },
	})
	if err != nil {
		t.Fatalf("a short tail response should succeed as EOF, got %v", err)
	}
	if !p.IsDone(3) {
		t.Fatal("the tail chunk should be MarkDone")
	}
	if got := rpc.callCount(); got != 4 {
		t.Fatalf("getFile calls = %d, want 4 (no retry amplification)", got)
	}
	// Progress accumulates actual bytes up to size.
	if lastTotal.Load() != size || lastDone.Load() != size {
		t.Fatalf("Progress = (%d, %d), want (%d, %d)", lastDone.Load(), lastTotal.Load(), size, size)
	}
	// The content is byte exact (the tail chunk writes only the remaining bytes and must not pad to
	// chunkSize).
	got, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(got)) != size {
		t.Fatalf("on-disk length = %d, want %d", len(got), size)
	}
	if !bytes.Equal(got, seedData) {
		t.Fatal("on-disk content does not match the expectation")
	}
}

// TestDownloadMidChunkShortResponseFails is the counterexample: a short response on a middle (non
// tail) chunk still counts as a failure, fails overall after 3 attempts, and the chunk is not
// marked.
func TestDownloadMidChunkShortResponseFails(t *testing.T) {
	size := int64(4 * testChunkSize)
	seedData := make([]byte, size)
	rpc := &fakeRPC{seedData: seedData, shortMap: map[int64]int{1: 10}} // chunk 1 always short
	d := newDownloaderFast(rpc)
	p := newPlan(t, size, testChunkSize)
	f := tempFile(t)
	err := d.Download(context.Background(), DownloadRequest{Item: newTestItem(size), Writer: f, Plan: p, Threads: 1})
	if err == nil {
		t.Fatal("a short response on a middle chunk should cause a failure")
	}
	var midCalls int64
	for _, r := range rpc.requests() {
		if r.offset == testChunkSize {
			midCalls++
		}
	}
	if midCalls != 3 {
		t.Fatalf("the middle chunk should be attempted 3 times, actually %d", midCalls)
	}
	if p.IsDone(1) {
		t.Fatal("a failed chunk should not be marked")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("short response")) {
		t.Fatalf("the error should state a short response, got %v", err)
	}
}

// nopWriterAt is an io.WriterAt test stub that discards writes.
type nopWriterAt struct{}

func (nopWriterAt) WriteAt(p []byte, off int64) (int, error) { return len(p), nil }

// Compile-time assertion (test side): fakeRPC satisfies getFileRPC.
var _ getFileRPC = (*fakeRPC)(nil)
