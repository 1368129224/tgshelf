package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	internfs "github.com/1368129224/tgshelf/internal/fs"
)

// ResumeMeta is the minimal metadata needed to validate a resume, filled in by the
// internal/tg side from a MediaItem.
//
// Note: this package must not import internal/tg (that would create an import cycle),
// so only the fields needed for validation are kept here, not the whole MediaItem.
type ResumeMeta struct {
	Size      int64 // total file size in bytes
	ChunkSize int64 // bytes per chunk (e.g. 512KiB)
	DocID     int64 // Telegram document ID, used to tell whether the .state belongs to the same file
	Peer      int64 // session peer ID
	MsgID     int   // message ID
}

const (
	// resumeStateVersion is the format version of the .state file.
	resumeStateVersion = 1
	// resumeFlushChunks is the count of finished chunks that triggers ShouldFlush.
	resumeFlushChunks = 32
	// resumeFlushInterval is the time since the last flush that triggers ShouldFlush.
	resumeFlushInterval = 5 * time.Second
	// partsDirName is the name of the chunk temp directory (created inside the directory
	// of the final file so rename stays atomic on the same disk).
	partsDirName = ".parts"
	// resumeDirPerm / resumeFilePerm are the permissions of the .parts directory and of
	// the part/state files.
	resumeDirPerm  = 0o700
	resumeFilePerm = 0o600
)

// resumeState is the JSON structure of the .state file (v=1).
type resumeState struct {
	V         int     `json:"v"`
	Size      int64   `json:"size"`
	Chunk     int64   `json:"chunk"`
	Done      *Bitmap `json:"done"`
	DocID     int64   `json:"doc_id"`
	Peer      int64   `json:"peer"`
	MsgID     int     `json:"msg_id"`
	UpdatedAt int64   `json:"updated_at"`
}

// Option customizes Session behavior.
type Option func(*sessionOptions)

type sessionOptions struct {
	keepState bool
	mtime     time.Time
}

// WithKeepState controls whether .state is kept after a successful Commit.
// The default is false: .state is deleted on success (confirmed requirement: delete on
// success, keep on failure).
func WithKeepState(v bool) Option {
	return func(o *sessionOptions) { o.keepState = v }
}

// WithMtime sets the modification time written to the final file after Commit (e.g. the
// Date of a media message).
// When unset, the mtime the final file already has after rename is kept.
func WithMtime(t time.Time) Option {
	return func(o *sessionOptions) { o.mtime = t }
}

// Session is the resumable download session of a single file.
//
// .part is a sparse file (preallocated with Truncate, no zero bytes are written),
// .state records the bitmap of finished chunks and is persisted atomically through
// AtomicWriteFile.
// MarkDone/Done/Flush/ShouldFlush may be called concurrently from several worker
// goroutines;
// Bitmap itself is not concurrency safe, so every access happens under the internal lock
// of Session.
type Session struct {
	mu      sync.Mutex
	flushMu sync.Mutex // serializes disk Flush/Commit/Close without holding mu across I/O

	meta      ResumeMeta
	numChunks int64

	finalPath string
	partPath  string
	statePath string

	f         *os.File
	bm        *Bitmap
	keepState bool
	mtime     time.Time

	resetReason string

	chunksSince int       // chunks completed since the last Flush
	lastFlush   time.Time // time of the last Flush (initialized to the Open time)
	committed   bool
	activeKey   string // registered in activeStatePaths when opened via ResolveTarget
}

var (
	resolveMu        sync.Mutex
	activeStatePaths = make(map[string]struct{})
)

