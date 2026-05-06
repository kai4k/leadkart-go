package ids_test

import (
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/ids"
)

func TestNewV7_ReturnsValidUUIDv7(t *testing.T) {
	t.Parallel()
	id := ids.NewV7()

	if id == uuid.Nil {
		t.Fatal("NewV7 returned nil UUID")
	}
	if got := id.Version(); got != 7 {
		t.Fatalf("UUID version = %d, want 7", got)
	}
}

func TestNewV7_TimeOrdered(t *testing.T) {
	t.Parallel()
	a := ids.NewV7()
	b := ids.NewV7()

	// UUIDv7 layout: first 48 bits are unix milliseconds — earlier IDs sort
	// lexicographically before later ones. Crucial for B-tree FK locality.
	if a.String() >= b.String() {
		t.Fatalf("UUIDv7 not time-ordered: a=%s b=%s", a, b)
	}
}

func TestNewV7_Unique(t *testing.T) {
	t.Parallel()
	const n = 1000
	seen := make(map[uuid.UUID]struct{}, n)
	for i := range n {
		id := ids.NewV7()
		if _, dup := seen[id]; dup {
			t.Fatalf("duplicate UUIDv7 within %d generations: %s", i+1, id)
		}
		seen[id] = struct{}{}
	}
}

// Concurrent generation must not produce collisions or panic.
// Run with `-race` in CI to catch races in the underlying generator.
func TestNewV7_ConcurrentSafe(t *testing.T) {
	t.Parallel()
	const goroutines = 32
	const perGoroutine = 100

	var (
		mu   sync.Mutex
		seen = make(map[uuid.UUID]struct{}, goroutines*perGoroutine)
		wg   sync.WaitGroup
	)
	for range goroutines {
		// Go 1.22 — range-over-int + safe loop-var capture; Go 1.25 — wg.Go.
		wg.Go(func() {
			for range perGoroutine {
				id := ids.NewV7()
				mu.Lock()
				if _, dup := seen[id]; dup {
					mu.Unlock()
					t.Errorf("concurrent collision: %s", id)
					return
				}
				seen[id] = struct{}{}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	if len(seen) != goroutines*perGoroutine {
		t.Fatalf("seen %d unique IDs, want %d", len(seen), goroutines*perGoroutine)
	}
}
