package adapters

import (
	"context"
	"sync"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

// phantomSweepEvery bounds how often a full store is scanned for expired
// entries: an attacker feeding endless fresh keys must not turn every request
// into an O(n) scan.
const phantomSweepEvery = time.Minute

// InMemoryPhantomLockout is the single-process command.PhantomLockout (v0.2
// deploy shape, ADR 0030): the lockout counter for login attempts that have no
// usable account, with the same thresholds as the Person aggregate
// (person.MaxFailedLogins within person.LockoutWindow, locked for
// person.LockoutDuration). Keys are opaque (the handler passes a SHA-256 of the
// email), so no address is held.
//
// Bounded: at maxEntries with nothing expired it stops registering new keys
// (fail-open, matching ADR 0053 best-effort semantics); keys already tracked keep
// counting. State is per process, so with several replicas an unknown email
// locks only when its attempts land on one replica. A shared (Redis) variant is
// follow-up work.
type InMemoryPhantomLockout struct {
	mu        sync.Mutex
	entries   map[string]phantomEntry
	max       int
	lastSweep time.Time
}

type phantomEntry struct {
	count       int
	lastFailed  time.Time
	lockedUntil time.Time
}

// NewInMemoryPhantomLockout wires the store; maxEntries must be at least 1.
func NewInMemoryPhantomLockout(maxEntries int) *InMemoryPhantomLockout {
	if maxEntries < 1 {
		panic("adapters: NewInMemoryPhantomLockout maxEntries must be >= 1")
	}
	return &InMemoryPhantomLockout{entries: make(map[string]phantomEntry), max: maxEntries}
}

// LockedUntil implements command.PhantomLockout.
func (l *InMemoryPhantomLockout) LockedUntil(_ context.Context, key string, now time.Time) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok || !now.Before(e.lockedUntil) {
		return time.Time{}
	}
	return e.lockedUntil
}

// RegisterFailure implements command.PhantomLockout.
func (l *InMemoryPhantomLockout) RegisterFailure(_ context.Context, key string, now time.Time) time.Time {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok && len(l.entries) >= l.max {
		l.sweep(now)
		if len(l.entries) >= l.max {
			return time.Time{}
		}
	}
	if ok && now.Sub(e.lastFailed) > person.LockoutWindow {
		e.count = 0
	}
	e.count++
	e.lastFailed = now
	var locked time.Time
	if e.count >= person.MaxFailedLogins {
		e.lockedUntil = now.Add(person.LockoutDuration)
		e.count = 0
		locked = e.lockedUntil
	}
	l.entries[key] = e
	return locked
}

// sweep drops entries whose window and lock have both ended. At most once per
// phantomSweepEvery. Caller holds the mutex.
func (l *InMemoryPhantomLockout) sweep(now time.Time) {
	if !l.lastSweep.IsZero() && now.Sub(l.lastSweep) < phantomSweepEvery {
		return
	}
	l.lastSweep = now
	for k, e := range l.entries {
		if now.Sub(e.lastFailed) > person.LockoutWindow && !now.Before(e.lockedUntil) {
			delete(l.entries, k)
		}
	}
}
