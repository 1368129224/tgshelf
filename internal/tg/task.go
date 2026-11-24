// task.go - task orchestration: disk precheck -> file-level concurrency -> each file goes
// through Downloader + Session -> progress aggregation -> failure isolation -> state machine
// transitions.
//
// # Responsibility boundaries
//
// This layer is only responsible for **scheduling**. It does not touch Telegram API details (it
// never calls getFile directly -- that is download.go's Downloader job) and it does not sanitize or
// render paths (RelPath is produced and already sanitized by the naming layer above; this layer
// only passes it through to state.Open for normalized validation).
//
// # Concurrency model
//
// File-level worker pool: the concurrency limit is cfg.FileConcurrency (degrades to 1 when <=0 and
// never rejects the task), and each worker claims the next unprocessed item in queue order. At most
// that many files are downloading at any moment.
//
// # State machine
//
// The repository has no in-memory state machine API (state.Store is disk JSON storage and this
// layer does not persist task state), so the per-task / per-item state machines are implemented
// with the same transition rules: PENDING -> RUNNING -> DONE/FAILED/CANCELLED; terminal states
// cannot transition anymore; an illegal transition returns state.ErrInvalidTransition (same
// semantics as Store.TransitionTask) instead of silently succeeding.
//
// # Progress aggregation
//
// Byte progress is accumulated atomically with atomic.Int64 (order does not matter). The progress
// callback may be invoked concurrently by several worker goroutines and the callback itself is not
// assumed to be thread-safe, so events are funnelled through a locked 128-deep queue into a single
// goroutine that invokes the callback serially (inside the lock only a non-blocking channel send is
// done, a user callback is never called while holding the lock); when the queue is full the
// snapshot is dropped but the file completion count is never lost (completion events fall back to
// the atomic counter).
//
// # Bitmap wiring (important)
//
// The *Bitmap returned by Session.Done() and the Session's internal lock protect the same bytes; if
// it were handed directly to ChunkPlan (ChunkPlan writes the bitmap under its own lock while
// Session.Flush reads the same bitmap under the Session's lock), two locks would protect one
// []byte and -race would certainly report a data race. So ChunkPlan holds an **independent bitmap
// copy** (state.NewBitmap(n) followed by copying the Session bitmap snapshot bit by bit), and the
// only write path to .state is the Session's own locked methods: the OnChunk callback calls
// session.MarkDone(idx) and ShouldFlush/Flush.
package tg

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gotd/td/tg"
	"github.com/iyear/tdl/core/dcpool"

	"go.uber.org/zap"

	"github.com/1368129224/tgshelf/internal/fs"
	"github.com/1368129224/tgshelf/internal/state"
)

// taskChunkSize is the chunk size the orchestration layer always uses (1MiB).
// It must divide 1MiB, and 1MiB satisfies that while being the server's hard limit.
//
// Basis for the value (real measurements): for the same file 1MiB chunks are about 1.7x faster
// than 512KiB (0.95~1.14 MiB/s vs 0.46~0.70 MiB/s) because the number of getFile requests is halved
// (28 vs 56) and the server's per-connection rate limit is proportional to the request count.
// Note: changing this value makes existing .part/.state files re-download because of a chunk
// validation mismatch (that is internal/state's established semantics, see ResetReason
// "chunk mismatch").
const taskChunkSize int64 = 1 << 20

// bestThreads picks the number of download threads for a single file adaptively by file size,
// bounded by max.
//
// Tiers: <1MiB -> 1, <5MiB -> 2, <20MiB -> 4, <50MiB -> 8, >=50MiB -> max.
// The result is truncated by max and is at least 1. When max <= 0 it falls back to
// defaultDownloadThreads (4).
func bestThreads(size int64, max int) int {
	if max <= 0 {
		max = defaultDownloadThreads
	}
	switch {
	case size < 1<<20:
		return min(1, max)
	case size < 5<<20:
		return min(2, max)
	case size < 20<<20:
		return min(4, max)
	case size < 50<<20:
		return min(8, max)
	default:
		return max
	}
}

