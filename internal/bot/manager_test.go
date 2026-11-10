package bot

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/1368129224/tgshelf/internal/state"
)

// newTestStore builds a Store rooted in t.TempDir() (reusing the real state.New so Manager
// semantics are verified against a real Store).
func newTestStore(t *testing.T) *state.Store {
	t.Helper()
	st, err := state.New(filepath.Join(t.TempDir(), "state.json"))
	if err != nil {
		t.Fatalf("create Store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// Core gate: with a semaphore of capacity 1 already held, submit B; Cancel(B); release the
// semaphore; assert B's work was never called and its terminal state is CANCELLED.
// TestManager_ReconcileOrphans: RUNNING/PENDING tasks left over from a restart must be wound
// down, otherwise /status shows ghost tasks and the progress message stays frozen on its last
// frame.
func TestManager_ReconcileOrphans(t *testing.T) {
	st := newTestStore(t)
	if err := st.CreateTask(state.Task{ID: "run-1", ChatID: 1, Link: "a", Tag: "video", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := st.CreateTask(state.Task{ID: "pend-1", ChatID: 1, Link: "b", Tag: "video", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}
	if err := st.TransitionTask("run-1", state.StateRunning); err != nil {
		t.Fatalf("Transition: %v", err)
	}

	m := NewManager(st, 1, nil)
	if n := m.ReconcileOrphans(); n != 2 {
		t.Fatalf("want 2 tasks wound down, got %d", n)
	}
	if got, _ := st.GetTask("run-1"); got.State != state.StateFailed {
		t.Fatalf("a leftover RUNNING task must be marked FAILED, got %s", got.State)
	}
	if got, _ := st.GetTask("pend-1"); got.State != state.StateCancelled {
		t.Fatalf("a leftover PENDING task must be marked CANCELLED, got %s", got.State)
	}
	if n := m.ReconcileOrphans(); n != 0 {
		t.Fatalf("idempotent: the second call must return 0, got %d", n)
	}
}

// TestManager_WorkPanicRecover verifies a panic during task execution neither kills the process
// nor leaves the task unmarked: it becomes FAILED and the semaphore slot is released for later
// tasks.
func TestManager_WorkPanicRecover(t *testing.T) {
	st := newTestStore(t)
	m := NewManager(st, 1, nil)

	// Submit a task that always panics.
	if err := m.Submit(state.Task{ID: "panic-task"}, func(ctx context.Context, t state.Task) error {
		panic("boom")
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	waitForState(t, m, "panic-task", state.StateFailed)

	// Verify the semaphore slot was released and later tasks can run.
	var nextRan atomic.Bool
	if err := m.Submit(state.Task{ID: "normal-task"}, func(ctx context.Context, t state.Task) error {
		nextRan.Store(true)
		return nil
	}); err != nil {
		t.Fatalf("Submit normal: %v", err)
	}
	waitForState(t, m, "normal-task", state.StateDone)
	if !nextRan.Load() {
		t.Fatal("the semaphore was not released after the panic, so later tasks cannot run")
	}
}

func TestManager_QueueCancelPreventsStart(t *testing.T) {
	st := newTestStore(t)
	m := NewManager(st, 1, nil)

	// Occupy the only semaphore slot.
	release := make(chan struct{})
	var started atomic.Int32
	if err := m.Submit(state.Task{ID: "A"}, func(ctx context.Context, t state.Task) error {
		started.Add(1)
		<-release // block until the test lets go
		return nil
	}); err != nil {
		t.Fatalf("Submit A: %v", err)
	}
	// Wait until A is really RUNNING (it has taken the semaphore).
	waitForState(t, m, "A", state.StateRunning)

	// Submit B: it must stay PENDING.
	var bRan atomic.Int32
	if err := m.Submit(state.Task{ID: "B"}, func(ctx context.Context, t state.Task) error {
		bRan.Add(1)
		return nil
	}); err != nil {
		t.Fatalf("Submit B: %v", err)
	}
	bTask, ok := m.Get("B")
	if !ok || bTask.State != state.StatePending {
		t.Fatalf("B must be PENDING, got %+v", bTask)
	}

	// Cancel B while it is queued.
	if _, err := m.Cancel("B"); err != nil {
		t.Fatalf("Cancel B: %v", err)
	}

	// Release the semaphore so the queued goroutine gets the lock - B is CANCELLED by now, so
	// the re-read gate must give up before starting it.
	close(release)

	// Leave an observation window for a possible wrong start.
	time.Sleep(200 * time.Millisecond)

	if got := bRan.Load(); got != 0 {
		t.Fatalf("B's work was called %d times (want 0)", got)
	}
	bTask, _ = m.Get("B")
	if bTask.State != state.StateCancelled {
		t.Fatalf("B's terminal state must be CANCELLED, got %s", bTask.State)
	}
	// A is unaffected and finishes normally.
	_ = started.Load()
}

// Cancel works on a RUNNING task: set CANCELLED, then cancel ctx, and work sees the cancellation.
func TestManager_CancelRunningTask(t *testing.T) {
	st := newTestStore(t)
	m := NewManager(st, 1, nil)

	ctxErr := make(chan error, 1)
	if err := m.Submit(state.Task{ID: "A"}, func(ctx context.Context, t state.Task) error {
		<-ctx.Done()
		ctxErr <- ctx.Err()
		return ctx.Err()
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitForState(t, m, "A", state.StateRunning)

	task, err := m.Cancel("A")
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if task.State != state.StateCancelled {
		t.Fatalf("Cancel must return the CANCELLED state, got %s", task.State)
	}
	select {
	case e := <-ctxErr:
		if !errors.Is(e, context.Canceled) {
			t.Fatalf("work's ctx must be cancelled, got %v", e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("work never received the ctx cancellation")
	}
	waitForState(t, m, "A", state.StateCancelled)
}

// Cancelling a terminal task returns ErrTaskTerminal.
func TestManager_CancelTerminalReturnsErr(t *testing.T) {
	st := newTestStore(t)
	m := NewManager(st, 1, nil)
	if err := m.Submit(state.Task{ID: "A"}, func(ctx context.Context, t state.Task) error {
		return nil
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitForState(t, m, "A", state.StateDone)
	if _, err := m.Cancel("A"); !errors.Is(err, ErrTaskTerminal) {
		t.Fatalf("cancelling a terminal task must return ErrTaskTerminal, got %v", err)
	}
}

// Snapshot is ordered by CreatedAt ascending and includes PENDING and RUNNING.
func TestManager_SnapshotOrder(t *testing.T) {
	st := newTestStore(t)
	m := NewManager(st, 1, nil)

	release := make(chan struct{})
	if err := m.Submit(state.Task{ID: "A"}, func(ctx context.Context, t state.Task) error {
		<-release
		return nil
	}); err != nil {
		t.Fatalf("Submit A: %v", err)
	}
	waitForState(t, m, "A", state.StateRunning)
	// Queue tasks B/C (A already holds the semaphore).
	for _, id := range []string{"B", "C"} {
		if err := m.Submit(state.Task{ID: id}, func(ctx context.Context, t state.Task) error {
			return nil
		}); err != nil {
			t.Fatalf("Submit %s: %v", id, err)
		}
	}
	snap := m.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("Snapshot must hold 3 tasks, got %d", len(snap))
	}
	for i := 1; i < len(snap); i++ {
		if snap[i].CreatedAt.Before(snap[i-1].CreatedAt) {
			t.Fatalf("Snapshot is not ordered by CreatedAt ascending: %v", snap)
		}
	}
	close(release)
}

// A work error moves the task to FAILED.
func TestManager_WorkErrorFails(t *testing.T) {
	st := newTestStore(t)
	m := NewManager(st, 1, nil)
	if err := m.Submit(state.Task{ID: "A"}, func(ctx context.Context, t state.Task) error {
		return errors.New("boom")
	}); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	waitForState(t, m, "A", state.StateFailed)
}

// Concurrent submit/cancel reports no data race under -race (this test is itself covered by
// -race).
func TestManager_ConcurrentSubmitCancel(t *testing.T) {
	st := newTestStore(t)
	m := NewManager(st, 2, nil)

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		id := string(rune('A'+i%26)) + fmtInt(i)
		wg.Add(2)
		go func(id string) {
			defer wg.Done()
			_ = m.Submit(state.Task{ID: id}, func(ctx context.Context, t state.Task) error {
				<-ctx.Done()
				return ctx.Err()
			})
		}(id)
		go func(id string) {
			defer wg.Done()
			time.Sleep(time.Duration(i%5) * 10 * time.Millisecond)
			_, _ = m.Cancel(id)
			_ = m.Snapshot()
		}(id)
	}
	wg.Wait()
}

// waitForState polls until the task reaches the given state (with a timeout).
func waitForState(t *testing.T, m *Manager, id string, want state.TaskState) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if task, ok := m.Get(id); ok && task.State == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	task, _ := m.Get(id)
	t.Fatalf("task %s did not reach %s before the deadline, got %+v", id, want, task)
}

// fmtInt is a decimal formatter for tests (avoids importing strconv just for an alias clash).
func fmtInt(i int) string {
	if i == 0 {
		return "0"
	}
	digits := []byte{}
	for i > 0 {
		digits = append([]byte{byte('0' + i%10)}, digits...)
		i /= 10
	}
	return string(digits)
}
