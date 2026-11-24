// download_cmd.go: the real download command (no bot needed), tgshelf -download <t.me link>.
//
// Chain: config.Load -> build the counting client (mirrors the internal/tg default middleware
// chain: recovery + floodWait + retry, with the floodWait variant also counting) ->
// Pipeline.ResolveLink -> internal/naming renders a safe relative path -> fs.CheckSpace disk
// precheck (before any file is created) -> file-level concurrency (semaphore queueing) ->
// state.Open session + NewDownloader chunked download.
//
// # Bitmap wiring (same convention as internal/tg/task.go)
//
// ChunkPlan is built on an **independent copy** of the Session bitmap snapshot; sess.Done() is
// never handed to ChunkPlan directly - two locks guarding the same []byte would be a data race
// (always reported by -race). The only write path for .state is the Session's own locked
// methods: MarkDone + ShouldFlush/Flush in the OnChunk callback; Commit on success, Abort on
// failure.
//
// # Why not use TaskRunner directly
//
// runItem in TaskRunner does not set Refresh on DownloadRequest, so an
// expired file_reference cannot heal itself; this command wires
// Downloader + Refresher directly, replicating the precheck, file concurrency, failure isolation
// and skip_same in this layer with a minimal implementation; full task state machine
// persistence lives in the bot path.
//
// # Drift risk against the internal/tg defaults
//
// buildDownloadClient mirrors the default parameters of internal/tg/client.go (retries,
// timeouts, device identity), because FLOOD_WAIT counting must sit where the floodWait
// middleware sits (inside the chain) and internal/tg does not allow injecting middleware from
// outside. The two sets of defaults must be kept in sync by hand (registered as a known risk).
package main

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cenkalti/backoff/v4"
	"github.com/gotd/td/bin"
	"github.com/gotd/td/telegram"
	tgpkg "github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"go.uber.org/zap"

	"github.com/iyear/tdl/core/dcpool"
	"github.com/iyear/tdl/core/middlewares/recovery"
	"github.com/iyear/tdl/core/middlewares/retry"

	"github.com/1368129224/tgshelf/internal/config"
	"github.com/1368129224/tgshelf/internal/fs"
	"github.com/1368129224/tgshelf/internal/naming"
	"github.com/1368129224/tgshelf/internal/state"
	"github.com/1368129224/tgshelf/internal/tg"
)

// downloadArgs carries all parameters of the -download subcommand.
type downloadArgs struct {
	urls            []string
	to              string
	tag             string
	threads         int
	fileConcurrency int
	keepParts       bool
	chunkBytes      int64
}

func (a *downloadArgs) String() string { return strings.Join(a.urls, ",") }

func (a *downloadArgs) Set(v string) error {
	for _, part := range strings.Split(v, ",") {
		if part = strings.TrimSpace(part); part != "" {
			a.urls = append(a.urls, part)
		}
	}
	return nil
}

// dlGetFile is the minimal RPC interface the download core needs (a structural mirror of
// internal/tg.getFileRPC, which is unexported, so a local interface satisfies the same method set).
type dlGetFile interface {
	UploadGetFile(ctx context.Context, req *tgpkg.UploadGetFileRequest) (tgpkg.UploadFileClass, error)
}

// rpcStats aggregates the RPC observations of one -download session.
type rpcStats struct {
	getFile   atomic.Int64 // number of logical UploadGetFile calls (retries inside the middleware are not counted)
	refreshes atomic.Int64 // number of file_reference refreshes
	mu        sync.Mutex
	floods    []floodEvent
	started   time.Time
}

// floodEvent is one FLOOD_WAIT event: the wait the server demanded and the offset from the start.
type floodEvent struct {
	wait     time.Duration
	atOffset time.Duration
}

func (s *rpcStats) recordFlood(wait time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	off := time.Duration(0)
	if !s.started.IsZero() {
		off = time.Since(s.started)
	}
	s.floods = append(s.floods, floodEvent{wait: wait, atOffset: off})
}

func (s *rpcStats) floodSnapshot() []floodEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]floodEvent(nil), s.floods...)
}