// ResolveTarget selects a non-colliding relative path for a download and (unless skipped)
// opens and pre-flushes its resumable Session atomically.
//
// Candidate order: relPath, then "<dir><base> (1)<ext>" .. "<dir><base> (999)<ext>".
//   - If a candidate's final file already exists on disk:
//   - when skipSame is true and the regular file size equals meta.Size, it returns
//     (candRel, true, nil, nil) without opening a session;
//   - otherwise that candidate is treated as occupied so existing files are never
//     overwritten, and the search moves to the next candidate.
//   - If the final file does not exist:
//   - when another live Session or a valid .state belonging to a different item already
//     occupies .parts/<cand>.state, the search moves to the next candidate;
//   - when no .state exists, or the .state matches meta (same interrupted item), Open is
//     called to start or resume on candRel and .state is flushed immediately.
func ResolveTarget(root, relPath string, meta ResumeMeta, skipSame bool, opts ...Option) (string, bool, *Session, error) {
	if relPath == "" {
		return "", false, nil, fmt.Errorf("resume: relPath must not be empty")
	}

	resolveMu.Lock()
	defer resolveMu.Unlock()

	slashRel := filepath.ToSlash(relPath)
	dir, file := path.Split(slashRel)
	ext := path.Ext(file)
	base := strings.TrimSuffix(file, ext)

	for i := range 1000 {
		candRel := slashRel
		if i > 0 {
			candRel = dir + fmt.Sprintf("%s (%d)%s", base, i, ext)
		}
		osCandRel := filepath.FromSlash(candRel)
		finalPath, err := resolveWithin(root, osCandRel)
		if err != nil {
			return "", false, nil, err
		}

		info, statErr := os.Stat(finalPath)
		if statErr == nil {
			if skipSame && meta.Size >= 0 && !info.IsDir() && info.Size() == meta.Size {
				return candRel, true, nil, nil
			}
			continue
		}
		if !errors.Is(statErr, os.ErrNotExist) {
			return "", false, nil, fmt.Errorf("resume: stat final %q: %w", finalPath, statErr)
		}

		statePath := filepath.Join(filepath.Dir(finalPath), partsDirName, filepath.Base(finalPath)+".state")
		if _, busy := activeStatePaths[statePath]; busy {
			continue
		}
		if otherItemStateExists(statePath, meta) {
			continue
		}

		s, err := Open(root, osCandRel, meta, opts...)
		if err != nil {
			return "", false, nil, err
		}
		if ferr := s.Flush(); ferr != nil {
			_ = s.Close()
			return "", false, nil, fmt.Errorf("resume: pre-flush state %q: %w", candRel, ferr)
		}
		s.activeKey = statePath
		activeStatePaths[statePath] = struct{}{}
		return candRel, false, s, nil
	}

	return "", false, nil, fmt.Errorf("resume: all 1000 candidate paths exist for %q", relPath)
}

// otherItemStateExists reports whether statePath holds a valid resumeState belonging to a
// different media item than meta (so its resumable progress must not be overwritten).
func otherItemStateExists(statePath string, meta ResumeMeta) bool {
	b, err := os.ReadFile(statePath) // #nosec G304 -- statePath is confined inside rootDir/.parts
	if err != nil {
		return false
	}
	var rs resumeState
	if err := json.Unmarshal(b, &rs); err != nil || rs.V != resumeStateVersion {
		return false
	}
	if rs.Size != meta.Size || rs.Chunk != meta.ChunkSize || rs.DocID != meta.DocID {
		return true
	}
	if rs.DocID == 0 && (rs.Peer != meta.Peer || rs.MsgID != meta.MsgID) {
		return true
	}
	return false
}

// Open opens (or recreates) the part and state files in the .parts directory of
// root/relPath.
//
// Reuse rule: an existing .state is reused to continue the download only when the three
// checks (size/chunk/doc_id) all match, the JSON parses and the bitmap range is legal;
// otherwise the old .state and .part are deleted and the download starts over. The
// reason is observable through Session.ResetReason() and is also logged to stderr.
func Open(root, relPath string, meta ResumeMeta, opts ...Option) (*Session, error) {
	if meta.ChunkSize <= 0 {
		return nil, fmt.Errorf("resume: chunk size must be positive, got %d", meta.ChunkSize)
	}
	if meta.Size < 0 {
		return nil, fmt.Errorf("resume: size must not be negative, got %d", meta.Size)
	}

	cfg := &sessionOptions{}
	for _, o := range opts {
		o(cfg)
	}

	finalPath, err := resolveWithin(root, relPath)
	if err != nil {
		return nil, err
	}

	partsDir := filepath.Join(filepath.Dir(finalPath), partsDirName)
	if err := internfs.EnsureDir(partsDir, resumeDirPerm); err != nil {
		return nil, err
	}

	base := filepath.Base(finalPath)
	partPath := filepath.Join(partsDir, base+".part")
	statePath := filepath.Join(partsDir, base+".state")

	numChunks := (meta.Size + meta.ChunkSize - 1) / meta.ChunkSize

	s := &Session{
		meta:      meta,
		numChunks: numChunks,
		finalPath: finalPath,
		partPath:  partPath,
		statePath: statePath,
		keepState: cfg.keepState,
		mtime:     cfg.mtime,
		lastFlush: time.Now(),
	}

	// 1. try to load an existing .state and run the three checks.
	var loaded *Bitmap
	b, err := os.ReadFile(statePath) // #nosec G304 -- statePath is confined inside rootDir/.parts
	switch {
	case err == nil:
		loaded, s.resetReason = loadAndValidate(b, meta, numChunks)
	case errors.Is(err, os.ErrNotExist):
		// first download: start fresh, not a reset.
	default:
		return nil, fmt.Errorf("resume: read state %q: %w", statePath, err)
	}

	// 2. check that the part file covers the byte ranges of the chunks marked in the bitmap.
	if loaded != nil {
		loaded, s.resetReason = validatePart(partPath, loaded, meta, numChunks, s.resetReason)
	}

	// 3. open the part file: keep existing data if reusable, else truncate and preallocate.
	var f *os.File
	if loaded != nil {
		f, err = os.OpenFile(partPath, os.O_RDWR, resumeFilePerm) // #nosec G304 -- partPath is confined inside rootDir/.parts
		if err != nil {
			return nil, fmt.Errorf("resume: open part %q: %w", partPath, err)
		}
	} else {
		// reuse failed or first download: delete old files and start over (reason already set).
		_ = os.Remove(statePath)
		_ = os.Remove(partPath)
		f, err = os.OpenFile(partPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, resumeFilePerm) // #nosec G304 -- partPath is confined inside rootDir/.parts
		if err != nil {
			return nil, fmt.Errorf("resume: create part %q: %w", partPath, err)
		}
		if s.resetReason != "" {
			fmt.Fprintf(os.Stderr, "resume: %s: restarting download %s\n", s.resetReason, finalPath)
		}
	}

	// Truncate preallocates a sparse file: a new file reaches its logical size at once
	// without using disk blocks;
	// an existing file is aligned to size (extra tail discarded, shortfall zero-filled).
	if err := f.Truncate(meta.Size); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("resume: truncate part %q to %d: %w", partPath, meta.Size, err)
	}

	s.f = f
	s.bm = canonicalBitmap(loaded, numChunks)
	return s, nil
}

