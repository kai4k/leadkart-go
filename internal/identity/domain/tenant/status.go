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
