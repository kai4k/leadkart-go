package person

import "time"

// LockoutOutcome is what the atomic failed-login registration returns: the count
// and the lock as the database decided them for THIS attempt, from the row it
// just wrote (not from a snapshot read earlier). The login flow answers 423 or
// 401 from it, so concurrent attempts cannot disagree with the stored state.
type LockoutOutcome struct {
	// FailedCount is the consecutive-failure count after this attempt.
	FailedCount int
	// LockedUntil is the lock expiry after this attempt, zero when none was ever set.
	LockedUntil time.Time
	// JustLocked is true for the attempt that crossed the threshold (a lock was not
	// in force before it and is now). Only that attempt records the lock event.
	JustLocked bool
}

// LockedAt reports whether the outcome leaves the account locked at now.
func (o LockoutOutcome) LockedAt(now time.Time) bool {
	return !o.LockedUntil.IsZero() && now.Before(o.LockedUntil)
}

// LockoutClear is what the atomic success registration returns.
type LockoutClear struct {
	// Cleared is true when the counters were reset. False means a lock was in force:
	// the attempt must be answered as locked and open no session.
	Cleared bool
	// LockedUntil is the lock that prevented the clear, zero when Cleared.
	LockedUntil time.Time
}