// loadAndValidate parses .state and runs the three checks; on failure it returns (nil, reason).
func loadAndValidate(b []byte, meta ResumeMeta, numChunks int64) (*Bitmap, string) {
	var rs resumeState
	if err := json.Unmarshal(b, &rs); err != nil {
		return nil, fmt.Sprintf("state corrupt: %v", err)
	}
	if rs.V != resumeStateVersion {
		return nil, fmt.Sprintf("state version mismatch: got %d want %d", rs.V, resumeStateVersion)
	}
	if rs.Size != meta.Size {
		return nil, fmt.Sprintf("size mismatch: state %d != meta %d", rs.Size, meta.Size)
	}
	if rs.Chunk != meta.ChunkSize {
		return nil, fmt.Sprintf("chunk mismatch: state %d != meta %d", rs.Chunk, meta.ChunkSize)
	}
	if rs.DocID != meta.DocID {
		return nil, fmt.Sprintf("doc_id mismatch: state %d != meta %d", rs.DocID, meta.DocID)
	}
	if rs.Done == nil {
		return nil, "state corrupt: done bitmap missing"
	}
	// bitmap out of range (corrupt or old-format file): such a mark is not valid
	// progress, so reset outright.
	if last, ok := lastSetBit(rs.Done); ok && last >= numChunks {
		return nil, fmt.Sprintf("state corrupt: bitmap index %d out of range (%d chunks)", last, numChunks)
	}
	return rs.Done, ""
}

// validatePart checks that the part file covers every byte range of the chunks marked in
// the bitmap.
// When it does not, the bitmap is dropped and the download restarts (part data is
// missing while the bitmap claims it is done, so resuming would produce a corrupt file).
func validatePart(partPath string, loaded *Bitmap, meta ResumeMeta, numChunks int64, prev string) (*Bitmap, string) {
	info, err := os.Stat(partPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, "part file missing though state exists"
		}
		return nil, fmt.Sprintf("part stat: %v", err)
	}
	last, ok := lastSetBit(loaded)
	if !ok {
		return loaded, prev
	}
	end := (last + 1) * meta.ChunkSize
	if end > meta.Size {
		end = meta.Size
	}
	if end > info.Size() {
		return nil, fmt.Sprintf("part truncated: %d bytes < needed %d for done chunks", info.Size(), end)
	}
	return loaded, prev
}

// canonicalBitmap normalizes a loaded bitmap (possibly too small or too large) to exactly
// numChunks bits.
func canonicalBitmap(loaded *Bitmap, numChunks int64) *Bitmap {
	bm := NewBitmap(int(numChunks))
	if loaded == nil {
		return bm
	}
	limit := int64(loaded.Len())
	if limit > numChunks {
		limit = numChunks
	}
	for i := int64(0); i < limit; i++ {
		if v, err := loaded.Get(int(i)); err == nil && v {
			_ = bm.Set(int(i), true)
		}
	}
	return bm
}

