package membership

import "fmt"

// Status enumerates the Membership lifecycle states.
//
// Active ↔ Inactive only. There is NO Pending state for Memberships —
// admin onboarding creates the Membership in Active state directly. The
// pending/verification flow happens at the Person/Email layer upstream.
type Status int

const (
	// StatusUnknown is the zero value — never persisted.
	StatusUnknown Status = iota
	// StatusActive — operating normally.
	StatusActive
	// StatusInactive — deactivated; reversible via Reactivate.
	StatusInactive
)

// String returns the snake_case form for log + DB serialisation.
func (s Status) String() string {
	switch s {
	case StatusActive:
		return "active"
	case StatusInactive:
		return "inactive"
	default:
		return "unknown"
	}
}

// ParseStatus decodes the snake_case form back into a Status.
func ParseStatus(s string) (Status, error) {
	switch s {
	case "active":
		return StatusActive, nil
	case "inactive":
		return StatusInactive, nil
	default:
		return StatusUnknown, fmt.Errorf("%w: unknown status %q", ErrInvalid, s)
	}
}