// RunnerConfig is the configuration of TaskRunner.
type RunnerConfig struct {
	// RootDir is the download root directory (the root of the disk precheck and of resume files).
	RootDir string
	// Threads is the per-file thread limit (config.Download.Threads).
	Threads int
	// FileConcurrency is the file-level concurrency limit.
	FileConcurrency int
	// SkipSame, when true: skip an item whose final file already exists with the same name and
	// size.
	SkipSame bool
	// KeepParts, when true: keep .state after success (passes through state.WithKeepState).
	KeepParts bool
	// Pool is an optional multi-DC connection pool (provides multi-connection pool and multi-DC
	// acceleration, reusing tdl core/dcpool). When nil it falls back to the single-connection rpc.
	Pool dcpool.Pool
	// Takeout indicates whether Takeout session acceleration is enabled.
	Takeout bool
}

// TaskItem is one download entry of the orchestration layer.
type TaskItem struct {
	// Item is the normalized media description (provides Location and Size).
	Item MediaItem
	// RelPath is the already sanitized/rendered relative path (produced by the naming layer above;
	// this layer does not sanitize it).
	RelPath string
	// Mtime is the modification time written to the final file once the download completes.
	Mtime time.Time
	// Refresh is an optional file_reference refresh function.
	Refresh RefreshFunc
}

// TaskProgress is a task-level progress snapshot.
type TaskProgress struct {
	// TaskID is the ID of the owning task.
	TaskID string
	// BytesDone is the accumulated completed bytes of all files (atomic accumulation, order does
	// not matter).
	BytesDone int64
	// BytesTotal is the total bytes of all files.
	BytesTotal int64
	// ItemsDone is the number of finished files (including skipped ones).
	ItemsDone int
	// ItemsTotal is the total number of files.
	ItemsTotal int
	// Current is the relative path of the most recently active file (best effort, display only).
	Current string
}

// ItemResult is the execution result of a single file.
type ItemResult struct {
	// Index is the position of the item in the input slice.
	Index int
	// RelPath is the relative path of the item.
	RelPath string
	// State is the terminal state of the file.
	State state.TaskState
	// Bytes is the completed bytes of the file (success = file size; skipped = file size;
	// canceled = amount completed so far).
	Bytes int64
	// Skipped reports whether the item was skipped because of a skip_same hit.
	Skipped bool
	// Err is the failure reason (nil on success/skip).
	Err error
}

// TaskResult is the overall result of one task execution.
type TaskResult struct {
	// State is the task terminal state: DONE / FAILED / CANCELLED.
	State state.TaskState
	// Items has the same length as the input items and is in input order.
	Items []ItemResult
	// Err is the task-level error: for FAILED it contains "N/M succeeded"; for CANCELLED it wraps
	// the ctx error.
	Err error
}

// TaskRunner schedules a batch of TaskItems into file-level concurrent execution.
type TaskRunner struct {
	rpc     getFileRPC
	pool    dcpool.Pool
	logger  *zap.Logger
	dl      *Downloader
	cfg     RunnerConfig
	workers int
}

