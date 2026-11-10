package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// helpers

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newTestStoreWithWriter(t *testing.T, wfn writeFunc) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := newWithWriter(filepath.Join(dir, "state.json"), wfn)
	if err != nil {
		t.Fatalf("newWithWriter: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func makeTask(id string) Task {
	return Task{
		ID:        id,
		ChatID:    100,
		MessageID: 200,
		Link:      "https://t.me/test/" + id,
		Tag:       "tag1",
		State:     StatePending,
		Account:   "acct1",
		CreatedAt: time.Now(),
	}
}

// 1. State machine tests

func TestStateMachine_LegalTransitions(t *testing.T) {
	cases := []struct {
		from TaskState
		to   TaskState
	}{
		{StatePending, StateRunning},
		{StatePending, StateCancelled},
		{StateRunning, StateDone},
		{StateRunning, StateFailed},
		{StateRunning, StateCancelled},
	}
	for _, c := range cases {
		t.Run(string(c.from)+"->"+string(c.to), func(t *testing.T) {
			if err := validateTransition(c.from, c.to); err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestStateMachine_IllegalTransitions(t *testing.T) {
	cases := []struct {
		from TaskState
		to   TaskState
	}{
		{StatePending, StateDone},
		{StatePending, StateFailed},
		{StatePending, StatePending},
		{StateRunning, StatePending},
		{StateRunning, StateRunning},
		{StateDone, StateRunning},
		{StateDone, StateCancelled},
		{StateFailed, StateRunning},
		{StateCancelled, StatePending},
	}
	for _, c := range cases {
		t.Run(string(c.from)+"->"+string(c.to), func(t *testing.T) {
			err := validateTransition(c.from, c.to)
			if err == nil {
				t.Fatalf("expected error for %s->%s, got nil", c.from, c.to)
			}
			if !errors.Is(err, ErrInvalidTransition) {
				t.Errorf("expected ErrInvalidTransition, got %T: %v", err, err)
			}
			var te *InvalidTransitionError
			if !errors.As(err, &te) {
				t.Errorf("expected *InvalidTransitionError, got %T", err)
			} else {
				if te.From != c.from || te.To != c.to {
					t.Errorf("wrong from/to: got %s->%s", te.From, te.To)
				}
			}
		})
	}
}

func TestStateMachine_TransitionTask_UpdatesTimestamps(t *testing.T) {
	s := newTestStore(t)
	_ = s.CreateTask(makeTask("ts1"))

	if err := s.TransitionTask("ts1", StateRunning); err != nil {
		t.Fatalf("transition to RUNNING: %v", err)
	}
	got, _ := s.GetTask("ts1")
	if got.StartedAt == nil {
		t.Error("StartedAt should be set after RUNNING transition")
	}
	if got.FinishedAt != nil {
		t.Error("FinishedAt should not be set after RUNNING transition")
	}

	if err := s.TransitionTask("ts1", StateDone); err != nil {
		t.Fatalf("transition to DONE: %v", err)
	}
	got, _ = s.GetTask("ts1")
	if got.FinishedAt == nil {
		t.Error("FinishedAt should be set after DONE transition")
	}

	if err := s.TransitionTask("ts1", StateCancelled); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("expected ErrInvalidTransition from terminal state, got %v", err)
	}
}

// 2. Debounce test

func TestDebounce_MultipleWritesSingleFlush(t *testing.T) {
	var writeCount int64

	wfn := func(path string, data []byte) error {
		atomic.AddInt64(&writeCount, 1)
		return defaultWriteFn(path, data)
	}

	s := newTestStoreWithWriter(t, wfn)
	_ = s.CreateTask(makeTask("debounce-base"))

	const N = 200
	start := time.Now()
	for i := 0; i < N; i++ {
		_ = s.UpdateTask("debounce-base", func(t *Task) error {
			t.Tag = fmt.Sprintf("tag-%d", i)
			return nil
		})
	}
	elapsed := time.Since(start)

	if err := s.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	finalCount := atomic.LoadInt64(&writeCount)
	t.Logf("%d updates / %d actual writes (elapsed=%v)", N, finalCount, elapsed)

	if finalCount > 3 {
		t.Errorf("debounce failure: 200 updates caused %d writes (expected <= 3)", finalCount)
	}
	if finalCount < 1 {
		t.Errorf("no writes happened (expected >= 1)")
	}
}

// 3. Restart / Load recovery

func TestRestart_LoadRecovery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	s1, err := New(path)
	if err != nil {
		t.Fatalf("New s1: %v", err)
	}
	_ = s1.CreateTask(makeTask("restart-task"))
	s1.UpsertEntity(Entity{Peer: "peer123", Hash: 42, Kind: "user"})
	if err = s1.Flush(); err != nil {
		t.Fatalf("Flush s1: %v", err)
	}
	_ = s1.Close()

	s2, err := New(path)
	if err != nil {
		t.Fatalf("New s2: %v", err)
	}
	defer func() { _ = s2.Close() }()

	task, ok := s2.GetTask("restart-task")
	if !ok {
		t.Fatal("task not found after restart")
	}
	if task.State != StatePending {
		t.Errorf("unexpected state %s", task.State)
	}

	entity, ok := s2.LookupEntity("peer123")
	if !ok {
		t.Fatal("entity not found after restart")
	}
	if entity.Hash != 42 {
		t.Errorf("unexpected hash %d", entity.Hash)
	}
}

// 4. 100 goroutines concurrent safety

func TestConcurrency_100Goroutines(t *testing.T) {
	s := newTestStore(t)

	for i := 0; i < 100; i++ {
		if err := s.CreateTask(makeTask(fmt.Sprintf("conc-%d", i))); err != nil {
			t.Fatalf("CreateTask: %v", err)
		}
	}

	var wg sync.WaitGroup
	wg.Add(100)
	for i := 0; i < 100; i++ {
		i := i
		go func() {
			defer wg.Done()
			id := fmt.Sprintf("conc-%d", i)
			if err := s.TransitionTask(id, StateRunning); err != nil {
				return
			}
			_ = s.UpdateTask(id, func(t *Task) error {
				t.Error = fmt.Sprintf("goroutine %d", i)
				return nil
			})
			_ = s.TransitionTask(id, StateDone)
		}()
	}
	wg.Wait()

	tasks := s.AllTasks()
	if len(tasks) != 100 {
		t.Errorf("expected 100 tasks, got %d", len(tasks))
	}

	if err := s.Flush(); err != nil {
		t.Errorf("Flush after concurrent updates: %v", err)
	}
	t.Logf("100 goroutine concurrent test passed, %d tasks", len(tasks))
}

// 5. File not found (first run)

func TestLoad_FileNotExist(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nonexistent.json")

	s, err := New(path)
	if err != nil {
		t.Fatalf("New with nonexistent file: %v", err)
	}
	defer func() { _ = s.Close() }()

	if tasks := s.AllTasks(); len(tasks) != 0 {
		t.Errorf("expected empty tasks, got %d", len(tasks))
	}
}

// 6. Corrupt JSON handling

func TestLoad_CorruptJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	corrupt := []byte(`{version:1,tasks:[{INVALID`)
	if err := os.WriteFile(path, corrupt, 0o600); err != nil {
		t.Fatalf("write corrupt: %v", err)
	}

	_, err := New(path)
	if err == nil {
		t.Fatal("expected error for corrupt JSON, got nil")
	}
	t.Logf("corrupt JSON error: %v", err)

	b, err2 := os.ReadFile(path)
	if err2 != nil {
		t.Fatalf("read after corrupt: %v", err2)
	}
	if string(b) != string(corrupt) {
		t.Error("corrupt file should not be overwritten")
	}
}

// 7. Bitmap boundary tests

func TestBitmap_Basic(t *testing.T) {
	b := NewBitmap(64)
	if b.Len() != 64 {
		t.Errorf("Len expected 64, got %d", b.Len())
	}
	for _, idx := range []int{0, 7, 8, 63} {
		v, err := b.Get(idx)
		if err != nil || v {
			t.Errorf("bit %d should be 0 initially", idx)
		}
	}
}

func TestBitmap_SetGet_Boundaries(t *testing.T) {
	b := NewBitmap(128)
	cases := []int{0, 7, 8, 63, 64, 127}
	for _, i := range cases {
		t.Run(fmt.Sprintf("bit%d", i), func(t *testing.T) {
			if err := b.Set(i, true); err != nil {
				t.Fatalf("Set(%d, true): %v", i, err)
			}
			v, err := b.Get(i)
			if err != nil {
				t.Fatalf("Get(%d): %v", i, err)
			}
			if !v {
				t.Errorf("bit %d should be 1 after Set", i)
			}
			if err := b.Set(i, false); err != nil {
				t.Fatalf("Set(%d, false): %v", i, err)
			}
			v, err = b.Get(i)
			if err != nil || v {
				t.Errorf("bit %d should be 0 after clear", i)
			}
		})
	}
}

func TestBitmap_NegativeIndex(t *testing.T) {
	b := NewBitmap(8)
	if err := b.Set(-1, true); err == nil {
		t.Error("expected error for negative index in Set")
	}
	if _, err := b.Get(-1); err == nil {
		t.Error("expected error for negative index in Get")
	}
}

func TestBitmap_OutOfRange_PanicFree(t *testing.T) {
	b := NewBitmap(8)
	if _, err := b.Get(8); err == nil {
		t.Error("expected error for out-of-range Get(8)")
	}
	if _, err := b.Get(100); err == nil {
		t.Error("expected error for out-of-range Get(100)")
	}
}

func TestBitmap_Set_AutoGrow(t *testing.T) {
	b := NewBitmap(0)
	if err := b.Set(64, true); err != nil {
		t.Fatalf("Set(64): %v", err)
	}
	v, err := b.Get(64)
	if err != nil || !v {
		t.Errorf("bit 64 should be 1 after auto-grow, err=%v v=%v", err, v)
	}
}

func TestBitmap_Count(t *testing.T) {
	b := NewBitmap(64)
	for _, i := range []int{0, 7, 8, 63} {
		_ = b.Set(i, true)
	}
	if c := b.Count(); c != 4 {
		t.Errorf("Count expected 4, got %d", c)
	}
}

func TestBitmap_MarshalUnmarshal(t *testing.T) {
	b := NewBitmap(64)
	_ = b.Set(0, true)
	_ = b.Set(7, true)
	_ = b.Set(8, true)
	_ = b.Set(63, true)

	data, err := b.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}

	var b2 Bitmap
	if err = b2.UnmarshalJSON(data); err != nil {
		t.Fatalf("UnmarshalJSON: %v", err)
	}

	for _, i := range []int{0, 7, 8, 63} {
		v, _ := b2.Get(i)
		if !v {
			t.Errorf("bit %d should be 1 after unmarshal", i)
		}
	}
	if b2.Count() != 4 {
		t.Errorf("Count after unmarshal: expected 4, got %d", b2.Count())
	}
}

func TestBitmap_BitOrder(t *testing.T) {
	b := NewBitmap(8)
	_ = b.Set(0, true)
	bytes := b.Bytes()
	if bytes[0] != 0x01 {
		t.Errorf("bit0 should set byte[0]=0x01, got 0x%02x", bytes[0])
	}

	b2 := NewBitmap(8)
	_ = b2.Set(7, true)
	bytes2 := b2.Bytes()
	if bytes2[0] != 0x80 {
		t.Errorf("bit7 should set byte[0]=0x80, got 0x%02x", bytes2[0])
	}

	b3 := NewBitmap(16)
	_ = b3.Set(8, true)
	bytes3 := b3.Bytes()
	if bytes3[0] != 0x00 || bytes3[1] != 0x01 {
		t.Errorf("bit8 should set byte[1]=0x01, got [0]=0x%02x [1]=0x%02x", bytes3[0], bytes3[1])
	}
}

// 8. Entity operations

func TestEntity_UpsertLookup(t *testing.T) {
	s := newTestStore(t)
	e := Entity{Peer: "peer1", Hash: 12345, Kind: "channel", Title: "Test Channel"}
	s.UpsertEntity(e)

	got, ok := s.LookupEntity("peer1")
	if !ok {
		t.Fatal("entity not found")
	}
	if got.Hash != 12345 {
		t.Errorf("hash mismatch: %d", got.Hash)
	}
	if got.SyncedAt.IsZero() {
		t.Error("SyncedAt should be auto-set")
	}

	e2 := Entity{Peer: "peer1", Hash: 99999, Kind: "channel", Title: "Updated"}
	s.UpsertEntity(e2)
	got2, _ := s.LookupEntity("peer1")
	if got2.Hash != 99999 {
		t.Errorf("upsert: hash should be 99999, got %d", got2.Hash)
	}
}

func TestEntity_LookupNotFound(t *testing.T) {
	s := newTestStore(t)
	if _, ok := s.LookupEntity("nonexistent"); ok {
		t.Error("should return false for nonexistent entity")
	}
}

// 9. PurgeTerminal

func TestPurgeTerminal(t *testing.T) {
	s := newTestStore(t)
	ids := []string{"p1", "p2", "p3"}
	for _, id := range ids {
		_ = s.CreateTask(makeTask(id))
		_ = s.TransitionTask(id, StateRunning)
		_ = s.TransitionTask(id, StateDone)
	}
	_ = s.CreateTask(makeTask("p4"))

	past := time.Now().Add(-2 * time.Hour)
	for _, id := range ids {
		_ = s.UpdateTask(id, func(t *Task) error {
			t.FinishedAt = &past
			return nil
		})
	}

	n := s.PurgeTerminal(time.Hour)
	if n != 3 {
		t.Errorf("expected 3 purged, got %d", n)
	}
	remaining := s.AllTasks()
	if len(remaining) != 1 || remaining[0].ID != "p4" {
		t.Errorf("unexpected remaining tasks: %v", remaining)
	}
}

// 10. ActiveTasks

func TestActiveTasks(t *testing.T) {
	s := newTestStore(t)
	_ = s.CreateTask(makeTask("a1"))
	_ = s.CreateTask(makeTask("a2"))
	_ = s.CreateTask(makeTask("a3"))
	_ = s.TransitionTask("a3", StateRunning)
	_ = s.TransitionTask("a3", StateDone)

	active := s.ActiveTasks()
	if len(active) != 2 {
		t.Errorf("expected 2 active tasks, got %d", len(active))
	}
}

// 11. Duplicate task ID

func TestCreateTask_DuplicateID(t *testing.T) {
	s := newTestStore(t)
	_ = s.CreateTask(makeTask("dup"))
	if err := s.CreateTask(makeTask("dup")); err == nil {
		t.Error("expected error for duplicate task ID")
	}
}

// 12. UpdateTask not found

func TestUpdateTask_NotFound(t *testing.T) {
	s := newTestStore(t)
	err := s.UpdateTask("missing", func(t *Task) error { return nil })
	if err == nil {
		t.Error("expected error for missing task")
	}
}

// 13. Close stops background goroutine

func TestClose_StopsBackground(t *testing.T) {
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = s.CreateTask(makeTask("close-test"))
	if err = s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// After Close, doneCh is closed; Flush falls back to direct flush
	if err = s.Flush(); err != nil {
		t.Logf("Flush after Close: %v (acceptable)", err)
	}
}

// 14. JSON version field

func TestStore_JSONVersionField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	s, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_ = s.CreateTask(makeTask("ver-test"))
	_ = s.Flush()
	_ = s.Close()

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var d storeData
	if err = json.Unmarshal(b, &d); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if d.Version != storeVersion {
		t.Errorf("version: expected %d, got %d", storeVersion, d.Version)
	}
}

