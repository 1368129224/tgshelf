package tg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// --- test fixtures ---

// refRPC layers a "file reference expired" scenario on top of fakeRPC: while expired
// is set every UploadGetFile returns tgerr.New(400, "FILE_REFERENCE_EXPIRED"); every
// successful request records the FileReference of the Location it used, so tests can
// assert the switch from the old reference to the new one.
type refRPC struct {
	fakeRPC
	expired  atomic.Bool
	mu       sync.Mutex
	seenRefs [][]byte
}

func (r *refRPC) UploadGetFile(ctx context.Context, req *tg.UploadGetFileRequest) (tg.UploadFileClass, error) {
	if r.expired.Load() {
		r.mu.Lock()
		r.calls++
		r.reqLog = append(r.reqLog, reqRec{offset: req.Offset, limit: req.Limit})
		r.mu.Unlock()
		return nil, tgerr.New(400, "FILE_REFERENCE_EXPIRED")
	}
	res, err := r.fakeRPC.UploadGetFile(ctx, req)
	if err == nil {
		r.mu.Lock()
		if l, ok := req.Location.(*tg.InputDocumentFileLocation); ok {
			r.seenRefs = append(r.seenRefs, l.FileReference)
		}
		r.mu.Unlock()
	}
	return res, err
}

func (r *refRPC) successRefs() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]byte, len(r.seenRefs))
	copy(out, r.seenRefs)
	return out
}

// locWithRef copies item.Location and replaces its FileReference.
func locWithRef(item MediaItem, ref []byte) tg.InputFileLocationClass {
	switch l := item.Location.(type) {
	case *tg.InputDocumentFileLocation:
		c := *l
		c.FileReference = ref
		return &c
	case *tg.InputFileLocation:
		c := *l
		c.FileReference = ref
		return &c
	default:
		panic(fmt.Sprintf("unsupported Location type %T", item.Location))
	}
}

// rand3 is a seeded random source (keeps runs deterministic).
func rand3() *rand.Rand { return rand.New(rand.NewSource(2024)) }

// fastRefresh zeroes the cooldown and wait windows so tests run instantly.
func fastRefresh(r *Refresher) { r.cooldown = 0; r.staleWait = 0 }

// ============ expiry -> one refresh -> retry only the current chunk -> correct content ============

