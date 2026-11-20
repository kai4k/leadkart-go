package argon2

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockingGateWith is blockingGate over a gate built with options.
func blockingGateWith(size int, queue time.Duration, opts ...GateOption) (g *Gate, release chan struct{}, inside *atomic.Int32) {
	g = NewGate(size, queue, opts...)
	release = make(chan struct{})
	inside = &atomic.Int32{}
	g.verify = func(_, _ string) error {
		inside.Add(1)
		<-release
		inside.Add(-1)
		return nil
	}
	return g, release, inside
}

// One client key holds at most perClient slots; with no line of its own (WithMaxWaiting(0)) the next is
// refused at once with ErrBusy, BEFORE it queues, so a source cannot also fill the
// queue; another key is untouched; and the bookkeeping map is empty when all are done.
func TestGate_OneClientCannotHoldMoreThanItsShare(t *testing.T) {
	t.Parallel()
	g, release, inside := blockingGateWith(8, 30*time.Second, WithPerClient(2), WithMaxWaiting(0))
	ctxA := WithClient(t.Context(), "198.51.100.1")
	ctxB := WithClient(t.Context(), "198.51.100.2")

	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if err := g.Verify(ctxA, "p", "s"); err != nil {
				t.Errorf("verify: %v", err)
			}
		})
	}
	waitFor(t, "two operations of client A inside", func() bool { return inside.Load() == 2 })

	// Refused immediately (the queue timeout is 30 s: a wait would hang the test).
	if err := g.Verify(ctxA, "p", "s"); !errors.Is(err, ErrBusy) {
		t.Fatalf("third operation of one client = %v, want ErrBusy", err)
	}
	if got := inside.Load(); got != 2 {
		t.Fatalf("%d inside after the refused call, want 2", got)
	}
	// Another client is not affected.
	wg.Go(func() {
		if err := g.Verify(ctxB, "p", "s"); err != nil {
			t.Errorf("client B: %v", err)
		}
	})
	waitFor(t, "client B inside", func() bool { return inside.Load() == 3 })

	close(release)
	wg.Wait()
	if n := g.trackedClients(); n != 0 {
		t.Fatalf("%d client keys still tracked after every operation finished", n)
	}
	// And the client can work again.
	if err := g.Verify(ctxA, "p", "s"); err != nil {
		t.Fatalf("after release: %v", err)
	}
}

// A refused (per-client) call does not leave a count behind: refuse many, then the
// client's own two release and it can take two again.
func TestGate_RefusalsDoNotLeakClientCounts(t *testing.T) {
	t.Parallel()
	g, release, inside := blockingGateWith(8, 30*time.Second, WithPerClient(1), WithMaxWaiting(0))
	ctx := WithClient(t.Context(), "k")
	var wg sync.WaitGroup
	wg.Go(func() { _ = g.Verify(ctx, "p", "s") })
	waitFor(t, "holder inside", func() bool { return inside.Load() == 1 })
	for range 50 {
		if err := g.Verify(ctx, "p", "s"); !errors.Is(err, ErrBusy) {
			t.Fatalf("got %v, want ErrBusy", err)
		}
	}
	close(release)
	wg.Wait()
	if n := g.trackedClients(); n != 0 {
		t.Fatalf("%d keys tracked", n)
	}
}

// With anonymous load saturating the anonymous share, authenticated work still gets
// a slot; anonymous work is refused (queue timeout) while the reserved slots stay free.
func TestGate_AuthenticatedWorkKeepsItsReservedShare(t *testing.T) {
	t.Parallel()
	const size, reserved = 4, 2
	g, release, inside := blockingGateWith(size, 20*time.Millisecond, WithReserved(reserved))

	var wg sync.WaitGroup
	for range size - reserved {
		wg.Go(func() { _ = g.Verify(t.Context(), "p", "s") })
	}
	waitFor(t, "the anonymous share inside", func() bool { return inside.Load() == size-reserved })

	// The anonymous share is full: more anonymous work is refused although slots are free.
	if err := g.Verify(t.Context(), "p", "s"); !errors.Is(err, ErrBusy) {
		t.Fatalf("anonymous work beyond its share = %v, want ErrBusy", err)
	}
	// Authenticated work gets in, as many as are reserved.
	auth := WithAuthenticated(t.Context())
	for range reserved {
		wg.Go(func() {
			if err := g.Verify(auth, "p", "s"); err != nil {
				t.Errorf("authenticated: %v", err)
			}
		})
	}
	waitFor(t, "the reserved slots used", func() bool { return inside.Load() == size })
	// Everything is taken now: even authenticated work is refused.
	if err := g.Verify(auth, "p", "s"); !errors.Is(err, ErrBusy) {
		t.Fatalf("authenticated work on a full gate = %v, want ErrBusy", err)
	}
	close(release)
	wg.Wait()
}

// Anonymous callers that give up (context done) while queued return their anonymous
// place: a later anonymous call is not starved by the leak.
func TestGate_AGivenUpWaiterReturnsItsPlaces(t *testing.T) {
	t.Parallel()
	g, release, inside := blockingGateWith(2, 30*time.Second, WithReserved(1), WithPerClient(2))
	var wg sync.WaitGroup
	wg.Go(func() { _ = g.Verify(WithClient(t.Context(), "a"), "p", "s") })
	waitFor(t, "holder inside", func() bool { return inside.Load() == 1 })

	ctx, cancel := context.WithCancel(WithClient(t.Context(), "b"))
	done := make(chan error, 1)
	go func() { done <- g.Verify(ctx, "p", "s") }()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if n := g.trackedClients(); n != 1 {
		t.Fatalf("%d clients tracked, want only the holder", n)
	}
	close(release)
	wg.Wait()
	if err := g.Verify(t.Context(), "p", "s"); err != nil {
		t.Fatalf("anonymous call after the waiter left: %v", err)
	}
}