// 15. GetTask returns a copy

func TestGetTask_ReturnsCopy(t *testing.T) {
	s := newTestStore(t)
	orig := makeTask("copy-test")
	orig.Items = []Item{{Idx: 0, Name: "file.mp4"}}
	_ = s.CreateTask(orig)

	got, _ := s.GetTask("copy-test")
	got.Tag = "mutated"
	got.Items[0].Name = "mutated.mp4"

	orig2, _ := s.GetTask("copy-test")
	if orig2.Tag == "mutated" {
		t.Error("GetTask should return a copy, not a reference")
	}
	if orig2.Items[0].Name == "mutated.mp4" {
		t.Error("GetTask items should be a copy")
	}
}

// 16. InvalidTransitionError message contains from/to

func TestInvalidTransitionError_Message(t *testing.T) {
	err := validateTransition(StatePending, StateDone)
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if msg == "" {
		t.Error("error message should not be empty")
	}
	t.Logf("error message: %s", msg)
	hasFrom := false
	hasPENDING := false
	hasDONE := false
	for i := 0; i <= len(msg)-7; i++ {
		if msg[i:i+7] == "PENDING" {
			hasPENDING = true
		}
	}
	for i := 0; i <= len(msg)-4; i++ {
		if msg[i:i+4] == "DONE" {
			hasDONE = true
		}
	}
	hasFrom = hasPENDING && hasDONE
	if !hasFrom {
		t.Errorf("error message should contain from/to states: %s", msg)
	}
}

