package tenant

import "fmt"

// Status enumerates tenant lifecycle states.
//
// State machine:
//
//	Pending          -> Active           (Activate)
//	Pending          -> Suspended        (Suspend before activation)
//	Active           -> Suspended        (Suspend, e.g. payment overdue)
//	Suspended        -> Active           (Activate / Reactivate)
//
// A company is never deleted (owner decision 2026-11-23, ADR 0071): the only
// states are pending, active and suspended.
type Status int

const (
	// StatusUnknown is the zero value; never persisted.
	StatusUnknown Status = iota
	// StatusPending — registered but not yet activated.
	StatusPending
	// StatusActive — operating normally.
	StatusActive
	// StatusSuspended — admin or billing suspension; reversible via Activate.
	StatusSuspended
)

// String returns the snake_case form used for logging and DB serialisation.
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

// MembersMaySignIn reports whether the members of a tenant in this status may open
// or keep a session (login, refresh). It is the ONE predicate behind both. Owner
// ruling 2026-11-21: "If a Company is not Active no one can login from that org."
// So:
//
//   - active: yes. The only one.
//   - pending: no. A company an operator registers is created ACTIVE (controller
//     default, ADR 0069), so pending is produced by no current route; it stays in the
//     domain for a future self-signup flow whose approval activates it.
//   - suspended: no. An operator or billing suspension ends the members' access.
//   - unknown (the zero value, never persisted): no, fail closed.
//
// Platform operators acting INTO a tenant do not pass through this predicate: their
// session belongs to the platform tenant (see authn.TenantScope).
func (s Status) MembersMaySignIn() bool {
	return s == StatusActive
}

// ParseStatus decodes the snake_case string produced by [Status.String].
// Used by the persistence adapter on row reads.
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