// TestDownloadRefreshExpiredOnceThenSucceed: the first request of chunk 1 returns
// FILE_REFERENCE_EXPIRED and is retried after Refresh returns a new reference;
// asserts: exactly one refresh, total getFile requests == missing chunks + extra
// retries, completed chunks are not downloaded again, the reference switches from the
// old one to the new one, and the final content is correct byte for byte.
func TestDownloadRefreshExpiredOnceThenSucceed(t *testing.T) {
	rng := rand3()
	size := int64(4 * testChunkSize)
	seedData := make([]byte, size)
	rng.Read(seedData)
	rpc := &refRPC{fakeRPC: fakeRPC{seedData: seedData}}
	rpc.expired.Store(true) // the first request expires; the refresh function flips this back
	d := newDownloaderFast(rpc)

	item := newTestItem(size)
	var refreshCalls atomic.Int64
	newRef := []byte{9, 9, 9}
	p := newPlan(t, size, testChunkSize)
	f := tempFile(t)

	// Pre-complete chunk 0: write the real content and mark it done (asserts no
	// re-download after the refresh).
	if _, err := f.WriteAt(seedData[0:testChunkSize], 0); err != nil {
		t.Fatal(err)
	}
	if err := p.MarkDone(0); err != nil {
		t.Fatal(err)
	}

	err := d.Download(context.Background(), DownloadRequest{
		Item:    item,
		Writer:  f,
		Plan:    p,
		Threads: 1, // serial: chunk 1 is guaranteed to hit expiry first and be retried after the refresh
		Refresh: func(ctx context.Context) (tg.InputFileLocationClass, error) {
			refreshCalls.Add(1)
			rpc.expired.Store(false)
			return locWithRef(item, newRef), nil
		},
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
	// Total getFile requests = missing chunks (3) + extra retries (1) = 4.
	if got := rpc.callCount(); got != 4 {
		t.Fatalf("total getFile requests = %d, want 4 (3 missing + 1 retry)", got)
	}
	// Completed chunk 0 is never resent.
	for _, r := range rpc.requests() {
		if r.offset == 0 {
			t.Fatalf("completed chunk 0 was resent: %+v", r)
		}
	}
	// Reference switch assertion: the first request (with the old reference) expires and
	// every successful request afterwards carries the new reference.
	// (The expired request itself produces no data and the old reference only appears in
	// failed requests, so all successful refs should be the new one.)
	reqs := rpc.requests()
	if len(reqs) == 0 || reqs[0].offset != testChunkSize {
		t.Fatalf("the first request should be chunk 1, got %+v", reqs)
	}
	refs := rpc.successRefs()
	if len(refs) == 0 {
		t.Fatal("no successful request to assert the reference on")
	}
	for i, ref := range refs {
		if !bytes.Equal(ref, newRef) {
			t.Fatalf("successful request %d did not use the new reference: %v", i, ref)
		}
	}
	// Final content is correct byte for byte.
	got, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, seedData) {
		t.Fatalf("content mismatch (len %d vs %d)", len(got), len(seedData))
	}
	if n := len(p.Missing()); n != 0 {
		t.Fatalf("the bitmap should be fully set, %d chunks still missing", n)
	}
}

// ============ three refresh failures in a row -> the whole file fails, detectable with errors.Is ============

// TestDownloadRefreshExhaustedFailsWholeFile: RefreshFunc returns a different new
// reference every time (the refresh "succeeds"), but the server keeps returning expiry
// (expired stays true) - simulating the extreme case where refreshing never helps:
// once the 3 refresh budget is spent the rate limiter refuses and the whole file fails.
// Asserts: exactly 3 fn calls, errors.Is(err, ErrFileRefExpired), and the error message
// carries the refresh count.
func TestDownloadRefreshExhaustedFailsWholeFile(t *testing.T) {
	size := int64(2 * testChunkSize)
	seedData := make([]byte, size)
	rpc := &refRPC{fakeRPC: fakeRPC{seedData: seedData}}
	rpc.expired.Store(true) // the server keeps refusing (still expired after the refresh)
	d := newDownloaderFast(rpc)
	item := newTestItem(size)
	p := newPlan(t, size, testChunkSize)
	f := tempFile(t)

	var refreshCalls atomic.Int64
	err := d.Download(context.Background(), DownloadRequest{
		Item:    item,
		Writer:  f,
		Plan:    p,
		Threads: 1,
		Refresh: func(ctx context.Context) (tg.InputFileLocationClass, error) {
			n := refreshCalls.Add(1)
			return locWithRef(item, []byte{byte(n)}), nil // "succeeds" with a new reference every time
		},
	})
	if err == nil {
		t.Fatal("should fail")
	}
	if !errors.Is(err, ErrFileRefExpired) {
		t.Fatalf("the error should match ErrFileRefExpired via errors.Is, got %v", err)
	}
	if refreshCalls.Load() != 3 {
		t.Fatalf("refresh calls = %d, want 3", refreshCalls.Load())
	}
	if !bytes.Contains([]byte(err.Error()), []byte("3 times")) {
		t.Fatalf("the error message should carry the refresh count, got %v", err)
	}
	// Failed chunks must not be marked done.
	if p.IsDone(1) {
		t.Fatal("a failed chunk must not be marked done")
	}
}

// ============ 8 concurrent workers hit expiry at once -> RefreshFunc call count == 1 ============

// TestDownloadConcurrentRefreshSingleFlight: all 8 workers hit expiry;
// asserts the RefreshFunc call count == 1, that every chunk finishes and that the
// content is correct.
func TestDownloadConcurrentRefreshSingleFlight(t *testing.T) {
	rng := rand3()
	size := int64(8 * testChunkSize)
	seedData := make([]byte, size)
	rng.Read(seedData)
	rpc := &refRPC{fakeRPC: fakeRPC{seedData: seedData}}
	rpc.expired.Store(true) // every request expires until the refresh flips this back
	d := newDownloaderFast(rpc)
	item := newTestItem(size)
	newRef := []byte{7, 7, 7}
	p := newPlan(t, size, testChunkSize)
	f := tempFile(t)

	var refreshCalls atomic.Int64
	err := d.Download(context.Background(), DownloadRequest{
		Item:    item,
		Writer:  f,
		Plan:    p,
		Threads: 8,
		Refresh: func(ctx context.Context) (tg.InputFileLocationClass, error) {
			refreshCalls.Add(1)
			time.Sleep(10 * time.Millisecond) // the other workers arrive and wait during the first refresh
			rpc.expired.Store(false)
			return locWithRef(item, newRef), nil
		},
	})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls with 8 concurrent workers = %d, want 1", got)
	}
	if n := len(p.Missing()); n != 0 {
		t.Fatalf("the bitmap should be fully set, %d chunks still missing", n)
	}
	got, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, seedData) {
		t.Fatal("content mismatch")
	}
}