// 17. Bitmap empty marshal

func TestBitmap_EmptyMarshal(t *testing.T) {
	b := NewBitmap(0)
	data, err := b.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON empty: %v", err)
	}
	var b2 Bitmap
	if err = b2.UnmarshalJSON(data); err != nil {
		t.Fatalf("UnmarshalJSON empty: %v", err)
	}
	if b2.Count() != 0 {
		t.Error("empty bitmap should have 0 bits set")
	}
}

// 18. Concurrent UpsertEntity

func TestConcurrency_UpsertEntity(t *testing.T) {
	s := newTestStore(t)
	var wg sync.WaitGroup
	wg.Add(100)
	for i := 0; i < 100; i++ {
		i := i
		go func() {
			defer wg.Done()
			s.UpsertEntity(Entity{
				Peer:  fmt.Sprintf("peer-%d", i%10),
				Hash:  int64(i),
				Kind:  "user",
				Title: fmt.Sprintf("User %d", i),
			})
		}()
	}
	wg.Wait()
	count := 0
	for i := 0; i < 10; i++ {
		if _, ok := s.LookupEntity(fmt.Sprintf("peer-%d", i)); ok {
			count++
		}
	}
	if count != 10 {
		t.Errorf("expected 10 entities, got %d", count)
	}
	t.Logf("concurrent UpsertEntity test passed, %d entities", count)
}
