// chunkplan.go -- chunk plan: splits a file into chunks of at most 1MiB and uses
// internal/state.Bitmap to track which chunks are done, which enables resumable downloads.
//
// # Hard constraints
//
// A single getFile call has limit <= 1MiB and must not cross a 1MiB boundary:
//
//	offset/(1MiB) == (offset+limit-1)/(1MiB)
//
// So chunkSize (the limit of each request) must be <= 1MiB and every chunk must start at a
// multiple of chunkSize -- Offset(idx) = idx*chunkSize satisfies the boundary invariant by
// construction.
//
// # Concurrency contract
//
// state.Bitmap's own Set/Get are not concurrency-safe (that is internal/state's boundary and is
// not changed here), so this type guards every access to Done with sync.Mutex. Missing(),
// MarkDone and IsDone may be called concurrently.
package tg

import (
	"fmt"
	"sync"

	"github.com/1368129224/tgshelf/internal/state"
)

// MaxChunkSize is the maximum number of bytes allowed for a single getFile call (1MiB, a hard
// constraint of the Telegram server).
const MaxChunkSize int64 = 1 << 20

// ChunkPlan describes the chunk layout and completion bitmap of one file.
type ChunkPlan struct {
	// Size is the total file size in bytes (may be 0).
	Size int64
	// ChunkSize is the number of bytes per chunk; 0 < ChunkSize <= MaxChunkSize.
	ChunkSize int64
	// Done is the completion bitmap; bit i corresponds to chunk i. Concurrent access is guarded
	// by this type's mu.
	Done *state.Bitmap

	mu sync.Mutex
}

// NewChunkPlan creates a chunk plan.
//
// size < 0, chunkSize <= 0, chunkSize > MaxChunkSize and done == nil are all errors.
// size == 0 is valid (Chunks()==0, no chunks at all).
func NewChunkPlan(size, chunkSize int64, done *state.Bitmap) (*ChunkPlan, error) {
	if size < 0 {
		return nil, fmt.Errorf("tg: invalid file size %d", size)
	}
	if chunkSize <= 0 {
		return nil, fmt.Errorf("tg: invalid chunk size %d (must be > 0)", chunkSize)
	}
	if chunkSize > MaxChunkSize {
		return nil, fmt.Errorf("tg: chunk size %d exceeds the 1MiB limit", chunkSize)
	}
	if MaxChunkSize%chunkSize != 0 {
		return nil, fmt.Errorf("tg: chunk size %d must divide 1MiB, otherwise chunks would cross a 1MiB boundary (the server rejects that or the behavior is undefined)", chunkSize)
	}
	if done == nil {
		return nil, fmt.Errorf("tg: completion bitmap is nil")
	}
	return &ChunkPlan{Size: size, ChunkSize: chunkSize, Done: done}, nil
}

// Chunks returns the total number of chunks; 0 when Size==0.
func (p *ChunkPlan) Chunks() int64 {
	return (p.Size + p.ChunkSize - 1) / p.ChunkSize
}

// Missing returns the indices of chunks that are not done yet (ascending).
// A chunk whose bitmap read fails (e.g. out of range) counts as not done.
func (p *ChunkPlan) Missing() []int64 {
	n := p.Chunks()
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []int64
	for i := int64(0); i < n; i++ {
		ok, err := p.Done.Get(int(i))
		if err != nil || !ok {
			out = append(out, i)
		}
	}
	return out
}

// Offset computes the (offset, limit) pair of chunk idx.
//
// Boundary-safety reasoning: NewChunkPlan already guarantees that chunkSize divides 1MiB
// (MaxChunkSize%chunkSize==0), so offset = idx*chunkSize always starts at an aligned multiple of a
// 1MiB window; a chunk covers [offset, offset+limit) with limit <= chunkSize <= 1MiB, so it extends
// at most to the next chunkSize grid point -- which is also a multiple of 1MiB -- and never past the
// end of the 1MiB window it belongs to. Hence for every chunk
// offset/(1MiB) == (offset+limit-1)/(1MiB) holds, and the invariant needs no runtime check.
//
// limit is always the full chunkSize (the tail chunk is not shortened to the file size): the real
// server only accepts limits that are "a power of two in 4KiB..1MiB with offset%limit==0" (live
// probe; 503514/503808/491520/1024 all return 400 LIMIT_INVALID), so a shortened value is almost
// certainly invalid; with a full request the server returns the remaining bytes for the tail chunk
// (a short response) and the EOF semantics are handled by Downloader.
// Out-of-range idx returns (0, 0).
func (p *ChunkPlan) Offset(idx int64) (int64, int) {
	if idx < 0 || idx >= p.Chunks() {
		return 0, 0
	}
	return idx * p.ChunkSize, int(p.ChunkSize)
}

// IsDone reports whether chunk idx is done.
func (p *ChunkPlan) IsDone(idx int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idx < 0 || idx >= p.Chunks() {
		return false
	}
	ok, err := p.Done.Get(int(idx))
	return err == nil && ok
}

// CompletedBytes returns the total number of bytes of the done chunks in the bitmap (order does not
// matter, bytes are aggregated per chunk). The tail chunk counts at its actual size (under the short
// response semantics the tail chunk only writes size-offset bytes).
func (p *ChunkPlan) CompletedBytes() int64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	var total int64
	for i := int64(0); i < p.Chunks(); i++ {
		ok, err := p.Done.Get(int(i))
		if err != nil || !ok {
			continue
		}
		limit := p.ChunkSize
		if rem := p.Size - i*p.ChunkSize; rem < limit {
			limit = rem
		}
		total += limit
	}
	return total
}

// MarkDone marks chunk idx as done. An out-of-range idx is an error.
func (p *ChunkPlan) MarkDone(idx int64) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if idx < 0 || idx >= p.Chunks() {
		return fmt.Errorf("tg: chunk index %d out of range (%d chunks in total)", idx, p.Chunks())
	}
	return p.Done.Set(int(idx), true)
}