// countingRPC counts UploadGetFile calls and forwards every other method to the embedded *tg.Client.
type countingRPC struct {
	*tgpkg.Client
	stats *rpcStats
}

func (c *countingRPC) UploadGetFile(ctx context.Context, req *tgpkg.UploadGetFileRequest) (tgpkg.UploadFileClass, error) {
	c.stats.getFile.Add(1)
	return c.Client.UploadGetFile(ctx, req)
}

// perFileRPC adds a per-file counter on top of the shared countingRPC: with concurrent
// downloads a "global counter delta" would include requests of other files, so per-file numbers
// taken that way are not trustworthy; this wrapper gives every file an exact own request count.
type perFileRPC struct {
	inner  dlGetFile
	shared *rpcStats
	local  *atomic.Int64
}

func (p *perFileRPC) UploadGetFile(ctx context.Context, req *tgpkg.UploadGetFileRequest) (tgpkg.UploadFileClass, error) {
	p.local.Add(1)
	if p.shared != nil {
		p.shared.getFile.Add(1)
	}
	return p.inner.UploadGetFile(ctx, req)
}

// Compile-time assertions: countingRPC satisfies both the resolve and the download narrow interfaces.
var (
	_ dlGetFile      = (*countingRPC)(nil)
	_ tg.PipelineRPC = (*countingRPC)(nil)
)

// floodWaitCount is the counting mirror of floodWait in internal/tg/middleware.go: on
// FLOOD_WAIT_x it records the event, sleeps x+1 seconds and retries the same call until ctx is
// cancelled.
type floodWaitCount struct {
	ctx   context.Context
	stats *rpcStats
}

func (f floodWaitCount) Handle(next tgpkg.Invoker) telegram.InvokeFunc {
	return func(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
		for {
			err := next.Invoke(ctx, input, output)
			if err == nil {
				return nil
			}
			wait, ok := tgerr.AsFloodWait(err)
			if !ok {
				return err
			}
			f.stats.recordFlood(wait)
			timer := time.NewTimer(wait + time.Second)
			select {
			case <-f.ctx.Done():
				timer.Stop()
				return f.ctx.Err()
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
				// the wait is over, retry the same call
			}
		}
	}
}

// downloadClientDefaults mirror the default runtime parameters of internal/tg/client.go.
const (
	dlMaxRetries       = 5
	dlRetryInterval    = 5 * time.Second
	dlDialTimeout      = 10 * time.Second
	dlReconnectTimeout = 10 * time.Minute
)

func dlReconnectBackoff() backoff.BackOff {
	b := backoff.NewExponentialBackOff()
	b.Multiplier = 1.1
	b.MaxElapsedTime = dlReconnectTimeout
	b.MaxInterval = 10 * time.Second
	return b
}

// downloadDevice shares the same identity/fingerprint rules as the main bot path (see
// tg.DeviceConfigFor); otherwise -download and the bot would join the same session with
// different identities and the server would see two applications.
func downloadDevice(cfg *config.Config) telegram.DeviceConfig {
	return tg.DeviceConfigFor(cfg.Telegram)
}

func downloadMiddlewares(ctx context.Context, stats *rpcStats) []telegram.Middleware {
	return []telegram.Middleware{
		recovery.New(ctx, dlReconnectBackoff()),
		floodWaitCount{ctx: ctx, stats: stats},
		retry.New(dlMaxRetries),
	}
}

// buildDownloadClient builds a counting telegram.Client. The middleware chain matches
// internal/tg defaultMiddlewares, with only floodWait replaced by the counting version.
func buildDownloadClient(ctx context.Context, cfg *config.Config, storage *tg.SessionStorage, stats *rpcStats) (*telegram.Client, error) {
	creds, err := tg.ResolveCredentials(cfg.Telegram)
	if err != nil {
		return nil, err
	}
	resolver, err := tg.BuildResolver(cfg.ProxyURL)
	if err != nil {
		return nil, fmt.Errorf("build proxy: %w", err)
	}
	opts := telegram.Options{
		SessionStorage:      storage,
		Device:              downloadDevice(cfg),
		Resolver:            resolver,
		ReconnectionBackoff: func() backoff.BackOff { return dlReconnectBackoff() },
		Middlewares:         downloadMiddlewares(ctx, stats),
		RetryInterval:       dlRetryInterval,
		MaxRetries:          dlMaxRetries,
		DialTimeout:         dlDialTimeout,
	}
	return telegram.NewClient(creds.AppID, creds.AppHash, opts), nil
}

