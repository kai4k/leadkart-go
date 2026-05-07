package tenant

import "fmt"

// Status enumerates the tenant lifecycle states.
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
// Per `data-retention.md`: deletion is a 30-day-grace lifecycle, not
// a hard table delete — the audit log MUST survive (DPDP §12 / GDPR
// Art. 17(3)(b) / SOC2 CC4.1). PendingDeletion blocks tenant ops but
// keeps the row queryable for the saga + restoration window. Deleted
// is a permanent terminal state — the row stays for FK integrity +
// audit; tenant operations are 410 Gone.
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
	// StatusPendingDeletion — operator initiated deletion; 30-day grace
	// window before HardDelete. RestoreFromDeletion cancels.
	StatusPendingDeletion
	// StatusDeleted — terminal state. Row retained for audit + FK integrity;
	// tenant ops respond 410 Gone.
	StatusDeleted
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
	case StatusPendingDeletion:
		return "pending_deletion"
	case StatusDeleted:
		return "deleted"
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
	case "pending_deletion":
		return StatusPendingDeletion, nil
	case "deleted":
		return StatusDeleted, nil
	default:
		return StatusUnknown, fmt.Errorf("%w: unknown status %q", ErrInvalid, s)
	}
}