// NewTaskRunner creates a task orchestrator.
//
// rpc must not be nil; logger may be nil (internally degrades to zap.NewNop());
// cfg.RootDir must not be empty. FileConcurrency <= 0 is treated as 1.
func NewTaskRunner(rpc getFileRPC, logger *zap.Logger, cfg RunnerConfig) (*TaskRunner, error) {
	if rpc == nil {
		return nil, errors.New("tg: TaskRunner requires a non-nil getFileRPC")
	}
	if cfg.RootDir == "" {
		return nil, errors.New("tg: RunnerConfig.RootDir must not be empty")
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	dl, err := NewDownloader(rpc, logger)
	if err != nil {
		return nil, fmt.Errorf("tg: create download core: %w", err)
	}
	workers := cfg.FileConcurrency
	if workers <= 0 {
		workers = 1
	}
	return &TaskRunner{
		rpc:     rpc,
		pool:    cfg.Pool,
		logger:  logger,
		dl:      dl,
		cfg:     cfg,
		workers: workers,
	}, nil
}

// taskEvent is an event on the progress funnel channel: closed==true when a file completes,
// otherwise it carries the latest progress on chunk completion (the byte snapshot is already
// accumulated atomically by the sender).
type taskEvent struct {
	taskID  string
	closed  bool
	relPath string
}

// Run executes one task: disk precheck -> concurrent download -> state transitions -> result
// aggregation.
//
// The returned TaskResult.Items has the same length as the input and is in input order. On ctx
// cancellation: queued files are not started, running Downloaders stop; already downloaded .part
// and .state files are kept (resumable); the overall result is CANCELLED and
// errors.Is(err, context.Canceled) holds.
func (r *TaskRunner) Run(ctx context.Context, taskID string, items []TaskItem, progress func(TaskProgress)) (*TaskResult, error) {
	if ctx == nil {
		return nil, errors.New("tg: ctx must not be nil")
	}
	if taskID == "" {
		return nil, errors.New("tg: taskID must not be empty")
	}
	if progress == nil {
		progress = func(TaskProgress) {}
	}

	// Disk precheck: before any file is created.
	var total int64
	for i := range items {
		if items[i].Item.Size > 0 {
			if total > math.MaxInt64-items[i].Item.Size {
				return nil, fmt.Errorf("tg: total bytes of task %s overflow", taskID)
			}
			total += items[i].Item.Size
		}
	}
	if err := fs.CheckSpace(r.cfg.RootDir, total); err != nil {
		return nil, fmt.Errorf("tg: disk precheck for task %s: %w", taskID, err)
	}

	res := &TaskResult{State: state.StateDone, Items: make([]ItemResult, len(items))}
	for i := range res.Items {
		res.Items[i] = ItemResult{Index: i, RelPath: items[i].RelPath, State: state.StatePending}
	}

	// Task-level state machine: PENDING -> RUNNING (a legal transition, routed explicitly through
	// the state machine function).
	if err := validateTransitionLocal(state.StatePending, state.StateRunning); err != nil {
		return nil, fmt.Errorf("tg: illegal state transition for task %s: %w", taskID, err)
	}

	var (
		bytesDone  atomic.Int64
		itemsDone  atomic.Int64
		events     = make(chan taskEvent, 128)
		eventsDone = make(chan struct{})
	)

	// Progress funnel: a single goroutine invokes the callback serially (the callback is not
	// assumed to be thread-safe). Event order is not guaranteed (it is only used for display and
	// statistics) and individual events may be dropped (queue full).
	go func() {
		defer close(eventsDone)
		current := ""
		for ev := range events {
			if ev.relPath != "" {
				current = ev.relPath
			}
			if ev.closed {
				itemsDone.Add(1)
			}
			progress(TaskProgress{
				TaskID:     ev.taskID,
				BytesDone:  bytesDone.Load(),
				BytesTotal: total,
				ItemsDone:  int(itemsDone.Load()),
				ItemsTotal: len(items),
				Current:    current,
			})
		}
	}()

	// Degraded path when the queue is full: the file completion count switches to atomic
	// accumulation, so ItemsDone is never lost.
	emit := func(ev taskEvent) {
		select {
		case events <- ev:
		default:
			if ev.closed {
				itemsDone.Add(1)
			}
		}
	}
	chunkDone := func(n int64, path string) {
		bytesDone.Add(n)
		emit(taskEvent{taskID: taskID, relPath: path})
	}

	results := res.Items

	var (
		wg        sync.WaitGroup
		cancelled atomic.Bool
		failed    atomic.Int64
		succeeded atomic.Int64
		firstErr  error
		errMu     sync.Mutex
	)
	// next is the locked work queue cursor: workers claim the next item index in turn.
	var nextMu sync.Mutex
	next := 0

	for w := 0; w < r.workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				// Cancellation propagation: queued files are no longer started.
				if cancelled.Load() || ctx.Err() != nil {
					return
				}
				nextMu.Lock()
				if next >= len(items) {
					nextMu.Unlock()
					return
				}
				idx := next
				next++
				nextMu.Unlock()

				res := r.runItem(ctx, taskID, idx, items[idx], chunkDone)
				res.Index = idx
				res.RelPath = items[idx].RelPath

				errMu.Lock()
				results[idx] = res
				if res.State == state.StateFailed && firstErr == nil {
					firstErr = res.Err
				}
				errMu.Unlock()

				switch res.State {
				case state.StateDone:
					succeeded.Add(1)
				case state.StateFailed:
					failed.Add(1)
				case state.StateCancelled:
					cancelled.Store(true)
				}

				emit(taskEvent{taskID: taskID, closed: true, relPath: res.RelPath})
			}
		}()
	}

	wg.Wait()
	close(events)
	<-eventsDone

	// Task-level state machine: RUNNING -> terminal state (validated explicitly; an illegal
	// transition is reported as an error).
	// Priority: any cancellation -> CANCELLED; otherwise any failure -> FAILED; otherwise DONE.
	if cancelled.Load() || ctx.Err() != nil {
		if err := validateTransitionLocal(state.StateRunning, state.StateCancelled); err != nil {
			return nil, fmt.Errorf("tg: illegal state transition for task %s: %w", taskID, err)
		}
		cause := ctx.Err()
		if cause == nil {
			cause = context.Canceled
		}
		res.State = state.StateCancelled
		res.Err = fmt.Errorf("tg: task %s canceled: %w", taskID, cause)
	} else if failed.Load() > 0 {
		if err := validateTransitionLocal(state.StateRunning, state.StateFailed); err != nil {
			return nil, fmt.Errorf("tg: illegal state transition for task %s: %w", taskID, err)
		}
		res.State = state.StateFailed
		if firstErr != nil {
			res.Err = fmt.Errorf("tg: task %s partially failed: %d/%d succeeded: %w", taskID, succeeded.Load(), len(items), firstErr)
		} else {
			res.Err = fmt.Errorf("tg: task %s partially failed: %d/%d succeeded", taskID, succeeded.Load(), len(items))
		}
	} else {
		if err := validateTransitionLocal(state.StateRunning, state.StateDone); err != nil {
			return nil, fmt.Errorf("tg: illegal state transition for task %s: %w", taskID, err)
		}
		res.State = state.StateDone
	}
	return res, nil
}

