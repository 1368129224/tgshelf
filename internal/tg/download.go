// download.go -- bitmap-driven chunked download core (own implementation).
//
// # Why not an off-the-shelf downloader
//
// Neither gotd's downloader.Parallel nor github.com/iyear/tdl/core/downloader can skip
// already-completed chunks, so they do not meet the resumable-download requirement. Hence this
// file implements its own core: it takes work items from ChunkPlan.Missing() and only requests
// the missing chunks.
//
// # Concurrency model
//
// N workers (N = req.Threads, default 4 when <=0) pull chunks from a buffered chan int64 task
// queue and complete independently in any order; progress is aggregated by "total bytes" (an
// atomic counter) and does not depend on completion order.
//
// # Failure semantics
//
//   - A chunk gets at most 3 attempts (1 initial + 2 retries) with exponential backoff of
//     200ms/400ms (the backoff field is injectable so tests can set it to 0).
//   - If any chunk still fails after 3 attempts: the abort flag is set to stop dispatching new
//     chunks, all in-flight requests are awaited (no goroutine leak), and the overall error
//     includes the chunk number and offset.
//   - ctx cancellation: workers check ctx before taking a new task; fetchChunk's backoff and RPC
//     waits both honor ctx; the error returned by Download can be tested with
//     errors.Is(err, context.Canceled).
//
// # FILE_REFERENCE expiry refresh
//
// FILE_REFERENCE_EXPIRED / FILE_REFERENCE_INVALID / FILEREF_UPGRADE_NEEDED are unavoidable during
// long downloads. When a chunk exhausts its retries with a file-reference error and req.Refresh is
// non-nil: the Refresher (singleflight semantics) re-fetches the message serially to obtain a new
// Location, and on success **only the current chunk is retried** -- the bitmap of completed chunks
// stays untouched, neither cleared nor re-downloaded. When Refresh is nil the error is treated as
// an ordinary failure (no panic).
//
// # Disk write contract
//
// After writing with req.Writer.WriteAt(buf, offset) the chunk is marked done and OnChunk is
// invoked -- this is where the resume precondition "chunks completed before cancellation must
// already be marked" is implemented. Requests always use the full chunkSize (the server only
// accepts aligned powers of two as limit); a short response is the EOF semantics:
// offset+len(buf) >= plan.Size means the tail chunk succeeded (only the actual bytes are written),
// otherwise it counts as a failure and goes into retry.
//
// The lifecycle of the .part file (open/truncate/fsync/rename) is owned by state.Session; this
// core only writes through io.WriterAt and never opens files itself.
package tg

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// errShortResponse is the retryable error sentinel for "short response on a middle chunk":
// getOnce detects it, fetchChunk catches it and retries, and downloadChunk wraps it up (overall
// failure once the 3 attempts are exhausted).
type errShortResponse struct {
	got, want int
}

func (e *errShortResponse) Error() string {
	return fmt.Sprintf("tg: short response, got %d/%d bytes", e.got, e.want)
}

// getFileRPC is the narrow interface of the download core towards the Telegram RPC.
// *tg.Client satisfies it naturally; tests inject a fake.
type getFileRPC interface {
	UploadGetFile(ctx context.Context, req *tg.UploadGetFileRequest) (tg.UploadFileClass, error)
}

// Compile-time assertion: the production implementation *tg.Client satisfies getFileRPC.
var _ getFileRPC = (*tg.Client)(nil)

// defaultDownloadThreads is the default concurrency when Threads <= 0.
const defaultDownloadThreads = 4

// chunkMaxRetries is the maximum number of attempts per chunk (1 initial + 2 retries).
const chunkMaxRetries = 3

// defaultChunkBackoff is the retry backoff sequence per chunk: 200ms, 400ms (exponential).
var defaultChunkBackoff = []time.Duration{200 * time.Millisecond, 400 * time.Millisecond}

// ProgressFn is called once per completed chunk (done is the aggregated number of completed
// bytes, total is the total file size). Throttling is the caller's responsibility.
type ProgressFn func(done, total int64)

// ChunkDoneFn is called exactly once per chunk after it is written to disk (state.Session uses it to chain
// MarkDone+Flush).
type ChunkDoneFn func(idx int64)

