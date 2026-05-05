package tenant

import "fmt"

// Status enumerates the tenant lifecycle states.
//
// State machine:
//
//	Pending -> Active     (Activate)
//	Pending -> Suspended  (Suspend before activation)
//	Active  -> Suspended  (Suspend, e.g. payment overdue)
//	Suspended -> Active   (Activate / Reactivate)
//
// Deletion is a separate concern handled via the data-retention saga
// per `data-retention.md` doctrine — a soft state distinct from
// suspension.
type Status int

const (
	// StatusUnknown is the zero value — never persisted; rejection target.
	StatusUnknown Status = iota
	// StatusPending — registered but not yet activated.
	StatusPending
	// StatusActive — operating normally.
	StatusActive
	// StatusSuspended — admin or billing suspension; reversible via Activate.
	StatusSuspended
)

// String returns the snake_case form for log + DB serialisation.
func (s Status) String() string {
	switch s {
	case StatusPending:
		return "pending"
	case StatusActive:
		return "active"
	case StatusSuspended:
		return "suspended"
	default:
		return "unknown"
	}
}

// ParseStatus decodes the snake_case form back into a Status.
// Used by the persistence adapter when reading rows.
func ParseStatus(s string) (Status, error) {
	switch s {
	case "pending":
		return StatusPending, nil
	case "active":
		return StatusActive, nil
	case "suspended":
		return StatusSuspended, nil
	default:
		return StatusUnknown, fmt.Errorf("%w: unknown status %q", ErrInvalid, s)
	}
}
