// Package state manages the lifecycle state machine of download tasks
// (PENDING / RUNNING / DONE / FAILED / CANCELLED).
//
// # Design notes
//
// ## File layout
//
//   - doc.go   — package documentation and design notes
//   - task.go  — task and entity data structures, state machine, sentinel errors
//   - bitmap.go — Bitmap resume bitmap ([]byte + bit operations)
//   - store.go  — Store struct: JSON persistence, debounced writes, concurrency safety
//
// ## Why Bitmap lives in this package
//
// Bitmap is the type of the downloaded field of a task Item (the resume progress
// bitmap). It is tightly coupled to task state, so keeping it in the same package
// avoids an import cycle and lets the Item struct embed the type directly.
// A separate subpackage (e.g. internal/bitmap) would make both callers and this
// package import it without any benefit.
//
// ## Bit order convention (Bitmap.Set/Get)
//
//   - Bit index i maps to byte data[i/8], bit i%8 within that byte (LSB = bit 0).
//   - That is: i=0 → data[0] bit0; i=7 → data[0] bit7; i=8 → data[1] bit0.
//   - This matches encoding/binary little-endian byte order and the LSB convention
//     of Go math/bits as well.
//
// ## Debounced writes
//
// Store keeps an internal "dirty" flag (dirty bool) and one background goroutine
// (triggered by a ticker once per second). Write operations only set dirty=true;
// when the background goroutine sees dirty it writes to disk and clears the flag,
// so N writes within the same second produce a single I/O. Flush() forces an
// immediate write; Close() stops the background goroutine and performs a final
// Flush.
//
// ## Concurrency safety
//
// All exported methods protect in-memory state with a sync.RWMutex. Writes to disk
// (AtomicWriteFile) are serialized outside the lock, so I/O never holds the lock
// for long.
package state