// dlItem is one pending download entry: source link, media and the rendered safe relative path.
type dlItem struct {
	url  string
	item tg.MediaItem
	rel  string
}

// fileStat holds the execution statistics of a single file.
type fileStat struct {
	relPath   string
	absPath   string
	size      int64
	bytes     int64
	skipped   bool
	md5       string
	requests  int64
	refreshes int64
	duration  time.Duration
	err       error
}

// dlEngine holds the static config and observation handles of one -download session.
type dlEngine struct {
	root      string
	tag       string
	threads   int
	fileConc  int
	chunk     int64
	skipSame  bool
	keepParts bool
	stats     *rpcStats
	out       io.Writer
	printMu   sync.Mutex
	started   time.Time
	pool      dcpool.Pool
}

// mediaDocID extracts the document ID of a MediaItem (0 when Location is nil or not a document).
// internal/tg.docIDOf is unexported, so the same rule is implemented here.
func mediaDocID(m tg.MediaItem) int64 {
	if loc, ok := m.Location.(*tgpkg.InputDocumentFileLocation); ok {
		return loc.ID
	}
	return 0
}

// downloadRelPath renders the config template and sanitizes a safe relative path (it may carry a
// tag subdirectory prefix).
func downloadRelPath(tpl string, item tg.MediaItem, tag string) (string, error) {
	if strings.TrimSpace(tpl) == "" {
		tpl = "{{ .DialogID }}_{{ .MessageID }}_{{ filenamify .FileName }}"
	}
	fields := naming.Fields{
		DialogID:     item.DialogID,
		MessageID:    item.MessageID,
		MessageDate:  time.Unix(item.Date, 0),
		FileName:     item.Name,
		FileSize:     item.Size,
		DownloadDate: time.Now(),
	}
	rendered, err := naming.Render(tpl, fields)
	if err != nil {
		return "", fmt.Errorf("render filename template: %w", err)
	}
	safe := naming.Sanitize(rendered)
	rel := safe
	if tag != "" {
		rel = tag + "/" + safe
	}
	return rel, nil
}