// ============ with Refresh == nil an expiry fails as an ordinary error (no panic) ============

// TestDownloadNilRefreshFailsOnExpired: with Refresh nil an expiry error fails the
// whole download.
func TestDownloadNilRefreshFailsOnExpired(t *testing.T) {
	size := int64(2 * testChunkSize)
	seedData := make([]byte, size)
	rpc := &refRPC{fakeRPC: fakeRPC{seedData: seedData}}
	rpc.expired.Store(true)
	d := newDownloaderFast(rpc)
	p := newPlan(t, size, testChunkSize)
	f := tempFile(t)
	err := d.Download(context.Background(), DownloadRequest{
		Item:    newTestItem(size),
		Writer:  f,
		Plan:    p,
		Threads: 1,
	})
	if err == nil {
		t.Fatal("with Refresh nil an expiry should fail")
	}
	if !errors.Is(err, ErrFileRefExpired) {
		t.Fatalf("the error chain should match ErrFileRefExpired via errors.Is, got %v", err)
	}
	if p.IsDone(1) {
		t.Fatal("a failed chunk must not be marked done")
	}
}

// ============ rate limit: cumulative cap of 3 ============

// TestRefresherLimitAfterThreeCalls: after 3 cumulative refreshes the 4th is refused by
// the rate limiter, and the error matches ErrRefreshTooFrequent and ErrFileRefExpired
// with errors.Is.
func TestRefresherLimitAfterThreeCalls(t *testing.T) {
	item := newTestItem(100)
	var calls atomic.Int64
	r := NewRefresher(func(ctx context.Context) (tg.InputFileLocationClass, error) {
		calls.Add(1)
		return locWithRef(item, []byte{byte(calls.Load())}), nil
	}, zap.NewNop())
	fastRefresh(r)

	ctx := context.Background()
	for i := 0; i < 3; i++ {
		loc, err := r.Refresh(ctx)
		if err != nil {
			t.Fatalf("refresh %d should not fail: %v", i+1, err)
		}
		if loc == nil {
			t.Fatalf("refresh %d returned nil", i+1)
		}
	}
	_, err := r.Refresh(ctx)
	if !errors.Is(err, ErrRefreshTooFrequent) {
		t.Fatalf("the 4th refresh should be rate limited, got %v", err)
	}
	if !errors.Is(err, ErrFileRefExpired) {
		t.Fatalf("the rate limit error should also wrap ErrFileRefExpired, got %v", err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("RefreshFunc calls = %d, want 3", got)
	}
}

// TestRefresherCurrentTracksLatest: after a successful refresh Current() returns the
// newest Location (gen advanced), for the remaining chunks of a Download.
func TestRefresherCurrentTracksLatest(t *testing.T) {
	item := newTestItem(100)
	var calls atomic.Int64
	r := NewRefresher(func(ctx context.Context) (tg.InputFileLocationClass, error) {
		calls.Add(1)
		return locWithRef(item, []byte{byte(calls.Load())}), nil
	}, zap.NewNop())
	fastRefresh(r)
	r.seed(item.Location)
	if got := r.Gen(); got != 0 {
		t.Fatalf("initial gen = %d, want 0", got)
	}
	loc, err := r.Refresh(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Gen() != 1 {
		t.Fatalf("after refresh gen = %d, want 1", r.Gen())
	}
	l, ok := loc.(*tg.InputDocumentFileLocation)
	if !ok || !bytes.Equal(l.FileReference, []byte{1}) {
		t.Fatalf("Refresh should return the new reference, got %#v", loc)
	}
	cur, ok := r.Current().(*tg.InputDocumentFileLocation)
	if !ok || !bytes.Equal(cur.FileReference, []byte{1}) {
		t.Fatalf("Current should be the new reference, got %#v", r.Current())
	}
}

// TestRefresherNilLocationFails: fn returning a nil Location counts as a failed
// refresh (and consumes the budget); the error matches ErrFileRefExpired with
// errors.Is.
func TestRefresherNilLocationFails(t *testing.T) {
	r := NewRefresher(func(ctx context.Context) (tg.InputFileLocationClass, error) {
		return nil, nil
	}, zap.NewNop())
	fastRefresh(r)
	_, err := r.Refresh(context.Background())
	if err == nil {
		t.Fatal("a nil Location should count as a failed refresh")
	}
	if !errors.Is(err, ErrFileRefExpired) {
		t.Fatalf("the error should wrap ErrFileRefExpired, got %v", err)
	}
}

// TestRefresherFollowerCancel: a waiting follower can be cancelled through ctx without
// leaking.
func TestRefresherFollowerCancel(t *testing.T) {
	item := newTestItem(100)
	gate := make(chan struct{})
	r := NewRefresher(func(ctx context.Context) (tg.InputFileLocationClass, error) {
		<-gate
		return locWithRef(item, []byte{1}), nil
	}, zap.NewNop())
	r.seed(item.Location)

	// The leader enters the hanging fn.
	go func() { _, _ = r.Refresh(context.Background()) }()
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Refresh(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("the follower should return ctx.Err, got %v", err)
	}
	close(gate)
}

// TestRefresherSameLocationDetected: same-reference detection driven directly (without
// going through Download).
func TestRefresherSameLocationDetected(t *testing.T) {
	item := newTestItem(100)
	var calls atomic.Int64
	r := NewRefresher(func(ctx context.Context) (tg.InputFileLocationClass, error) {
		calls.Add(1)
		return locWithRef(item, item.FileRef), nil // always returns the old reference
	}, zap.NewNop())
	fastRefresh(r)
	r.seed(item.Location)
	_, err := r.Refresh(context.Background())
	if err == nil {
		t.Fatal("a same-reference refresh should fail")
	}
	if !errors.Is(err, ErrFileRefExpired) {
		t.Fatalf("the error should wrap ErrFileRefExpired, got %v", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("same-reference detection should trigger a wait-and-retry (2 fn calls), got %d", got)
	}
}

// TestRefresherConcurrentCurrentRace: concurrent refreshes plus Current reads (-race
// smoke test).
func TestRefresherConcurrentCurrentRace(t *testing.T) {
	item := newTestItem(100)
	r := NewRefresher(func(ctx context.Context) (tg.InputFileLocationClass, error) {
		return locWithRef(item, []byte{1}), nil
	}, zap.NewNop())
	fastRefresh(r)
	r.seed(item.Location)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = r.Refresh(context.Background())
			_ = r.Current()
			_ = r.Gen()
		}()
	}
	wg.Wait()
}

// TestIsFileRefErrByCode: the file reference error class is decided from the tgerr code
// (not by string containment).
func TestIsFileRefErrByCode(t *testing.T) {
	for _, code := range []string{"FILE_REFERENCE_EXPIRED", "FILE_REFERENCE_INVALID", "FILEREF_UPGRADE_NEEDED"} {
		if !isFileRefErr(tgerr.New(400, code)) {
			t.Fatalf("%s should be recognized as a file reference error", code)
		}
	}
	if isFileRefErr(tgerr.New(400, "FLOOD_WAIT_3")) {
		t.Fatal("FLOOD_WAIT should not be recognized as a file reference error")
	}
	if isFileRefErr(errors.New("FILE_REFERENCE_EXPIRED literal")) {
		t.Fatal("a plain error that merely contains the string must not be recognized (tgerr detection is required)")
	}
}

// TestAsFileRefErrBridge: an ordinary RPC expiry error wrapped by asFileRefErr matches
// ErrFileRefExpired with errors.Is; errors that are not file reference errors are
// returned unchanged.
func TestAsFileRefErrBridge(t *testing.T) {
	e := asFileRefErr(tgerr.New(400, "FILE_REFERENCE_EXPIRED"))
	if !errors.Is(e, ErrFileRefExpired) {
		t.Fatalf("after wrapping it should match ErrFileRefExpired via errors.Is, got %v", e)
	}
	var rpcE *tgerr.Error
	if !errors.As(e, &rpcE) || rpcE.Type != "FILE_REFERENCE_EXPIRED" {
		t.Fatalf("the original RPC error should stay in the error chain, got %v", e)
	}
	other := asFileRefErr(tgerr.New(400, "FLOOD_WAIT_3"))
	if errors.Is(other, ErrFileRefExpired) {
		t.Fatal("a non file reference error must not be wrapped")
	}
	plain := asFileRefErr(errors.New("plain"))
	if plain == nil || plain.Error() != "plain" {
		t.Fatalf("an ordinary error should be returned unchanged, got %v", plain)
	}
}

// Compile-time assertion: tgerr.Is is used for file reference error detection (a guard
// against degrading to string containment).
var _ = tgerr.Is
