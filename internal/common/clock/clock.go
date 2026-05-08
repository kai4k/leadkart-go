// Package clock provides a deterministic time source for the application.
//
// Production calls clock.Now() — wall clock in UTC.
// Tests override via clock.Set(t) and clean up with clock.Reset().
//
// Per ADR 0033 ideology: NO `IClock` interface. Go canon is package-level
// functions for cross-cutting deterministic time, mutable for tests via
// thread-safe setters.
//
// Wall clock canonicalised to UTC at the API boundary so the rest of the
// codebase never needs to think about local timezones (Indian PCD pharma
// accidents from accidental IST/UTC mixing are a known risk in the .NET
// version's audit trail).
package clock

import (
	"sync/atomic"
	"time"
)

// fixed holds an optional override; nil means "use wall clock".
//
// atomic.Pointer chosen over sync.RWMutex because reads dominate (Now()
// is hot path; Set() runs only at test setup). atomic.Pointer is lock-free
// for reads.
var fixed atomic.Pointer[time.Time] //nolint:gochecknoglobals // canonical singleton clock

// activeFreezes counts the number of in-flight Set calls that have not
// yet been paired with a Reset. Refcounting protects parallel tests:
// when test A and test B both call Set concurrently and one finishes
// + runs its t.Cleanup(Reset) early, the other test's freeze must
// survive until ITS own Reset runs. Without this, A's Cleanup would
// silently un-freeze the clock under B's feet — observed as
// time-sensitive token validations failing intermittently when run
// with `-count=N` or `-shuffle=on`.
//
// Reads are NOT lock-free against Set/Reset interleavings: Now() reads
// `fixed` only, which atomic.Pointer keeps consistent. The counter
// only gates Reset's pointer-clearing decision.
var activeFreezes atomic.Int64 //nolint:gochecknoglobals // pairs with fixed

// Now returns the current time. Always UTC.
//
// Returns the wall clock unless Set has been called; then returns the
// frozen time until Reset is called.
func Now() time.Time {
	if p := fixed.Load(); p != nil {
		return p.UTC()
	}
	return time.Now().UTC()
}

// Set freezes the clock at t (canonicalised to UTC) for tests.
//
// Subsequent Now() calls return t until Reset is invoked.
// Safe for concurrent use; multiple concurrent freezes compose — see
// [activeFreezes] for the parallel-test contract.
func Set(t time.Time) {
	utc := t.UTC()
	fixed.Store(&utc)
	activeFreezes.Add(1)
}

// Reset releases ONE freeze. The wall clock resumes only when every
// outstanding Set has been paired with a Reset (i.e. the in-flight
// freeze count returns to zero). Idempotent when called without a
// prior Set — safe for tests that defensively Reset at startup.
//
// Always pair Set() with t.Cleanup(clock.Reset) in tests.
func Reset() {
	for {
		old := activeFreezes.Load()
		if old <= 0 {
			// No active freezes; treat as idempotent reset of stale state.
			fixed.Store(nil)
			// Cap counter at 0 — defensive against stray Resets so the
			// next Set+Reset pair lands on a clean baseline.
			if old < 0 {
				activeFreezes.CompareAndSwap(old, 0)
			}
			return
		}
		if activeFreezes.CompareAndSwap(old, old-1) {
			if old == 1 {
				fixed.Store(nil)
			}
			return
		}
	}
}
