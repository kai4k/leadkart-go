package argon2

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// blockingGate returns a gate whose verify parks on release until the test lets it
// go, and a counter of how many verifies are inside the gate right now.
func blockingGate(size int, queue time.Duration) (g *Gate, release chan struct{}, inside *atomic.Int32, peak *atomic.Int32) {
	g = NewGate(size, queue)
	release = make(chan struct{})
	inside, peak = &atomic.Int32{}, &atomic.Int32{}
	g.verify = func(_, _ string) error {
		n := inside.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		inside.Add(-1)
		return nil
	}
	return g, release, inside, peak
}

// waitFor yields until cond holds (a bounded number of scheduler turns, no
// stopwatch): the conditions are reached as soon as the goroutines under test run.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for range 50_000_000 {
		if cond() {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("gave up waiting for %s", what)
}

// Never more than size operations run at once, however many callers arrive.
func TestGate_NeverRunsMoreThanSizeOperationsAtOnce(t *testing.T) {
	t.Parallel()
	g, release, inside, peak := blockingGate(3, 30*time.Second)
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := g.Verify(t.Context(), "p", "s"); err != nil {
				t.Errorf("verify: %v", err)
			}
		}()
	}
	waitFor(t, "three operations inside", func() bool { return inside.Load() == 3 })
	if got := inside.Load(); got != 3 {
		t.Fatalf("%d operations inside a 3-slot gate", got)
	}
	close(release)
	wg.Wait()
	if p := peak.Load(); p != 3 {
		t.Fatalf("peak concurrency %d, want exactly 3", p)
	}
}

// A caller that cannot get a slot within the queue timeout is refused with ErrBusy
// and does NO work: the injected hasher is never called for it.
func TestGate_QueueTimeoutRefusesWithErrBusyAndDoesNoWork(t *testing.T) {
	t.Parallel()
	g, release, inside, _ := blockingGate(1, 20*time.Millisecond)
	done := make(chan error, 1)
	go func() { done <- g.Verify(t.Context(), "holder", "s") }()
	waitFor(t, "the holder inside", func() bool { return inside.Load() == 1 })

	var calls atomic.Int32
	g.verify = func(_, _ string) error { calls.Add(1); return nil }
	if err := g.Verify(t.Context(), "queued", "s"); !errors.Is(err, ErrBusy) {
		t.Fatalf("err = %v, want ErrBusy", err)
	}
	if _, err := g.Hash(t.Context(), "queued"); !errors.Is(err, ErrBusy) {
		t.Fatalf("Hash err = %v, want ErrBusy", err)
	}
	if calls.Load() != 0 {
		t.Fatal("a refused caller still did the work")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A request whose context ends while queued leaves the queue with the context's
// error (the client is gone), not ErrBusy, and frees nothing it never held.
func TestGate_ContextEndsTheWait(t *testing.T) {
	t.Parallel()
	g, release, inside, _ := blockingGate(1, 30*time.Second)
	done := make(chan error, 1)
	go func() { done <- g.Verify(t.Context(), "holder", "s") }()
	waitFor(t, "the holder inside", func() bool { return inside.Load() == 1 })

	ctx, cancel := context.WithCancel(t.Context())
	res := make(chan error, 1)
	go func() { res <- g.Verify(ctx, "queued", "s") }()
	cancel()
	if err := <-res; !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	// Already-cancelled contexts never enter at all.
	if err := g.Verify(ctx, "late", "s"); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled err = %v", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// The slot the holder had is free again.
	g.verify = func(_, _ string) error { return nil }
	if err := g.Verify(t.Context(), "after", "s"); err != nil {
		t.Fatalf("slot leaked: %v", err)
	}
}

// A slot is returned when the operation fails or panics-free returns an error.
func TestGate_ReleasesTheSlotWhenTheOperationFails(t *testing.T) {
	t.Parallel()
	g := NewGate(1, 20*time.Millisecond)
	g.verify = func(_, _ string) error { return ErrMismatch }
	for range 5 {
		if err := g.Verify(t.Context(), "p", "s"); !errors.Is(err, ErrMismatch) {
			t.Fatalf("err = %v, want the operation's own error", err)
		}
	}
}

// A nil gate is unbounded (tests, tools) and still does the real work.
func TestGate_NilIsUnboundedAndWorks(t *testing.T) {
	t.Parallel()
	var g *Gate
	h, err := g.Hash(t.Context(), "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if err := g.Verify(t.Context(), "correct horse battery staple", h); err != nil {
		t.Fatalf("verify own hash: %v", err)
	}
	if err := g.Verify(t.Context(), "wrong", h); !errors.Is(err, ErrMismatch) {
		t.Fatalf("wrong password err = %v", err)
	}
	if g.Size() != 0 {
		t.Fatal("nil gate has no size")
	}
}

func TestNewGate_RejectsNonsense(t *testing.T) {
	t.Parallel()
	for name, f := range map[string]func(){
		"zero size":    func() { NewGate(0, time.Second) },
		"zero timeout": func() { NewGate(1, 0) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: want a panic", name)
				}
			}()
			f()
		}()
	}
}
