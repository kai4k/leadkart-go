package reminder

import "fmt"

// State is the lifecycle position of a [Reminder] per BRD §4.6.
//
// State machine (strict):
//
//	pending → sent      (terminal)
//	pending → cancelled (terminal)
//
// Both `sent` and `cancelled` are terminal — once a reminder fires or
// gets cancelled it never goes back to `pending`. The dashboard
// "today/upcoming/overdue" view reads only `pending`; sent + cancelled
// rows are retained for audit but excluded from the active surface.
//
// Wire-stable lowercase strings — match the CHECK constraint on
// crm.reminders.state in the slice-1 migration.
type State string

const (
	// StatePending is the initial state. A reminder sits here until a
	// user marks it sent OR cancels it (terminal in both directions).
	StatePending State = "pending"

	// StateSent is the terminal-success state. Set by the
	// mark-reminder-sent command (user manually flagged the reminder
	// fired). The dashboard partial index excludes sent rows.
	StateSent State = "sent"

	// StateCancelled is the terminal-revoked state. Set by the
	// cancel-reminder command with a required reason for audit.
	StateCancelled State = "cancelled"
)

// String returns the wire form.
func (s State) String() string { return string(s) }

// IsTerminal reports whether s allows NO further transitions.
func (s State) IsTerminal() bool { return s == StateSent || s == StateCancelled }

// IsValid reports whether s is a known catalogue entry.
func (s State) IsValid() bool {
	switch s {
	case StatePending, StateSent, StateCancelled:
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
