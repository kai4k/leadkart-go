package argon2

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// orderedGate is a gate whose verify announces the password it works for on entered
// and then parks until that password's own latch is closed, so a test releases the
// operations one by one and observes the order they ran in.
type orderedGate struct {
	*Gate
	entered chan string
	mu      sync.Mutex
	latch   map[string]chan struct{}
	calls   atomic.Int32
}

func newOrderedGate(size int, queue time.Duration, opts ...GateOption) *orderedGate {
	o := &orderedGate{Gate: NewGate(size, queue, opts...), entered: make(chan string, 64), latch: map[string]chan struct{}{}} // arch-test:bounded-justified test fixture; at most a few dozen announcements
	o.verify = func(pw, _ string) error {
		o.calls.Add(1)
		o.entered <- pw
		<-o.latchFor(pw)
		return nil
	}
	return o
}

func (o *orderedGate) latchFor(pw string) chan struct{} {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.latch[pw] == nil {
		o.latch[pw] = make(chan struct{})
	}
	return o.latch[pw]
}

func (o *orderedGate) let(pw string) { close(o.latchFor(pw)) }

func (o *orderedGate) start(wg *sync.WaitGroup, ctx context.Context, pw string, res chan<- error) {
	wg.Go(func() { res <- o.Verify(ctx, pw, "s") })
}

// A client's excess operations wait for its running ones instead of failing: with a
// share of two, the third and the fourth queue and then run, in arrival order.
func TestGate_ExcessOperationsOfOneClientWaitTheirTurnInOrder(t *testing.T) {
	t.Parallel()
	o := newOrderedGate(8, 30*time.Second, WithPerClient(2))
	ctx := WithClient(t.Context(), "198.51.100.1")
	res := make(chan error, 8)
	var wg sync.WaitGroup
	o.start(&wg, ctx, "p1", res)
	o.start(&wg, ctx, "p2", res)
	got := []string{<-o.entered, <-o.entered}
	o.start(&wg, ctx, "p3", res)
	waitFor(t, "p3 queued", func() bool { return o.waiting("198.51.100.1") == 1 })
	o.start(&wg, ctx, "p4", res)
	waitFor(t, "p4 queued", func() bool { return o.waiting("198.51.100.1") == 2 })
	if len(o.entered) != 0 {
		t.Fatal("a queued operation ran before a place was free")
	}
	o.let(got[0])
	got = append(got, <-o.entered)
	o.let(got[1])
	got = append(got, <-o.entered)
	if got[2] != "p3" || got[3] != "p4" {
		t.Fatalf("queued operations ran as %v, want p3 then p4", got[2:])
	}
	o.let("p3")
	o.let("p4")
	wg.Wait()
	close(res)
	for err := range res {
		if err != nil {
			t.Fatalf("an operation failed: %v", err)
		}
	}
	if n := o.trackedClients(); n != 0 {
		t.Fatalf("%d client keys tracked after everything finished", n)
	}
}

// At most 8 operations wait per client: 2 running + 8 waiting are served, the 11th is
// refused at once (the queue timeout is 30 s: a wait would hang the test). Another
// client is served meanwhile.
func TestGate_AClientBeyondTheWaitingBoundIsRefusedAtOnce(t *testing.T) {
	t.Parallel()
	o := newOrderedGate(16, 30*time.Second, WithPerClient(2))
	ctx := WithClient(t.Context(), "A")
	res := make(chan error, 16)
	var wg sync.WaitGroup
	names := []string{"a1", "a2", "a3", "a4", "a5", "a6", "a7", "a8", "a9", "a10"}
	for i, n := range names {
		o.start(&wg, ctx, n, res)
		if i >= 2 {
			waitFor(t, n+" queued", func() bool { return o.waiting("A") == i-1 })
		}
	}
	if err := o.Verify(ctx, "a11", "s"); !errors.Is(err, ErrBusy) {
		t.Fatalf("the 11th operation of one client = %v, want ErrBusy", err)
	}
	if got := o.waiting("A"); got != 8 {
		t.Fatalf("%d waiting, want 8", got)
	}
	// Another client is not affected by A's queue.
	bres := make(chan error, 1)
	o.start(&wg, WithClient(t.Context(), "B"), "b1", bres)
	waitFor(t, "b1 entered", func() bool { return o.calls.Load() >= 3 })
	o.let("b1")
	if err := <-bres; err != nil {
		t.Fatalf("client B: %v", err)
	}
	for _, n := range names {
		o.let(n)
	}
	wg.Wait()
	close(res)
	for err := range res {
		if err != nil {
			t.Fatalf("a queued operation failed: %v", err)
		}
	}
	if n := o.trackedClients(); n != 0 {
		t.Fatalf("%d client keys tracked", n)
	}
}

// A waiter whose context ends leaves the queue with the context's error and its place
// can be taken by the next caller.
func TestGate_AWaiterWhoseContextEndsFreesItsPlace(t *testing.T) {
	t.Parallel()
	o := newOrderedGate(16, 30*time.Second, WithPerClient(1))
	ctx := WithClient(t.Context(), "A")
	res := make(chan error, 16)
	var wg sync.WaitGroup
	o.start(&wg, ctx, "run", res)
	<-o.entered
	var cancels []context.CancelFunc
	for i := range 8 {
		c, cancel := context.WithCancel(ctx)
		cancels = append(cancels, cancel)
		o.start(&wg, c, "w"+string(rune('0'+i)), res)
		waitFor(t, "waiter queued", func() bool { return o.waiting("A") == i+1 })
	}
	if err := o.Verify(ctx, "full", "s"); !errors.Is(err, ErrBusy) {
		t.Fatalf("a full queue = %v, want ErrBusy", err)
	}
	cancels[3]()
	if err := <-res; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled waiter = %v, want context.Canceled", err)
	}
	waitFor(t, "a place freed", func() bool { return o.waiting("A") == 7 })
	late := make(chan error, 1)
	o.start(&wg, ctx, "late", late)
	waitFor(t, "the freed place taken", func() bool { return o.waiting("A") == 8 })
	for _, c := range cancels {
		c()
	}
	o.let("run")
	o.let("late")
	wg.Wait()
	if n := o.trackedClients(); n != 0 {
		t.Fatalf("%d client keys tracked after everyone left", n)
	}
}

// A waiter that cannot start within the queue timeout is refused with ErrBusy and does
// NO password work; the operations that were running are unaffected.
func TestGate_AWaiterThatTimesOutIsRefusedWithoutWork(t *testing.T) {
	t.Parallel()
	o := newOrderedGate(8, 30*time.Millisecond, WithPerClient(2))
	ctx := WithClient(t.Context(), "A")
	res := make(chan error, 4)
	var wg sync.WaitGroup
	o.start(&wg, ctx, "p1", res)
	o.start(&wg, ctx, "p2", res)
	<-o.entered
	<-o.entered
	if err := o.Verify(ctx, "p3", "s"); !errors.Is(err, ErrBusy) {
		t.Fatalf("a waiter past the queue timeout = %v, want ErrBusy", err)
	}
	if got := o.calls.Load(); got != 2 {
		t.Fatalf("%d operations did work, want only the two running ones", got)
	}
	if got := o.waiting("A"); got != 0 {
		t.Fatalf("%d still waiting after the timeout", got)
	}
	o.let("p1")
	o.let("p2")
	wg.Wait()
	if n := o.trackedClients(); n != 0 {
		t.Fatalf("%d client keys tracked", n)
	}
}