// DownloadRequest holds every parameter of one chunked download.
type DownloadRequest struct {
	// RPC is optional: it selects the RPC interface used for this download (overriding
	// Downloader.rpc, used for multi-connection pools and multi-DC acceleration). When nil it
	// falls back to Downloader.rpc.
	RPC getFileRPC
	// Item is the target media (provides the initial Location and Size).
	Item MediaItem
	// Writer is the already-open .part file (WriteAt on *os.File is concurrency-safe).
	Writer io.WriterAt
	// Plan is the chunk plan (with the completion bitmap).
	Plan *ChunkPlan
	// Threads is the number of concurrent workers; 4 when <=0.
	Threads int
	// Progress is called once after each chunk completes; may be nil.
	Progress ProgressFn
	// OnChunk is called exactly once after each chunk is written to disk; may be nil.
	OnChunk ChunkDoneFn
	// Refresh re-fetches the message and returns a new file location (filled from
	// Pipeline.ResolveLink). May be nil: when nil, an expired reference fails as an
	// ordinary error.
	Refresh RefreshFunc
}

// Downloader is the self-written chunked download core.
type Downloader struct {
	rpc     getFileRPC
	logger  *zap.Logger
	backoff []time.Duration
}

// NewDownloader creates a download core.
//
// rpc must not be nil; logger may be nil (internally degrades to zap.NewNop()).
func NewDownloader(rpc getFileRPC, logger *zap.Logger) (*Downloader, error) {
	if rpc == nil {
		return nil, errors.New("tg: download core requires a non-nil getFileRPC")
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Downloader{rpc: rpc, logger: logger, backoff: defaultChunkBackoff}, nil
}

// Download downloads the missing chunks of req.Item according to the plan and writes them to
// req.Writer.
//
// Idempotent: it only issues getFile for plan.Missing(); calling it again on the same plan fills in
// just the missing chunks (the core assertion of resumable downloads). A mismatch between
// Item.Size and plan.Size is an error; a 0-byte file issues no getFile at all and succeeds
// immediately.
func (d *Downloader) Download(ctx context.Context, req DownloadRequest) error {
	if req.Writer == nil {
		return errors.New("tg: DownloadRequest.Writer is nil")
	}
	if req.Plan == nil {
		return errors.New("tg: DownloadRequest.Plan is nil")
	}
	if req.Item.Size != req.Plan.Size {
		return fmt.Errorf("tg: Item.Size %d does not match Plan.Size %d", req.Item.Size, req.Plan.Size)
	}
	if req.Item.Location == nil && len(req.Plan.Missing()) > 0 {
		return errors.New("tg: MediaItem.Location is nil")
	}

	missing := req.Plan.Missing()
	if len(missing) == 0 {
		return nil
	}

	threads := req.Threads
	if threads <= 0 {
		threads = defaultDownloadThreads
	}
	if threads > len(missing) {
		threads = len(missing)
	}

	// Progress aggregation: the initial completed byte count comes from the bitmap (order does not
	// matter); from then on every completed chunk adds atomically.
	var doneBytes atomic.Int64
	doneBytes.Store(req.Plan.CompletedBytes())
	total := req.Plan.Size

	dlCtx, dlCancel := context.WithCancel(ctx)
	defer dlCancel()

	var (
		abort    atomic.Bool // set once any chunk ultimately fails, stops dispatching new chunks
		errMu    sync.Mutex
		firstErr error
	)

	setErr := func(idx, offset int64, err error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = fmt.Errorf("tg: chunk %d at offset %d failed: %w", idx, offset, asFileRefErr(err))
		}
		errMu.Unlock()
		abort.Store(true)
		dlCancel()
	}

	// File reference refresher: one per file (counters are isolated per file); when Refresh is nil
	// the refresh path inside the workers short-circuits to an ordinary error.
	var ref *Refresher
	if req.Refresh != nil {
		ref = NewRefresher(req.Refresh, d.logger)
		ref.seed(req.Item.Location)
	}

	tasks := make(chan int64, len(missing))
	for _, idx := range missing {
		tasks <- idx
	}
	close(tasks)

	var wg sync.WaitGroup
	for w := 0; w < threads; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range tasks {
				// ctx already canceled or a failure already happened: dispatch no new chunk.
				if dlCtx.Err() != nil || abort.Load() {
					return
				}
				offset, limit := req.Plan.Offset(idx)
				if limit <= 0 {
					setErr(idx, offset, fmt.Errorf("tg: illegal limit %d for chunk %d", limit, idx))
					return
				}
				if err := d.downloadChunk(dlCtx, req, ref, &doneBytes, total, idx, offset, limit, &abort, setErr); err != nil {
					return
				}
			}
		}()
	}

	// Wait for every in-flight request to return so no goroutine leaks.
	wg.Wait()

	if err := ctx.Err(); err != nil {
		return err // source for errors.Is(err, context.Canceled)
	}
	errMu.Lock()
	defer errMu.Unlock()
	return firstErr
}

