package clock_test

import (
	"sync"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/clock"
)

// Clock tests share global state — NOT parallel. The thread-safety test
// below exercises concurrent calls within one test; that's the only
// parallelism we want.

func TestNow_DefaultsToWallClock(t *testing.T) {
	t.Cleanup(clock.Reset)

	clock.Reset()
	before := time.Now().UTC()
	got := clock.Now()
	after := time.Now().UTC()

	if got.Before(before) || got.After(after) {
		t.Fatalf("Now() = %v, expected within [%v, %v]", got, before, after)
	}
	if got.Location() != time.UTC {
		t.Fatalf("Now().Location() = %v, want UTC", got.Location())
	}
}

func TestSet_OverridesNow(t *testing.T) {

	t.Cleanup(clock.Reset)

	frozen := time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC)
	clock.Set(frozen)

	got := clock.Now()
	if !got.Equal(frozen) {
		t.Fatalf("Now() = %v, want %v", got, frozen)
	}
}

func TestReset_RestoresWallClock(t *testing.T) {

	t.Cleanup(clock.Reset)

	clock.Set(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	clock.Reset()

	got := clock.Now()
	wallNow := time.Now().UTC()
	if got.Year() != wallNow.Year() {
		t.Fatalf("after Reset(): Now() = %v, expected close to %v", got, wallNow)
	}
}

// Concurrent reads while Set/Reset run must not race.
// Run with `-race` in CI.
func TestSet_IsThreadSafe(t *testing.T) {

	t.Cleanup(clock.Reset)

	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = clock.Now()
		}()
		go func(i int) {
			defer wg.Done()
			clock.Set(time.Date(2026, 5, 5, 0, 0, i, 0, time.UTC))
		}(i)
	}
	wg.Wait()
}

func TestNow_AlwaysUTC(t *testing.T) {

	t.Cleanup(clock.Reset)

	mumbaiTime := time.Date(2026, 5, 5, 12, 0, 0, 0, time.FixedZone("IST", 5*3600+30*60))
	clock.Set(mumbaiTime)

	got := clock.Now()
	if got.Location() != time.UTC {
		t.Fatalf("Now().Location() = %v, want UTC (clock must normalise)", got.Location())
	}
	if !got.Equal(mumbaiTime) {
		t.Fatalf("Now() = %v, want equivalent to %v", got, mumbaiTime)
	}
}