// lastSetBit returns the index of the highest set bit; ok=false when the bitmap is empty.
func lastSetBit(b *Bitmap) (int64, bool) {
	data := b.Bytes()
	for i := len(data) - 1; i >= 0; i-- {
		if data[i] == 0 {
			continue
		}
		for bit := 7; bit >= 0; bit-- {
			if data[i]&(1<<uint(bit)) != 0 {
				return int64(i*8 + bit), true
			}
		}
	}
	return 0, false
}

// resolveWithin normalizes root+relPath into an absolute path and asserts the result is
// still inside root.
// It returns an error when relPath contains ".." and escapes root (path traversal guard).
func resolveWithin(root, relPath string) (string, error) {
	if relPath == "" {
		return "", fmt.Errorf("resume: relPath must not be empty")
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resume: abs root %q: %w", root, err)
	}
	cleaned := filepath.Clean(relPath)
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("resume: relPath %q escapes root", relPath)
	}
	final := filepath.Join(absRoot, cleaned)
	absFinal, err := filepath.Abs(final)
	if err != nil {
		return "", fmt.Errorf("resume: abs %q: %w", final, err)
	}
	if absFinal != absRoot && !strings.HasPrefix(absFinal, absRoot+string(filepath.Separator)) {
		return "", fmt.Errorf("resume: normalized path %q escapes root %q", absFinal, absRoot)
	}
	return absFinal, nil
}

// ResetReason returns why this Open restarted the download; a fresh start returns an empty string.
func (s *Session) ResetReason() string { return s.resetReason }

// Done returns the bitmap of finished chunks (the same object semantics as
// ChunkPlan.Done, which lets the caller read the missing chunks).
//
// The bitmap is not concurrency safe: concurrent MarkDone is protected by the internal
// lock of Session, but a caller that reads the returned value concurrently with workers
// must synchronize itself (or Flush and read from disk, copying with Bytes()).
func (s *Session) Done() *Bitmap {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bm
}

// FinalPath returns the destination path of the final file (updated on Commit if a
// last-moment collision occurred).
func (s *Session) FinalPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finalPath
}

// PartPath returns the path of the .part file.
func (s *Session) PartPath() string { return s.partPath }

// StatePath returns the path of the .state file.
func (s *Session) StatePath() string { return s.statePath }

// ChunkCount returns the total chunk count (⌈size/chunk⌉; 0 when size=0).
func (s *Session) ChunkCount() int64 { return s.numChunks }

// WriteAt writes chunk data at offset off in the part file.
// os.File.WriteAt is itself safe for concurrent use; the caller calls MarkDone afterwards.
func (s *Session) WriteAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	f := s.f
	s.mu.Unlock()
	if f == nil {
		return 0, fmt.Errorf("resume: session closed")
	}
	return f.WriteAt(p, off)
}

// MarkDone marks chunk idx as finished (concurrency safe).
func (s *Session) MarkDone(idx int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.committed {
		return fmt.Errorf("resume: session already committed")
	}
	if idx < 0 || idx >= s.numChunks {
		return fmt.Errorf("resume: chunk index %d out of range [0,%d)", idx, s.numChunks)
	}
	if err := s.bm.Set(int(idx), true); err != nil {
		return fmt.Errorf("resume: mark chunk %d: %w", idx, err)
	}
	s.chunksSince++
	return nil
}

// ShouldFlush reports whether data should be flushed now: every 32 chunks, or at least
// 5s after the last Flush.
// completedChunks is the caller's count of chunks finished since the last Flush;
// Session also counts them itself through MarkDone, and either count reaching the
// threshold triggers a flush.
func (s *Session) ShouldFlush(completedChunks int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.committed {
		return false
	}
	if s.chunksSince >= resumeFlushChunks || completedChunks >= resumeFlushChunks {
		return true
	}
	return time.Since(s.lastFlush) >= resumeFlushInterval
}