// downloadChunk downloads a single chunk: up to 3 attempts with backoff; on a file-reference error
// it refreshes serially to obtain a new Location and **retries only the current chunk** (the bitmap
// of completed chunks is left alone). A non-nil return means this chunk or the whole download has
// failed and the worker should exit.
func (d *Downloader) downloadChunk(
	ctx context.Context,
	req DownloadRequest,
	ref *Refresher,
	doneBytes *atomic.Int64,
	total int64,
	idx, offset int64,
	limit int,
	abort *atomic.Bool,
	setErr func(idx, offset int64, err error),
) error {
	// cur holds the currently effective Location locally; gen0 is the generation snapshot taken
	// when the first request was issued: on failure, if gen has already advanced (another worker
	// refreshed successfully while this request was in flight) the new Location is reused directly
	// and no redundant Refresh is triggered.
	rpc := d.rpc
	if req.RPC != nil {
		rpc = req.RPC
	}
	cur := req.Item.Location
	var gen0 int
	if ref != nil {
		cur = ref.Current()
		gen0 = ref.Gen()
	}
	buf, err := d.fetchChunk(ctx, rpc, cur, offset, limit, req.Plan.Size)
	for {
		if err == nil {
			return d.finishChunk(req, doneBytes, total, abort, setErr, idx, offset, limit, buf)
		}
		if ctx.Err() != nil {
			return err // canceled: let Download return ctx.Err() uniformly
		}
		if !isFileRefErr(err) || ref == nil {
			// Ordinary error (or no refresher): the whole download fails.
			abort.Store(true)
			setErr(idx, offset, err)
			return err
		}

		if ref.Gen() > gen0 {
			// Generation short-circuit: another worker already refreshed successfully, so reuse the
			// new Location directly.
			cur = ref.Current()
		} else {
			newLoc, rerr := ref.Refresh(ctx)
			if rerr != nil {
				if ctx.Err() != nil {
					return rerr
				}
				if errors.Is(rerr, ErrRefreshTooFrequent) {
					// Rate limit/limit hit: the whole file fails, no more looping.
					abort.Store(true)
					setErr(idx, offset, rerr)
					return rerr
				}
				// Non-rate-limited refresh failure: keep the old Location and retry the current
				// chunk; the refresher's cumulative attempt cap guarantees the loop terminates.
			} else {
				cur = newLoc
			}
		}
		gen0 = ref.Gen()
		d.logger.Info("tg: file reference updated, retrying current chunk",
			zap.Int64("chunk", idx), zap.Int64("offset", offset))

		buf, err = d.fetchChunk(ctx, rpc, cur, offset, limit, req.Plan.Size)
	}
}

// finishChunk writes to disk, marks done and invokes callbacks; returns nil on success.
func (d *Downloader) finishChunk(
	req DownloadRequest,
	doneBytes *atomic.Int64,
	total int64,
	abort *atomic.Bool,
	setErr func(idx, offset int64, err error),
	idx, offset int64,
	limit int,
	buf []byte,
) error {
	// EOF semantics: plan.Size is the authoritative length. Requests always use the full
	// chunkSize and the server returns only the remaining bytes for the tail chunk -- so
	// offset+len(buf) >= plan.Size means the end of the file is reached and counts as success
	// (only the actual bytes are written); a short response on a middle chunk was already detected
	// as errShortResponse in getOnce and retried by fetchChunk, so it never reaches here (the
	// defensive branch stays: excess bytes are never written to disk).
	if len(buf) > limit {
		buf = buf[:limit]
	}
	if _, err := req.Writer.WriteAt(buf, offset); err != nil {
		// Retrying a disk write is of limited use (a disk-state problem), so abort the whole
		// download.
		abort.Store(true)
		setErr(idx, offset, fmt.Errorf("tg: WriteAt: %w", err))
		return err
	}
	// Mark done + callback (the precondition for resume correctness).
	if err := req.Plan.MarkDone(idx); err != nil {
		abort.Store(true)
		setErr(idx, offset, fmt.Errorf("tg: MarkDone: %w", err))
		return err
	}
	doneBytes.Add(int64(len(buf)))
	if req.Progress != nil {
		req.Progress(doneBytes.Load(), total)
	}
	if req.OnChunk != nil {
		req.OnChunk(idx)
	}
	return nil
}

