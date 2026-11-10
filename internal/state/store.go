package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	internfs "github.com/1368129224/tgshelf/internal/fs"
)

const (
	storeVersion     = 1
	debounceInterval = time.Second
	filePerm         = 0o600
)

// writeFunc is the type of the actual persistence function; tests can inject a counter.
type writeFunc func(path string, data []byte) error

// Store is a concurrency-safe JSON state store.
//
// Every exported method may be called safely from any number of goroutines.
// Writes are debounced internally: several writes within the same second trigger
// only one I/O.
type Store struct {
	path string
	mu   sync.RWMutex

	tasks    map[string]*Task
	entities map[string]*Entity

	// Debounced persistence
	dirty     bool
	writeFn   writeFunc // injectable, defaults to fs.AtomicWriteFile
	ticker    *time.Ticker
	stopCh    chan struct{}
	doneCh    chan struct{}
	flushCh   chan chan error // synchronous Flush requests
	closeOnce sync.Once
}

// defaultWriteFn writes atomically through internal/fs.AtomicWriteFile.
func defaultWriteFn(path string, data []byte) error {
	return internfs.AtomicWriteFile(path, data, filePerm)
}

// New creates and initializes a Store and starts the background persistence goroutine.
// It restores state from the file through Load(); a missing file yields an empty Store.
func New(path string) (*Store, error) {
	s := &Store{
		path:     path,
		tasks:    make(map[string]*Task),
		entities: make(map[string]*Entity),
		writeFn:  defaultWriteFn,
		ticker:   time.NewTicker(debounceInterval),
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
		flushCh:  make(chan chan error, 4),
	}
	if err := s.Load(); err != nil {
		return nil, err
	}
	go s.background()
	return s, nil
}

// newWithWriter is for tests only: it injects a custom writeFunc.
func newWithWriter(path string, wfn writeFunc) (*Store, error) {
	s := &Store{
		path:     path,
		tasks:    make(map[string]*Task),
		entities: make(map[string]*Entity),
		writeFn:  wfn,
		ticker:   time.NewTicker(debounceInterval),
		stopCh:   make(chan struct{}),
		doneCh:   make(chan struct{}),
		flushCh:  make(chan chan error, 4),
	}
	if err := s.Load(); err != nil {
		return nil, err
	}
	go s.background()
	return s, nil
}

// background is the background persistence goroutine (exactly one, to prevent concurrent writes).
func (s *Store) background() {
	defer close(s.doneCh)
	for {
		select {
		case <-s.stopCh:
			// final write before stopping
			_ = s.flush()
			return
		case <-s.ticker.C:
			s.mu.Lock()
			dirty := s.dirty
			s.mu.Unlock()
			if dirty {
				_ = s.flush()
			}
		case replyCh := <-s.flushCh:
			replyCh <- s.flush()
		}
	}
}

// flush serializes and writes to disk and clears the dirty flag (the caller need not hold the lock).
func (s *Store) flush() error {
	s.mu.Lock()
	data := s.snapshot()
	s.dirty = false
	s.mu.Unlock()

	b, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("state.Store: marshal: %w", err)
	}
	if err = s.writeFn(s.path, b); err != nil {
		return fmt.Errorf("state.Store: write %q: %w", s.path, err)
	}
	return nil
}

// snapshot builds a storeData snapshot while the lock is held (the caller must hold
// the write or read lock).
// The write lock of mu is taken directly here (already held when called from flush).
func (s *Store) snapshot() storeData {
	tasks := make([]Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		cp := *t
		if t.Items != nil {
			cp.Items = make([]Item, len(t.Items))
			copy(cp.Items, t.Items)
		}
		tasks = append(tasks, cp)
	}
	entities := make([]Entity, 0, len(s.entities))
	for _, e := range s.entities {
		cp := *e
		entities = append(entities, cp)
	}
	return storeData{
		Version:  storeVersion,
		Tasks:    tasks,
		Entities: entities,
	}
}

// markDirty marks the state as changed (the caller must hold the write lock).
func (s *Store) markDirty() { s.dirty = true }

// -------------------------------------------------------------------
// Load / Save
// -------------------------------------------------------------------

// Load loads state from the file.
// A missing file returns nil (the first run uses an empty Store).
// Corrupt JSON returns a clear error and does not overwrite the original file.
func (s *Store) Load() error {
	b, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil // first run
		}
		return fmt.Errorf("state.Store: read %q: %w", s.path, err)
	}

	var d storeData
	if err = json.Unmarshal(b, &d); err != nil {
		// corrupt file: keep it and return an error that carries the path
		return fmt.Errorf("state.Store: corrupt JSON (file kept for inspection) %q: %w", s.path, err)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.tasks = make(map[string]*Task, len(d.Tasks))
	for i := range d.Tasks {
		t := d.Tasks[i]
		s.tasks[t.ID] = &t
	}
	s.entities = make(map[string]*Entity, len(d.Entities))
	for i := range d.Entities {
		e := d.Entities[i]
		s.entities[e.Peer] = &e
	}
	return nil
}

