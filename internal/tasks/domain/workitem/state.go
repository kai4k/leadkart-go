package workitem

import "fmt"

// State is the lifecycle position of a [WorkItem] per BRD §6.8.
//
// State machine:
//
//	pending → in_progress → completed (terminal)
//	                     ↘  cancelled (terminal)
//	pending → overdue   → completed (terminal)
//	                     ↘  cancelled (terminal)
//	pending → cancelled (terminal)
//
// Notes:
//   - in_progress → overdue is allowed; the overdue scanner re-flags
//     in-flight work items whose due_at slipped.
//   - overdue → in_progress is intentionally not allowed; the user
//     re-engaging an overdue task uses Complete (closes the task) or
//     Cancel (drops it). Re-opening adds a fresh task instead.
//   - Reassign / ChangePriority are allowed on any non-terminal state
//     (handled by the aggregate methods, not the state machine itself).
//
// Wire-stable lowercase strings — match the CHECK constraint on
// tasks.work_items.state in migration 20260604000001.
type State string

const (
	// StatePending — created, not yet started.
	StatePending State = "pending"
	// StateInProgress — assignee has started but not closed it.
	StateInProgress State = "in_progress"
	// StateCompleted — assignee marked done (terminal).
	StateCompleted State = "completed"
	// StateOverdue — due_at passed without completion; the overdue
	// scanner flips pending/in_progress rows here every 15 minutes.
	StateOverdue State = "overdue"
	// StateCancelled — task explicitly dropped before completion
	// (terminal). Cancellation_reason is required.
	StateCancelled State = "cancelled"
)

// String returns the wire form.
func (s State) String() string { return string(s) }

// IsTerminal reports whether the state allows NO further transitions.
func (s State) IsTerminal() bool { return s == StateCompleted || s == StateCancelled }

// IsOpen reports whether the work item is countable as "open work"
// for dashboards (pending, in_progress, or overdue).
func (s State) IsOpen() bool {
	return s == StatePending || s == StateInProgress || s == StateOverdue
}

// IsValid reports whether s is a known catalogue entry.
func (s State) IsValid() bool {
	switch s {
	case StatePending, StateInProgress, StateCompleted, StateOverdue, StateCancelled:
		return true
	}
	return false
}

// ParseState turns an untrusted string into a [State] or returns
// [ErrInvalid] wrapped with the bad value.
func ParseState(raw string) (State, error) {
	s := State(raw)
	if !s.IsValid() {
		return "", fmt.Errorf("%w: state %q not in catalogue", ErrInvalid, raw)
	}
	return s, nil
}