// runItem runs the download lifecycle of a single file and returns that item's terminal result.
func (r *TaskRunner) runItem(ctx context.Context, taskID string, idx int, it TaskItem, chunkDone func(int64, string)) ItemResult {
	res := ItemResult{Index: idx, RelPath: it.RelPath, State: state.StatePending}

	// PENDING -> RUNNING (a legal transition, validated explicitly rather than silently).
	if err := validateTransitionLocal(res.State, state.StateRunning); err != nil {
		res.State = state.StateFailed
		res.Err = fmt.Errorf("tg: illegal state transition for item %d: %w", idx, err)
		return res
	}
	res.State = state.StateRunning

	size := it.Item.Size
	// skip_same: a file with the same name and size already exists -> skip, no getFile at all.
	if r.cfg.SkipSame && size >= 0 && it.RelPath != "" {
		final := filepath.Join(r.cfg.RootDir, filepath.FromSlash(it.RelPath))
		if info, err := os.Stat(final); err == nil && info.Size() == size {
			r.logger.Info("tg: skip_same hit, skipping download",
				zap.String("task", taskID), zap.String("path", it.RelPath))
			res.State = state.StateDone
			res.Skipped = true
			res.Bytes = size
			return res
		}
	}

	meta := state.ResumeMeta{
		Size:      size,
		ChunkSize: taskChunkSize,
		DocID:     docIDOf(it.Item),
		Peer:      it.Item.DialogID,
		MsgID:     it.Item.MessageID,
	}
	opts := []state.Option{state.WithMtime(it.Mtime)}
	if r.cfg.KeepParts {
		opts = append(opts, state.WithKeepState(true))
	}

	sess, err := state.Open(r.cfg.RootDir, it.RelPath, meta, opts...)
	if err != nil {
		res.Err = fmt.Errorf("tg: open resume session %q: %w", it.RelPath, err)
		res.State = state.StateFailed
		return res
	}
	defer func() { _ = sess.Close() }()

	// Pre-emptive flush: flush once before downloading so that .state definitely exists on
	// failure/cancellation (the Session writes .state only on Flush; if a chunk failure happens
	// before the first Flush threshold, without this step there would be no resume state file).
	if ferr := sess.Flush(); ferr != nil {
		res.Err = fmt.Errorf("tg: pre-flush .state %q: %w", it.RelPath, ferr)
		res.State = state.StateFailed
		return res
	}

	// Bitmap wiring (constraint 8): build an independent copy from the Session bitmap snapshot and
	// never hand sess.Done() directly to ChunkPlan (two locks protecting the same []byte would
	// certainly be a -race).
	planBM := state.NewBitmap(int(sess.ChunkCount()))
	if snap := sess.Done(); snap != nil {
		for i := int64(0); i < sess.ChunkCount(); i++ {
			v, gerr := snap.Get(int(i))
			if gerr == nil && v {
				if serr := planBM.Set(int(i), true); serr != nil {
					res.Err = fmt.Errorf("tg: build chunk bitmap: %w", serr)
					res.State = state.StateFailed
					return res
				}
			}
		}
	}
	plan, err := NewChunkPlan(size, taskChunkSize, planBM)
	if err != nil {
		res.Err = fmt.Errorf("tg: create chunk plan %q: %w", it.RelPath, err)
		res.State = state.StateFailed
		return res
	}

	part, err := os.OpenFile(sess.PartPath(), os.O_RDWR, 0o600)
	if err != nil {
		res.Err = fmt.Errorf("tg: open part file %q: %w", sess.PartPath(), err)
		res.State = state.StateFailed
		return res
	}

	// OnChunk: the only write path to .state is the Session's own locked methods.
	onChunk := func(chunkIdx int64) {
		if err := sess.MarkDone(chunkIdx); err != nil {
			r.logger.Warn("tg: MarkDone failed",
				zap.String("task", taskID), zap.Int64("chunk", chunkIdx), zap.Error(err))
			return
		}
		if sess.ShouldFlush(0) {
			if err := sess.Flush(); err != nil {
				r.logger.Warn("tg: Flush failed",
					zap.String("task", taskID), zap.Int64("chunk", chunkIdx), zap.Error(err))
			}
		}
	}

	// Multi-DC connection pool acceleration (aligned with tdl core/dcpool): per file, obtain a
	// dedicated invoker for its DCID from the pool (the multi-connection pool connects to that DC
	// directly, avoiding single-connection rate limits and cross-DC relaying).
	var itemRPC = r.rpc
	if r.pool != nil {
		dc := it.Item.DCID
		if r.cfg.Takeout {
			if dc > 0 {
				itemRPC = r.pool.Takeout(ctx, dc)
			} else {
				itemRPC = r.pool.Takeout(ctx, 0)
			}
		} else {
			if dc > 0 {
				itemRPC = r.pool.Client(ctx, dc)
			} else {
				itemRPC = r.pool.Default(ctx)
			}
		}
	}

	// Per-file progress conversion: the done value of the Downloader's Progress(done,total) is "the
	// accumulated bytes completed for this file" (order does not matter, including the part carried
	// over from a previous resume). The task-level BytesDone accumulates across files, so the last
	// value reported for this file is recorded here and only the **delta** is added to the global
	// counter, avoiding double counting across files.
	var prevDone atomic.Int64
	req := DownloadRequest{
		RPC:     itemRPC,
		Item:    it.Item,
		Writer:  part,
		Plan:    plan,
		Threads: bestThreads(size, r.cfg.Threads),
		OnChunk: onChunk,
		Progress: func(done, tot int64) {
			for {
				prev := prevDone.Load()
				if done <= prev {
					return
				}
				if prevDone.CompareAndSwap(prev, done) {
					chunkDone(done-prev, it.RelPath)
					return
				}
			}
		},
		Refresh: it.Refresh,
	}
	derr := r.dl.Download(ctx, req)
	cerr := part.Close()

	if derr != nil {
		// Canceled: Abort keeps .part and .state (resumable), terminal state CANCELLED.
		if errors.Is(derr, context.Canceled) || ctx.Err() != nil {
			if aerr := sess.Abort(); aerr != nil {
				r.logger.Warn("tg: Abort failed", zap.String("task", taskID), zap.Error(aerr))
			}
			if err := validateTransitionLocal(state.StateRunning, state.StateCancelled); err != nil {
				res.Err = fmt.Errorf("tg: illegal state transition for item %d: %w", idx, err)
				res.State = state.StateFailed
				return res
			}
			res.State = state.StateCancelled
			res.Err = derr
			res.Bytes = plan.CompletedBytes()
			return res
		}
		// Ordinary failure: Abort keeps the state so the download can be resumed, terminal state
		// FAILED.
		if aerr := sess.Abort(); aerr != nil {
			r.logger.Warn("tg: Abort failed", zap.String("task", taskID), zap.Error(aerr))
		}
		if err := validateTransitionLocal(state.StateRunning, state.StateFailed); err != nil {
			res.Err = fmt.Errorf("tg: illegal state transition for item %d: %w", idx, err)
			res.State = state.StateFailed
			return res
		}
		res.State = state.StateFailed
		res.Err = derr
		res.Bytes = plan.CompletedBytes()
		if cerr != nil {
			res.Err = fmt.Errorf("%w (closing part file: %v)", res.Err, cerr)
		}
		return res
	}
	if cerr != nil {
		if aerr := sess.Abort(); aerr != nil {
			r.logger.Warn("tg: Abort failed", zap.String("task", taskID), zap.Error(aerr))
		}
		res.Err = fmt.Errorf("tg: close part file %q: %w", sess.PartPath(), cerr)
		res.State = state.StateFailed
		return res
	}

	// Success: Commit (rename to the final file), terminal state DONE.
	if err := validateTransitionLocal(state.StateRunning, state.StateDone); err != nil {
		res.Err = fmt.Errorf("tg: illegal state transition for item %d: %w", idx, err)
		res.State = state.StateFailed
		return res
	}
	if err := sess.Commit(); err != nil {
		if aerr := sess.Abort(); aerr != nil {
			r.logger.Warn("tg: Abort failed", zap.String("task", taskID), zap.Error(aerr))
		}
		res.Err = fmt.Errorf("tg: commit %q: %w", it.RelPath, err)
		res.State = state.StateFailed
		return res
	}
	res.State = state.StateDone
	res.Bytes = size
	return res
}