// Save forces an immediate write to disk (an alias of Flush, for external callers).
func (s *Store) Save() error { return s.Flush() }

// Flush forces an immediate write to disk (process exit / tests).
// It synchronizes with the background goroutine through flushCh so that only one
// goroutine writes to disk.
// If the background goroutine has already exited (after Close), Flush writes
// synchronously in the caller instead.
func (s *Store) Flush() error {
	// first check whether the background goroutine has exited (non-blocking)
	select {
	case <-s.doneCh:
		// already exited, write synchronously
		return s.flush()
	default:
	}

	// the background goroutine is still running, delegate to it
	replyCh := make(chan error, 1)
	select {
	case s.flushCh <- replyCh:
		// wait for the background goroutine, also watching doneCh (in case it exits while we wait)
		select {
		case err := <-replyCh:
			return err
		case <-s.doneCh:
			return s.flush()
		}
	case <-s.doneCh:
		return s.flush()
	}
}

// Close stops the background goroutine and performs a final Flush. Safe to call multiple times.
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.ticker.Stop()
		close(s.stopCh)
	})
	<-s.doneCh
	return nil
}

// -------------------------------------------------------------------
// Task operations
// -------------------------------------------------------------------

// CreateTask creates a new task (initial state PENDING).
// It returns an error if a task with the same ID already exists.
func (s *Store) CreateTask(t Task) error {
	if t.ID == "" {
		return fmt.Errorf("state.Store: task ID must not be empty")
	}
	if t.State == "" {
		t.State = StatePending
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if _, ok := s.tasks[t.ID]; ok {
		return fmt.Errorf("state.Store: task %q already exists", t.ID)
	}
	cp := t
	s.tasks[t.ID] = &cp
	s.markDirty()
	return nil
}

// GetTask returns a copy of the task by ID. It returns false when the task does not exist.
func (s *Store) GetTask(id string) (Task, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	t, ok := s.tasks[id]
	if !ok {
		return Task{}, false
	}
	cp := *t
	if t.Items != nil {
		cp.Items = make([]Item, len(t.Items))
		copy(cp.Items, t.Items)
	}
	return cp, true
}

// UpdateTask updates the task in place with fn (fn runs under the lock and may
// modify *Task).
// fn may change any field; state transitions must go through TransitionTask.
// It returns an error when the task does not exist.
func (s *Store) UpdateTask(id string, fn func(*Task) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tasks[id]
	if !ok {
		return fmt.Errorf("state.Store: task %q does not exist", id)
	}
	if err := fn(t); err != nil {
		return err
	}
	s.markDirty()
	return nil
}

// TransitionTask performs a state machine transition: it validates legality, then
// updates the state field and the timestamps.
func (s *Store) TransitionTask(id string, to TaskState) error {
	return s.UpdateTask(id, func(t *Task) error {
		if err := validateTransition(t.State, to); err != nil {
			return err
		}
		t.State = to
		now := time.Now()
		switch to {
		case StateRunning:
			t.StartedAt = &now
		case StateDone, StateFailed, StateCancelled:
			t.FinishedAt = &now
		}
		return nil
	})
}

// ActiveTasks returns a list of copies of every non-terminal task (PENDING/RUNNING).
func (s *Store) ActiveTasks() []Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Task, 0)
	for _, t := range s.tasks {
		if !t.State.isTerminal() {
			cp := *t
			if t.Items != nil {
				cp.Items = make([]Item, len(t.Items))
				copy(cp.Items, t.Items)
			}
			out = append(out, cp)
		}
	}
	return out
}

// AllTasks returns a list of copies of all tasks (used by /status).
func (s *Store) AllTasks() []Task {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Task, 0, len(s.tasks))
	for _, t := range s.tasks {
		cp := *t
		if t.Items != nil {
			cp.Items = make([]Item, len(t.Items))
			copy(cp.Items, t.Items)
		}
		out = append(out, cp)
	}
	return out
}

// PurgeTerminal deletes every task that is terminal and finished longer than olderThan ago.
func (s *Store) PurgeTerminal(olderThan time.Duration) int {
	cutoff := time.Now().Add(-olderThan)
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for id, t := range s.tasks {
		if t.State.isTerminal() && t.FinishedAt != nil && t.FinishedAt.Before(cutoff) {
			delete(s.tasks, id)
			n++
		}
	}
	if n > 0 {
		s.markDirty()
	}
	return n
}

// -------------------------------------------------------------------
// Entity operations
// -------------------------------------------------------------------

// UpsertEntity inserts or updates the entity cache (keyed by peer).
func (s *Store) UpsertEntity(e Entity) {
	if e.SyncedAt.IsZero() {
		e.SyncedAt = time.Now()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cp := e
	s.entities[e.Peer] = &cp
	s.markDirty()
}

// LookupEntity looks up the entity cache by peer. It returns false when absent.
func (s *Store) LookupEntity(peer string) (Entity, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.entities[peer]
	if !ok {
		return Entity{}, false
	}
	return *e, true
}
