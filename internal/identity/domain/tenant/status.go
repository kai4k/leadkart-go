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
//	Active|Suspended -> PendingDeletion  (MarkForDeletion — operator-initiated)
//	PendingDeletion  -> Active           (RestoreFromDeletion within grace window)
//	PendingDeletion  -> Deleted          (HardDelete after grace, saga-driven)
//
// Deletion is a 30-day-grace lifecycle (data-retention.md) — audit log must
// survive (DPDP §12, GDPR Art. 17(3)(b), SOC2 CC4.1). PendingDeletion keeps
// the row queryable for the saga; Deleted is terminal (row retained for FK
// integrity; ops return 410 Gone).
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
	// StatusPendingDeletion — 30-day grace window; RestoreFromDeletion cancels.
	StatusPendingDeletion
	// StatusDeleted — terminal; row retained for audit + FK integrity.
	StatusDeleted
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
	case StatusPendingDeletion:
		return "pending_deletion"
	case StatusDeleted:
		return "deleted"
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
//   - pending_deletion: no. The company can be restored within the grace window
//     (operators act into it to do so); its members wait.
//   - deleted: no (terminal).
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
	case "pending_deletion":
		return StatusPendingDeletion, nil
	case "deleted":
		return StatusDeleted, nil
	default:
		return StatusUnknown, fmt.Errorf("%w: unknown status %q", ErrInvalid, s)
	}
}