// md5File computes the file MD5 (lowercase hex).
func md5File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// runDownload is the entry point of the -download subcommand (cfg.Validate is not called: no
// bot_token is needed). It returns nil when everything succeeded and a summary error when some
// items failed (main then exits 1).
func runDownload(cfg *config.Config, configPath string, args *downloadArgs) error {
	if err := preflightTelegram(cfg); err != nil {
		return err
	}
	if len(args.urls) == 0 {
		return errors.New("usage: tgshelf -download <t.me link> [-download <link2> ...] " +
			"[-to dir] [-threads N] [-file-concurrency N] [-tag NAME] [-keep-parts] [-chunk-bytes N] [-config X]")
	}
	root := strings.TrimSpace(args.to)
	if root == "" {
		root = strings.TrimSpace(cfg.DownloadPath)
	}
	if root == "" {
		return errors.New("download target directory is empty: pass -to or set download_path")
	}
	if abs, aerr := filepath.Abs(root); aerr == nil {
		root = abs
	}
	if args.tag != "" {
		if strings.ContainsAny(args.tag, "/\\") || strings.Contains(args.tag, "..") {
			return fmt.Errorf("invalid tag %q: it cannot contain path separators or two dots", args.tag)
		}
		if len(cfg.Tags) > 0 {
			found := false
			for _, t := range cfg.Tags {
				if t == args.tag {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("tag %q is not listed in config tags", args.tag)
			}
		}
	}
	threads := args.threads
	if threads <= 0 {
		threads = cfg.Download.Threads
	}
	if threads <= 0 {
		threads = 1
	}
	fileConc := args.fileConcurrency
	if fileConc <= 0 {
		fileConc = cfg.Download.FileConcurrency
	}
	if fileConc <= 0 {
		fileConc = 1
	}
	chunk := args.chunkBytes
	if chunk <= 0 {
		chunk = 1 << 20 // server-side hard limit; measured about 1.7x faster than 512KiB
	}
	if chunk <= 0 || chunk > tg.MaxChunkSize || tg.MaxChunkSize%chunk != 0 {
		return fmt.Errorf("invalid chunk size %d: it must divide 1MiB and not exceed 1MiB", chunk)
	}

	sessionPath := sessionPathOf(cfg, configPath)
	if _, err := os.Stat(sessionPath); err != nil {
		return fmt.Errorf("not logged in: session file %s not found, run tgshelf -login first", sessionPath)
	}
	storage, err := tg.OpenSession(sessionPath)
	if err != nil {
		return fmt.Errorf("open session: %w", err)
	}
	defer func() { _ = storage.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	stats := &rpcStats{started: time.Now()}
	client, err := buildDownloadClient(ctx, cfg, storage, stats)
	if err != nil {
		return fmt.Errorf("build Telegram client: %w", err)
	}

	dataDir := dataDirOf(configPath)
	if err := fs.EnsureDir(dataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	store, err := state.New(filepath.Join(dataDir, "state.json"))
	if err != nil {
		return fmt.Errorf("open state file: %w", err)
	}
	defer func() { _ = store.Close() }()

	poolSize := int64(threads * fileConc)
	if poolSize < 8 {
		poolSize = 8
	}
	if poolSize > 32 {
		poolSize = 32
	}
	pool := dcpool.NewPool(client, poolSize, downloadMiddlewares(ctx, stats)...)
	defer func() { _ = pool.Close() }()

	eng := &dlEngine{
		root:      root,
		tag:       args.tag,
		threads:   threads,
		fileConc:  fileConc,
		chunk:     chunk,
		skipSame:  cfg.Download.SkipSame,
		keepParts: args.keepParts || cfg.Download.KeepParts,
		stats:     stats,
		out:       os.Stdout,
		started:   time.Now(),
		pool:      pool,
	}

	err = client.Run(ctx, func(ctx context.Context) error {
		return eng.run(ctx, client.API(), store, cfg, args.urls)
	})
	if err != nil {
		return fmt.Errorf("run download: %w", err)
	}
	return nil
}

// run does its work inside the connected session: resolve -> precheck -> concurrent download -> summary.
func (e *dlEngine) run(ctx context.Context, api *tgpkg.Client, store *state.Store, cfg *config.Config, urls []string) error {
	counting := &countingRPC{Client: api, stats: e.stats}
	// When <= 0, tg.NewPeerResolver falls back to its built-in default (6 hours); see the note
	// in main.go.
	ttl := time.Duration(cfg.Download.CacheDialogsHours) * time.Hour
	resolver, err := tg.NewPeerResolver(counting, store, ttl)
	if err != nil {
		return fmt.Errorf("build peer resolver: %w", err)
	}
	pipeline, err := tg.NewPipeline(counting, resolver)
	if err != nil {
		return fmt.Errorf("build resolve pipeline: %w", err)
	}

	// 1) Resolve all links (failure isolation: a bad link does not affect the remaining entries).
	var items []dlItem
	var resolveErrs []string
	seen := map[string]int{}
	for _, raw := range urls {
		res, rerr := pipeline.ResolveLink(ctx, raw)
		if rerr != nil {
			resolveErrs = append(resolveErrs, fmt.Sprintf("[%s] resolve failed: %v", raw, rerr))
			continue
		}
		for _, item := range res.Items {
			rel, nerr := downloadRelPath(cfg.Download.Template, item, e.tag)
			if nerr != nil {
				resolveErrs = append(resolveErrs, fmt.Sprintf("[%s] naming failed: %v", raw, nerr))
				continue
			}
			if _, jerr := naming.SafeJoin(e.root, rel); jerr != nil {
				resolveErrs = append(resolveErrs, fmt.Sprintf("[%s] path validation failed %q: %v", raw, rel, jerr))
				continue
			}
			if n := seen[rel]; n > 0 {
				ext := filepath.Ext(rel)
				rel = strings.TrimSuffix(rel, ext) + fmt.Sprintf("_dup%d%s", n+1, ext)
			}
			seen[rel]++
			items = append(items, dlItem{url: raw, item: item, rel: rel})
		}
	}
	for _, msg := range resolveErrs {
		_, _ = fmt.Fprintln(e.out, msg)
	}
	if len(items) == 0 {
		return fmt.Errorf("no downloadable items (all %d links failed to resolve)", len(urls))
	}

	var total int64
	for _, it := range items {
		if it.item.Size > 0 {
			total += it.item.Size
		}
	}

	// 2) Disk precheck: before any file is created (nothing is left behind).
	if err := fs.EnsureDir(e.root, 0o755); err != nil {
		return fmt.Errorf("create download directory (no data downloaded): %w", err)
	}
	if err := fs.CheckSpace(e.root, total); err != nil {
		return fmt.Errorf("disk space precheck (no data downloaded): %w", err)
	}
	_, _ = fmt.Fprintf(e.out, "%d items, %s total bytes, target directory %s, threads=%d file_concurrency=%d chunk=%d\n",
		len(items), fs.HumanBytes(total), e.root, e.threads, e.fileConc, e.chunk)

	// refreshFor resolves the source link again, matches the original media by MessageID+DocID
	// and returns the new Location (the basis for healing an expired file_reference).
	refreshFor := func(it dlItem) tg.RefreshFunc {
		return func(ctx context.Context) (tgpkg.InputFileLocationClass, error) {
			e.stats.refreshes.Add(1)
			res, rerr := pipeline.ResolveLink(ctx, it.url)
			if rerr != nil {
				return nil, fmt.Errorf("re-resolve to refresh file_reference: %w", rerr)
			}
			for _, m := range res.Items {
				if m.MessageID == it.item.MessageID && mediaDocID(m) == mediaDocID(it.item) && m.Location != nil {
					return m.Location, nil
				}
			}
			return nil, errors.New("refresh file_reference: the original media is missing from the re-resolved result")
		}
	}

	// 3) File-level concurrent download: semaphore queueing, the timeline proves the queueing.
	fileConc := e.fileConc
	if fileConc <= 0 {
		fileConc = 1
	}
	results := make([]fileStat, len(items))
	var (
		wg        sync.WaitGroup
		sem       = make(chan struct{}, fileConc)
		global    atomic.Int64
		doneCount atomic.Int64
	)
	progressStop := make(chan struct{})
	progressDone := make(chan struct{})
	go func() {
		defer close(progressDone)
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-progressStop:
				return
			case <-t.C:
				e.printMu.Lock()
				_, _ = fmt.Fprintf(e.out, "[progress] %s / %s (%d/%d files finished) elapsed %s\n",
					fs.HumanBytes(global.Load()), fs.HumanBytes(total),
					doneCount.Load(), len(items), time.Since(e.started).Round(time.Millisecond))
				e.printMu.Unlock()
			}
		}
	}()

	for i := range items {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e.timeline(fmt.Sprintf("queued file[%d] %s", i, items[i].rel))
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results[i] = fileStat{relPath: items[i].rel, err: ctx.Err()}
				doneCount.Add(1)
				e.timeline(fmt.Sprintf("end file[%d] %s state=CANCEL", i, items[i].rel))
				return
			}
			defer func() { <-sem }()
			e.timeline(fmt.Sprintf("start file[%d] %s", i, items[i].rel))
			results[i] = e.runOneFile(ctx, counting, items[i], refreshFor(items[i]), func(delta int64) { global.Add(delta) })
			doneCount.Add(1)
			r := results[i]
			st := "OK"
			if r.err != nil {
				st = "FAIL"
			} else if r.skipped {
				st = "SKIP"
			}
			e.timeline(fmt.Sprintf("end file[%d] %s state=%s", i, r.relPath, st))
		}(i)
	}
	wg.Wait()
	close(progressStop)
	<-progressDone

	// 4) Summary.
	e.printSummary(results, resolveErrs, total, global.Load(), time.Since(e.started))
	var failed int
	for _, r := range results {
		if r.err != nil {
			failed++
		}
	}
	if failed > 0 || len(resolveErrs) > 0 {
		return fmt.Errorf("download finished with failures: %d/%d succeeded", len(results)-failed, len(results)+len(resolveErrs))
	}
	return nil
}

