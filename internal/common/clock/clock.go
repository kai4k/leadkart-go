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
// Safe for concurrent use.
func Set(t time.Time) {
	utc := t.UTC()
	fixed.Store(&utc)
}

// Reset restores the wall clock as the source of Now().
// Always pair Set() with t.Cleanup(clock.Reset) in tests.
func Reset() {
	fixed.Store(nil)
}
