// Task manager and the "cancel while queued" gate.
//
// Gate semantics (a task cancelled while still queued must never start):
//
//   - Submit registers PENDING with store.CreateTask **at call time** (visible and cancellable
//     while queued);
//   - a background goroutine waits for the semaphore; once it gets it, it must re-read the state
//     under mu and give up before starting if the state is no longer PENDING (never call work);
//   - Cancel is mutually exclusive with "re-read + switch to RUNNING" under the same mu: it sets
//     CANCELLED first and only then calls the cancel func, so an already started task stops
//     naturally through ctx cancellation.
package bot

import (
	"context"
	"errors"
	"sort"
	"sync"

	"github.com/1368129224/tgshelf/internal/state"
	"go.uber.org/zap"
)

// ErrTaskTerminal means the target task is already terminal and cannot be cancelled again.
var ErrTaskTerminal = errors.New("bot: task already finished")

// Manager is an in-process task manager: PENDING queueing plus RUNNING execution, with a
// concurrency gate.
type Manager struct {
	mu     sync.Mutex
	store  *state.Store
	sem    chan struct{}
	live   map[string]context.CancelFunc
	logger *zap.Logger
}

// NewManager creates a manager. maxConcurrency is the cap on simultaneously running tasks
// (values <= 0 are treated as 1).
func NewManager(st *state.Store, maxConcurrency int, logger *zap.Logger) *Manager {
	if logger == nil {
		logger = zap.NewNop()
	}
	n := maxConcurrency
	if n <= 0 {
		n = 1
	}
	return &Manager{
		store:  st,
		sem:    make(chan struct{}, n),
		live:   make(map[string]context.CancelFunc),
		logger: logger,
	}
}

// Submit registers a PENDING task and returns at call time; a background goroutine waits for the
// semaphore and then starts work according to the gate semantics.
//
// t.State is forced to PENDING before being written to the store (CreateTask's default
// behaviour). After work returns, the task moves to a terminal state only if it is still RUNNING
// (CANCELLED/FAILED are decided by the Cancel path or later card handling); the semaphore is
// released in every case.
func (m *Manager) Submit(t state.Task, work func(ctx context.Context, t state.Task) error) error {
	t.State = state.StatePending
	if err := m.store.CreateTask(t); err != nil {
		return err
	}
	go m.run(t.ID, work)
	return nil
}

// run is Submit's background body: wait for the semaphore -> re-read under the lock -> start or
// give up.
func (m *Manager) run(id string, work func(ctx context.Context, t state.Task) error) {
	m.sem <- struct{}{}
	defer func() { <-m.sem }()

	// -- Gate: mutually exclusive with Cancel on mu --
	m.mu.Lock()
	t, ok := m.store.GetTask(id)
	if !ok || t.State != state.StatePending {
		// Cancelled while queued (or changed externally): never start.
		m.mu.Unlock()
		return
	}
	if err := m.store.TransitionTask(id, state.StateRunning); err != nil {
		// Unreachable in theory: PENDING->RUNNING is always legal; give up defensively.
		m.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	m.live[id] = cancel
	m.mu.Unlock()
	defer cancel()

	defer func() {
		if r := recover(); r != nil {
			m.logger.Error("task panicked, recovered automatically",
				zap.String("task_id", id),
				zap.Any("recover", r),
				zap.Stack("stack"),
			)
			m.mu.Lock()
			delete(m.live, id)
			_ = m.store.TransitionTask(id, state.StateFailed)
			m.mu.Unlock()
		}
	}()

	err := work(ctx, t)

	// -- Winding down: move to a terminal state atomically with live-map removal; the semaphore
	// is released either way (already done by the defer). If work already called Finish, this
	// transition is an idempotent no-op. --
	m.mu.Lock()
	delete(m.live, id)
	if err == nil {
		_ = m.store.TransitionTask(id, state.StateDone)
	} else {
		_ = m.store.TransitionTask(id, state.StateFailed)
	}
	m.mu.Unlock()
}

// Finish atomically transitions a RUNNING task to a terminal state (StateDone or StateFailed)
// under m.mu, returning the task's resulting state. Callers should invoke Finish before editing
// the terminal Telegram message so a concurrent Cancel cannot slip in and overwrite it.
func (m *Manager) Finish(id string, target state.TaskState) state.TaskState {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.live, id)
	t, ok := m.store.GetTask(id)
	if !ok {
		return target
	}
	if t.State == state.StateRunning {
		if err := m.store.TransitionTask(id, target); err == nil {
			return target
		}
	}
	return t.State
}

// Cancel cancels a task. It works for both PENDING and RUNNING:
// under mu it is mutually exclusive with Submit's "re-read + switch to RUNNING"; it sets the state
// to CANCELLED first and then calls the registered cancel func (a queued task has no cancel func,
// so setting the state is enough to block its start).
// A missing task returns nil (indistinguishable from the idempotent view); a terminal task returns
// ErrTaskTerminal.
func (m *Manager) Cancel(id string) (state.Task, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	t, ok := m.store.GetTask(id)
	if !ok {
		return state.Task{}, nil
	}
	if t.State == state.StateDone || t.State == state.StateFailed || t.State == state.StateCancelled {
		return t, ErrTaskTerminal
	}
	if err := m.store.TransitionTask(id, state.StateCancelled); err != nil {
		return t, err
	}
	if cancel, live := m.live[id]; live {
		cancel() // set the state before cancelling so work's teardown always sees CANCELLED
	}
	updated, _ := m.store.GetTask(id)
	return updated, nil
}

// ReconcileOrphans winds down non-terminal tasks left over from before a restart.
//
// After a process restart every worker goroutine is gone, but state.json still holds RUNNING and
// PENDING records. Left alone, /status keeps showing ghost tasks and the progress message stays
// frozen on its last frame: the user neither gets a result nor can retry.
//
// State machine constraint: PENDING may only go to RUNNING or CANCELLED; RUNNING may go to FAILED.
// So queued tasks are marked CANCELLED and running tasks FAILED (their downloads really were
// interrupted).
func (m *Manager) ReconcileOrphans() int {
	n := 0
	for _, t := range m.store.ActiveTasks() {
		target := state.StateFailed
		if t.State == state.StatePending {
			target = state.StateCancelled
		}
		if err := m.store.TransitionTask(t.ID, target); err != nil {
			continue
		}
		n++
	}
	return n
}

// Get returns a copy of the current task by ID.
func (m *Manager) Get(id string) (state.Task, bool) {
	return m.store.GetTask(id)
}

// Snapshot returns every non-terminal task (PENDING and RUNNING), ordered by CreatedAt ascending.
func (m *Manager) Snapshot() []state.Task {
	tasks := m.store.ActiveTasks()
	sort.SliceStable(tasks, func(i, j int) bool {
		return tasks[i].CreatedAt.Before(tasks[j].CreatedAt)
	})
	return tasks
}