// timeline prints an event line with a relative timestamp (concurrency safe).
func (e *dlEngine) timeline(msg string) {
	e.printMu.Lock()
	_, _ = fmt.Fprintf(e.out, "[timeline t+%.3fs] %s\n", time.Since(e.started).Seconds(), msg)
	e.printMu.Unlock()
}

// printSummary prints the summary report: per-file results + global statistics.
func (e *dlEngine) printSummary(results []fileStat, resolveErrs []string, total, done int64, elapsed time.Duration) {
	e.printMu.Lock()
	defer e.printMu.Unlock()
	out := e.out
	_, _ = fmt.Fprintln(out, "=== download summary ===")
	var okN, skipN, failN int
	for i, r := range results {
		switch {
		case r.err != nil:
			failN++
			_, _ = fmt.Fprintf(out, "  [FAIL] file[%d] %s: %s done, took %s, %d requests: %v\n",
				i, r.relPath, fs.HumanBytes(r.bytes), r.duration.Round(time.Millisecond), r.requests, r.err)
		case r.skipped:
			skipN++
			_, _ = fmt.Fprintf(out, "  [SKIP] file[%d] %s → %s (%s, same name and size already exists, md5=%s)\n",
				i, r.relPath, r.absPath, fs.HumanBytes(r.size), r.md5)
		default:
			okN++
			_, _ = fmt.Fprintf(out, "  [OK] file[%d] %s → %s (%s, md5=%s, took %s, getFile %d times, %d refreshes)\n",
				i, r.relPath, r.absPath, fs.HumanBytes(r.size), r.md5,
				r.duration.Round(time.Millisecond), r.requests, r.refreshes)
		}
	}
	floods := e.stats.floodSnapshot()
	mbps := float64(done) / elapsed.Seconds() / (1 << 20)
	_, _ = fmt.Fprintf(out, "succeeded %d/%d (skipped %d, failed %d, resolve failed %d) | downloaded %s / %s | elapsed %s | average %.2f MiB/s | getFile requests %d | FLOOD_WAIT %d times\n",
		okN, len(results), skipN, failN, len(resolveErrs),
		fs.HumanBytes(done), fs.HumanBytes(total), elapsed.Round(time.Millisecond), mbps,
		e.stats.getFile.Load(), len(floods))
	for i, f := range floods {
		_, _ = fmt.Fprintf(out, "  FLOOD_WAIT[%d] waited %s (t+%.1fs)\n", i, f.wait, f.atOffset.Seconds())
	}
}