// errFileRef wraps a file-reference RPC error: the error chain also carries ErrFileRefExpired so
// callers can test it with errors.Is (tgerr.Error is not itself a sentinel, so an explicit bridge
// is needed).
type errFileRef struct {
	rpc *tgerr.Error
}

func (e *errFileRef) Error() string   { return e.rpc.Error() }
func (e *errFileRef) Unwrap() []error { return []error{e.rpc, ErrFileRefExpired} }

// asFileRefErr wraps a file-reference RPC error into one that supports errors.Is; non
// file-reference errors are returned unchanged.
func asFileRefErr(err error) error {
	if rpcErr, ok := tgerr.As(err); ok && isFileRefErr(rpcErr) {
		return &errFileRef{rpc: rpcErr}
	}
	return err
}

// fetchChunk requests a single chunk with at most 3 attempts (1 initial + 2 retries) and
// exponential backoff. ctx is checked before every attempt and the backoff wait honors ctx.
func (d *Downloader) fetchChunk(ctx context.Context, rpc getFileRPC, loc tg.InputFileLocationClass, offset int64, limit int, size int64) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < chunkMaxRetries; attempt++ {
		if attempt > 0 {
			delay := d.backoff[attempt-1]
			if delay > 0 {
				select {
				case <-time.After(delay):
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			} // delay <= 0: test fast path, retry immediately
		}
		buf, err := d.getOnce(ctx, rpc, loc, offset, limit, size)
		if err == nil {
			return buf, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var short *errShortResponse
		if errors.As(err, &short) {
			// Short response on a middle chunk: take the normal retry path (downloadChunk wraps up
			// after the 3 attempts are exhausted).
			d.logger.Warn("tg: short response for chunk, about to retry",
				zap.Int64("offset", offset),
				zap.Int("limit", limit),
				zap.Int("attempt", attempt+1),
				zap.Error(err),
			)
			continue
		}
		if isFileRefErr(err) {
			// Expired file reference: retrying with the same expired reference is pointless, so
			// hand it up immediately to the refresh path (it does not consume the chunk's remaining
			// attempts).
			d.logger.Warn("tg: file reference expired, skipping remaining retries for this chunk",
				zap.Int64("offset", offset), zap.Int("limit", limit))
			return nil, asFileRefErr(err)
		}
		d.logger.Warn("tg: chunk request failed, about to retry",
			zap.Int64("offset", offset),
			zap.Int("limit", limit),
			zap.Int("attempt", attempt+1),
			zap.Error(err),
		)
	}
	return nil, lastErr
}

// getOnce performs one UploadGetFile call and validates the response type.
//
// A short response (len(file.Bytes) < limit) is **not** an error: when the tail chunk requests the
// full chunkSize the server returns exactly the remaining bytes (confirmed by a live probe + the
// authoritative approach of gotd's downloader: treat len(data) < partSize as EOF). The EOF verdict
// belongs to finishChunk and is based on plan.Size.
// Precise is always set (relaxing the alignment constraint from 4KiB to 1KiB); note that it does
// not fix the tail chunk -- the power-of-two constraint still comes from the path without precise,
// and a shortened value (e.g. 503514) is not a multiple of 1024 either.
func (d *Downloader) getOnce(ctx context.Context, rpc getFileRPC, loc tg.InputFileLocationClass, offset int64, limit int, size int64) ([]byte, error) {
	if rpc == nil {
		rpc = d.rpc
	}
	resp, err := rpc.UploadGetFile(ctx, &tg.UploadGetFileRequest{
		Location: loc,
		Offset:   offset,
		Limit:    limit,
		Precise:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("UploadGetFile: %w", err)
	}
	file, ok := resp.(*tg.UploadFile)
	if !ok {
		return nil, fmt.Errorf("tg: unexpected UploadGetFile response type %T", resp)
	}
	// A short response on a middle chunk (not enough to fill it up to the end of the file) is still
	// a retryable error; a short response on the tail chunk (exactly reaching EOF) is judged a
	// success by finishChunk based on plan.Size.
	if n := len(file.Bytes); n < limit && offset+int64(n) < size {
		return nil, &errShortResponse{got: n, want: limit}
	}
	return file.Bytes, nil
}