// Flush fsyncs the part file first and then persists .state atomically (temp+rename+fsync).
// Ordering matters for crash safety: .state must never mark chunks done before their bytes
// are durable in .part.
// Safe for concurrent calls: flushMu serializes disk I/O while mu is only held for the
// in-memory snapshot, so concurrent WriteAt/MarkDone calls are not blocked on fsync.
// Flush after Commit is a no-op.
func (s *Session) Flush() error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()

	s.mu.Lock()
	if s.committed {
		s.mu.Unlock()
		return nil
	}
	if s.f == nil {
		s.mu.Unlock()
		return fmt.Errorf("resume: session closed")
	}
	f := s.f
	partPath := s.partPath
	statePath := s.statePath
	rs := resumeState{
		V:         resumeStateVersion,
		Size:      s.meta.Size,
		Chunk:     s.meta.ChunkSize,
		Done:      &Bitmap{data: s.bm.Bytes()},
		DocID:     s.meta.DocID,
		Peer:      s.meta.Peer,
		MsgID:     s.meta.MsgID,
		UpdatedAt: time.Now().Unix(),
	}
	s.mu.Unlock()

	if err := f.Sync(); err != nil {
		return fmt.Errorf("resume: sync part %q: %w", partPath, err)
	}
	b, err := json.Marshal(rs)
	if err != nil {
		return fmt.Errorf("resume: marshal state: %w", err)
	}
	if err := internfs.AtomicWriteFile(statePath, b, resumeFilePerm); err != nil {
		return fmt.Errorf("resume: write state %q: %w", statePath, err)
	}
	s.mu.Lock()
	s.chunksSince = 0
	s.lastFlush = time.Now()
	s.mu.Unlock()
	return nil
}

// Commit publishes the final file atomically:
// fsync part → rename to the final path (.parts and the final file share a disk, so
// rename is atomic) →
// fsync the parent directory → Chtimes → delete .state unless keepState is set (the
// default deletes it).
// After Commit, Flush becomes a no-op and MarkDone returns an error.
func (s *Session) Commit() error {
	defer s.releaseActive()
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.committed {
		return nil
	}
	if s.f == nil {
		return fmt.Errorf("resume: session closed")
	}
	if err := s.f.Sync(); err != nil {
		return fmt.Errorf("resume: sync part %q: %w", s.partPath, err)
	}
	if err := s.f.Close(); err != nil {
		return fmt.Errorf("resume: close part %q: %w", s.partPath, err)
	}
	s.f = nil

	targetFinal, err := nextFreeFinalPath(s.finalPath)
	if err != nil {
		return err
	}
	s.finalPath = targetFinal

	if err := os.Rename(s.partPath, s.finalPath); err != nil {
		return fmt.Errorf("resume: rename %q → %q: %w", s.partPath, s.finalPath, err)
	}
	s.committed = true
	// rename itself is atomic, but the directory entry needs an fsync to survive a power loss.
	if df, err := os.Open(filepath.Dir(s.finalPath)); err == nil {
		_ = df.Sync()
		_ = df.Close()
	}

	if !s.mtime.IsZero() {
		if err := os.Chtimes(s.finalPath, s.mtime, s.mtime); err != nil {
			return fmt.Errorf("resume: chtimes %q: %w", s.finalPath, err)
		}
	}

	if !s.keepState {
		if err := os.Remove(s.statePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("resume: remove state %q: %w", s.statePath, err)
		}
	}
	return nil
}

// Abort keeps .part and .state after a failed download so the next run can resume, and
// only fsyncs the part file.
// Session remains usable after Abort (WriteAt/MarkDone/Flush all work until Close).
func (s *Session) Abort() error {
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.committed || s.f == nil {
		return nil
	}
	if err := s.f.Sync(); err != nil {
		return fmt.Errorf("resume: sync part %q: %w", s.partPath, err)
	}
	return nil
}

// Close closes the part file handle. It returns nil after Commit. Safe to call multiple times.
func (s *Session) Close() error {
	defer s.releaseActive()
	s.flushMu.Lock()
	defer s.flushMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	if err != nil {
		return fmt.Errorf("resume: close part %q: %w", s.partPath, err)
	}
	return nil
}

func (s *Session) releaseActive() {
	s.mu.Lock()
	key := s.activeKey
	s.activeKey = ""
	s.mu.Unlock()
	if key != "" {
		resolveMu.Lock()
		delete(activeStatePaths, key)
		resolveMu.Unlock()
	}
}

func nextFreeFinalPath(desired string) (string, error) {
	if _, err := os.Stat(desired); errors.Is(err, os.ErrNotExist) {
		return desired, nil
	} else if err != nil {
		return "", fmt.Errorf("resume: stat final %q: %w", desired, err)
	}

	dir := filepath.Dir(desired)
	file := filepath.Base(desired)
	ext := filepath.Ext(file)
	base := strings.TrimSuffix(file, ext)
	for i := 1; i < 1000; i++ {
		cand := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
		if _, err := os.Stat(cand); errors.Is(err, os.ErrNotExist) {
			return cand, nil
		} else if err != nil {
			return "", fmt.Errorf("resume: stat final %q: %w", cand, err)
		}
	}
	return "", fmt.Errorf("resume: all 1000 candidate paths exist for %q", desired)
}
