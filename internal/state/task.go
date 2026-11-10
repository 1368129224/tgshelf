package state

import (
	"fmt"
	"time"
)

// TaskState represents one state of the task state machine.
type TaskState string

const (
	// StatePending means the task was created and is waiting to be scheduled.
	StatePending TaskState = "PENDING"
	// StateRunning means the task is running.
	StateRunning TaskState = "RUNNING"
	// StateDone means the task completed successfully (terminal state).
	StateDone TaskState = "DONE"
	// StateFailed means the task failed (terminal state).
	StateFailed TaskState = "FAILED"
	// StateCancelled means the task was cancelled (terminal state).
	StateCancelled TaskState = "CANCELLED"
)

// isTerminal reports whether the state is terminal (a terminal state cannot transition again).
func (s TaskState) isTerminal() bool {
	return s == StateDone || s == StateFailed || s == StateCancelled
}

// ErrInvalidTransition is the sentinel error for an illegal state transition.
// Detect it with errors.Is(err, ErrInvalidTransition).
var ErrInvalidTransition = fmt.Errorf("state: invalid transition")

// InvalidTransitionError carries the concrete from/to states and unwraps to ErrInvalidTransition.
type InvalidTransitionError struct {
	From TaskState
	To   TaskState
}

func (e *InvalidTransitionError) Error() string {
	return fmt.Sprintf("state: invalid transition %s → %s", e.From, e.To)
}

func (e *InvalidTransitionError) Unwrap() error { return ErrInvalidTransition }

// validateTransition checks whether a state transition is legal.
// Rules:
//   - terminal states (DONE/FAILED/CANCELLED) cannot transition;
//   - PENDING can only go to RUNNING or CANCELLED;
//   - RUNNING can only go to DONE, FAILED or CANCELLED.
func validateTransition(from, to TaskState) error {
	if from.isTerminal() {
		return &InvalidTransitionError{From: from, To: to}
	}
	switch from {
	case StatePending:
		if to == StateRunning || to == StateCancelled {
			return nil
		}
	case StateRunning:
		if to == StateDone || to == StateFailed || to == StateCancelled {
			return nil
		}
	}
	return &InvalidTransitionError{From: from, To: to}
}

// ItemKind describes the kind of a download item (file, document and so on).
type ItemKind string

// Item is the state record of a single download item inside a task.
type Item struct {
	Idx        int       `json:"idx"`
	Kind       ItemKind  `json:"kind"`
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	DcID       int       `json:"dc_id"`
	MessageID  int64     `json:"message_id"`
	DialogID   int64     `json:"dialog_id"`
	DocID      int64     `json:"doc_id"`
	State      TaskState `json:"state"`
	Downloaded int64     `json:"downloaded"`
	Error      string    `json:"error,omitempty"`
}

// Task is the complete state record of a single download task.
type Task struct {
	ID         string     `json:"id"`
	ChatID     int64      `json:"chat_id"`
	MessageID  int64      `json:"message_id"`
	Link       string     `json:"link"`
	Tag        string     `json:"tag"`
	State      TaskState  `json:"state"`
	Account    string     `json:"account"`
	Items      []Item     `json:"items"`
	CreatedAt  time.Time  `json:"created_at"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// EntityKind describes the entity kind (user/chat/channel and so on).
type EntityKind string

// Entity is a cached record of a Telegram entity (peer).
type Entity struct {
	Peer     string     `json:"peer"`
	Hash     int64      `json:"hash"`
	Kind     EntityKind `json:"kind"`
	Username string     `json:"username,omitempty"`
	Title    string     `json:"title,omitempty"`
	SyncedAt time.Time  `json:"synced_at"`
}

// storeData is the top-level structure of the JSON file (version=1).
type storeData struct {
	Version  int      `json:"version"`
	Tasks    []Task   `json:"tasks"`
	Entities []Entity `json:"entities"`
}