// runOneFile runs the download lifecycle of a single file: skip_same -> state.Open ->
// independent copy of the bitmap snapshot -> NewDownloader -> Commit/Abort -> MD5.
func (e *dlEngine) runOneFile(ctx context.Context, rpc dlGetFile, it dlItem, refresh tg.RefreshFunc, onDelta func(int64)) fileStat {
	res := fileStat{relPath: it.rel}
	if err := ctx.Err(); err != nil {
		res.err = err
		return res
	}
	size := it.item.Size
	final := filepath.Join(e.root, filepath.FromSlash(it.rel))
	if abs, aerr := filepath.Abs(final); aerr == nil {
		res.absPath = abs
	} else {
		res.absPath = final
	}

	// skip_same: when a final file with the same name and size already exists, skip it without
	// a single getFile call.
	if e.skipSame && size > 0 {
		if info, err := os.Stat(final); err == nil && info.Size() == size {
			res.skipped = true
			res.size = size
			res.bytes = size
			if sum, merr := md5File(final); merr == nil {
				res.md5 = sum
			}
			return res
		}
	}

	meta := state.ResumeMeta{
		Size:      size,
		ChunkSize: e.chunk,
		DocID:     mediaDocID(it.item),
		Peer:      it.item.DialogID,
		MsgID:     it.item.MessageID,
	}
	opts := []state.Option{state.WithMtime(time.Unix(it.item.Date, 0))}
	if e.keepParts {
		opts = append(opts, state.WithKeepState(true))
	}
	sess, err := state.Open(e.root, it.rel, meta, opts...)
	if err != nil {
		res.err = fmt.Errorf("open resume session: %w", err)
		return res
	}
	defer func() { _ = sess.Close() }()

	// Write .state early so a resumable state file exists even after a failure or a kill.
	if err := sess.Flush(); err != nil {
		res.err = fmt.Errorf("write .state early: %w", err)
		return res
	}

	// Bitmap wiring: Session bitmap snapshot -> independent copy (sess.Done() is never given to ChunkPlan).
	planBM := state.NewBitmap(int(sess.ChunkCount()))
	if snap := sess.Done(); snap != nil {
		for i := int64(0); i < sess.ChunkCount(); i++ {
			if v, gerr := snap.Get(int(i)); gerr == nil && v {
				if serr := planBM.Set(int(i), true); serr != nil {
					res.err = fmt.Errorf("build chunk bitmap: %w", serr)
					return res
				}
			}
		}
	}
	plan, err := tg.NewChunkPlan(size, e.chunk, planBM)
	if err != nil {
		res.err = fmt.Errorf("create chunk plan: %w", err)
		return res
	}

	part, err := os.OpenFile(sess.PartPath(), os.O_RDWR, 0o600)
	if err != nil {
		res.err = fmt.Errorf("open part file: %w", err)
		return res
	}
	defer func() { _ = part.Close() }()

	var prevDone atomic.Int64
	var localReqs atomic.Int64
	refreshStart := e.stats.refreshes.Load()

	var targetRPC = rpc
	var sharedStats *rpcStats
	if e.pool != nil {
		// Pooled connections bypass countingRPC, so perFileRPC updates global getFile stats.
		sharedStats = e.stats
		if it.item.DCID > 0 {
			targetRPC = e.pool.Client(ctx, it.item.DCID)
		} else {
			targetRPC = e.pool.Default(ctx)
		}
	}
	dl, err := tg.NewDownloader(&perFileRPC{inner: targetRPC, shared: sharedStats, local: &localReqs}, nil)
	if err != nil {
		res.err = fmt.Errorf("create download core: %w", err)
		return res
	}

	req := tg.DownloadRequest{
		Item:    it.item,
		Writer:  part,
		Plan:    plan,
		Threads: e.threads,
		OnChunk: func(idx int64) {
			if err := sess.MarkDone(idx); err != nil {
				return
			}
			if sess.ShouldFlush(0) {
				_ = sess.Flush()
			}
		},
		Progress: func(done, tot int64) {
			for {
				prev := prevDone.Load()
				if done <= prev {
					return
				}
				if prevDone.CompareAndSwap(prev, done) {
					onDelta(done - prev)
					return
				}
			}
		},
		Refresh: refresh,
	}

	t0 := time.Now()
	derr := dl.Download(ctx, req)
	res.duration = time.Since(t0)
	cerr := part.Close()
	res.requests = localReqs.Load()
	res.refreshes = e.stats.refreshes.Load() - refreshStart

	if derr != nil {
		// Failure: Abort keeps the .part and .state files (resumable).
		_ = sess.Abort()
		res.err = derr
		res.bytes = prevDone.Load()
		if cerr != nil {
			res.err = fmt.Errorf("%w (closing part file: %v)", res.err, cerr)
		}
		return res
	}
	if cerr != nil {
		_ = sess.Abort()
		res.err = fmt.Errorf("close part file: %w", cerr)
		return res
	}
	if err := sess.Commit(); err != nil {
		_ = sess.Abort()
		res.err = fmt.Errorf("commit: %w", err)
		res.bytes = prevDone.Load()
		return res
	}
	res.size = size
	res.bytes = size
	if sum, merr := md5File(final); merr == nil {
		res.md5 = sum
	} else {
		res.err = fmt.Errorf("compute MD5: %w", merr)
	}
	return res
}

// Compile-time assertion that keeps the zap import in use (logging is reserved for future extensions).
var _ = zap.NewNop