// docIDOf extracts the document ID of a MediaItem (0 when Location is nil).
func docIDOf(m MediaItem) int64 {
	if loc, ok := m.Location.(*tg.InputDocumentFileLocation); ok {
		return loc.ID
	}
	return 0
}

// validateTransitionLocal is a local implementation of the internal/state state machine transition
// rules (state.validateTransition is not exported). The rules match Store.TransitionTask:
//   - terminal states (DONE/FAILED/CANCELLED) cannot transition;
//   - PENDING can only -> RUNNING or CANCELLED;
//   - RUNNING can only -> DONE, FAILED or CANCELLED.
//
// An illegal transition returns an error wrapping state.ErrInvalidTransition, testable with
// errors.Is.
func validateTransitionLocal(from, to state.TaskState) error {
	terminal := func(s state.TaskState) bool {
		return s == state.StateDone || s == state.StateFailed || s == state.StateCancelled
	}
	if terminal(from) {
		return fmt.Errorf("%w: %s → %s", state.ErrInvalidTransition, from, to)
	}
	switch from {
	case state.StatePending:
		if to == state.StateRunning || to == state.StateCancelled {
			return nil
		}
	case state.StateRunning:
		if to == state.StateDone || to == state.StateFailed || to == state.StateCancelled {
			return nil
		}
	}
	return fmt.Errorf("%w: %s → %s", state.ErrInvalidTransition, from, to)
}
