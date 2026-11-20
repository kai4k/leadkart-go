package adapters_test

import (
	"strconv"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

var phantomT0 = time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)

// failN registers n failures one second apart starting at start and returns the
// time of the last one and the lock expiry the last call reported.
func failN(t *testing.T, l *adapters.InMemoryPhantomLockout, key string, n int, start time.Time) (time.Time, time.Time) {
	t.Helper()
	at := start
	var until time.Time
	for i := range n {
		at = start.Add(time.Duration(i) * time.Second)
		until = l.RegisterFailure(t.Context(), key, at)
	}
	return at, until
}

func TestInMemoryPhantomLockout_LocksAtTheRealThreshold(t *testing.T) {
	t.Parallel()
	l := adapters.NewInMemoryPhantomLockout(10)
	last, until := failN(t, l, "k", person.MaxFailedLogins-1, phantomT0)
	if !until.IsZero() || !l.LockedUntil(t.Context(), "k", last).IsZero() {
		t.Fatal("locked before the threshold")
	}
	at := last.Add(time.Second)
	until = l.RegisterFailure(t.Context(), "k", at)
	if want := at.Add(person.LockoutDuration); !until.Equal(want) {
		t.Fatalf("threshold attempt locked until %v, want %v", until, want)
	}
	if got := l.LockedUntil(t.Context(), "k", at.Add(time.Minute)); !got.Equal(until) {
		t.Fatalf("LockedUntil = %v, want %v", got, until)
	}
	if got := l.LockedUntil(t.Context(), "other", at); !got.IsZero() {
		t.Fatalf("another key is locked: %v", got)
	}
}

func TestInMemoryPhantomLockout_AGapBeyondTheWindowResetsTheCount(t *testing.T) {
	t.Parallel()
	l := adapters.NewInMemoryPhantomLockout(10)
	last, _ := failN(t, l, "k", person.MaxFailedLogins-1, phantomT0)
	later := last.Add(person.LockoutWindow + time.Second)
	if until := l.RegisterFailure(t.Context(), "k", later); !until.IsZero() {
		t.Fatalf("an attempt after the window locked: %v", until)
	}
	// The window restarted at `later`: it takes the full threshold again.
	if _, until := failN(t, l, "k", person.MaxFailedLogins-2, later.Add(time.Second)); !until.IsZero() {
		t.Fatalf("locked after %d attempts in the new window", person.MaxFailedLogins-1)
	}
}

func TestInMemoryPhantomLockout_LockExpiresAfterTheLockoutDuration(t *testing.T) {
	t.Parallel()
	l := adapters.NewInMemoryPhantomLockout(10)
	last, until := failN(t, l, "k", person.MaxFailedLogins, phantomT0)
	if until.IsZero() {
		t.Fatal("setup: not locked")
	}
	if l.LockedUntil(t.Context(), "k", until.Add(-time.Second)).IsZero() {
		t.Fatal("lock ended early")
	}
	if got := l.LockedUntil(t.Context(), "k", until); !got.IsZero() {
		t.Fatalf("lock still active at its expiry: %v", got)
	}
	// After expiry a fresh run of failures locks again (the real aggregate does too).
	if _, again := failN(t, l, "k", person.MaxFailedLogins, until.Add(time.Second)); again.IsZero() {
		t.Fatal("could not re-lock after the lock expired")
	}
	_ = last
}

func TestInMemoryPhantomLockout_FullStoreFailsOpenButKeepsTrackedKeys(t *testing.T) {
	t.Parallel()
	l := adapters.NewInMemoryPhantomLockout(3)
	for i := range 3 {
		l.RegisterFailure(t.Context(), "k"+strconv.Itoa(i), phantomT0)
	}
	// A new key does not fit and nothing has expired: fail open, no panic, no lock.
	if until := l.RegisterFailure(t.Context(), "new", phantomT0); !until.IsZero() {
		t.Fatalf("an untracked key locked while the store was full: %v", until)
	}
	// Tracked keys keep counting.
	if _, until := failN(t, l, "k0", person.MaxFailedLogins-1, phantomT0.Add(time.Second)); until.IsZero() {
		t.Fatal("a tracked key stopped locking when the store filled up")
	}
	// Once entries age out the store accepts new keys again.
	later := phantomT0.Add(person.LockoutWindow + person.LockoutDuration + time.Minute)
	l.RegisterFailure(t.Context(), "new", later)
	if _, until := failN(t, l, "new", person.MaxFailedLogins-1, later.Add(time.Second)); until.IsZero() {
		t.Fatal("the store did not recover after entries expired")
	}
}

func TestNewInMemoryPhantomLockout_RejectsANonPositiveBound(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Fatal("want a panic for maxEntries < 1")
		}
	}()
	adapters.NewInMemoryPhantomLockout(0)
}
