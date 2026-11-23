//go:build integration

// arch-test:no-timeout-needed — every wait below is bounded by its own deadline.
//
// arch-test:no-synctest — the outbox, the dead-letter table and the inbox are read
// across the pgx wire protocol, which synctest cannot model.

// Review G2b H1: a timed-out event must not be lost, and stuck events must not add up
// on the delivery lane. A handler attempt that times out gets at most one retry, is
// dead-lettered, and the replay job delivers it again once the block clears (bounded,
// with backoff, the dead-letter row kept), exactly once through the inbox.

package messaging_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/components/cqrs"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/audit"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/messaging/messagingtest"
)

// replayRig is a router with the given handlers and the outbox forwarder feeding it.
type replayRig struct {
	pool     *pgxpool.Pool
	replayer *messaging.DeadLetterReplayer
}

// startReplayRig runs the handlers on a router with a forwarder. The replayer it returns
// replays only the handlers the test marked with [messaging.ReplaySafe].
func startReplayRig(t *testing.T, pool *pgxpool.Pool, retry messaging.RetryConfig, attemptTimeout time.Duration, handlers ...cqrs.EventHandler) replayRig {
	t.Helper()
	// t.Context() is cancelled just before the cleanups run; the router and the forwarder must
	// outlive it so the drain wait below runs while they still acknowledge.
	ctx := context.WithoutCancel(t.Context())
	log := silentLog()
	bus := messaging.NewPubSub(watermill.NopLogger{})
	t.Cleanup(func() { _ = bus.Close() })
	router, err := messaging.NewRouter(messaging.Deps{
		Subscriber: bus, Publisher: bus, Logger: log,
		IdempotencyInbox: messaging.NewIdempotentReceiver(pool),
		AuditWriter:      audit.NewWriter(pool, log, time.Now),
		DeadLetters:      messaging.NewDeadLetterWriter(pool, log, time.Now),
		CloseTimeout:     2 * time.Second,
		Retry:            retry,
		HandlerTimeout:   attemptTimeout,
	})
	if err != nil {
		t.Fatal(err)
	}
	ep, err := messaging.NewEventProcessor(router.RawRouter(), bus, watermill.NewSlogLogger(log))
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range handlers {
		if err := router.AddCqrsHandler(ep, h); err != nil {
			t.Fatal(err)
		}
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = router.Run(runCtx) // arch-test:ignore-err — stops with the test
	}()
	t.Cleanup(func() { cancel(); <-done })
	<-router.Running()

	fwd, err := messaging.NewOutboxForwarder(pool, bus, watermill.NopLogger{})
	if err != nil {
		t.Fatal(err)
	}
	fctx, fcancel := context.WithCancel(ctx)
	fdone := make(chan struct{})
	go func() {
		defer close(fdone)
		_ = fwd.Run(fctx) // arch-test:ignore-err — stops with the test
	}()
	// Close, not just cancel: a forwarder left subscribed would claim a later test's outbox row.
	t.Cleanup(func() { fcancel(); <-fdone; _ = fwd.Close() })
	// Runs first on cleanup: let the forwarder finish acknowledging what it delivered. A row
	// left unacknowledged holds every later row of the shared outbox behind its ack deadline.
	t.Cleanup(func() {
		deadline := time.Now().Add(10 * time.Second)
		// arch-test:wait-justified — bounded poll for the outbox to drain
		for len(messagingtest.DrainOutbox(ctx, t, pool)) > 0 && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond) // arch-test:wait-justified — poll interval inside a deadline-bounded loop
		}
	})
	return replayRig{pool: pool, replayer: messaging.NewDeadLetterReplayer(pool, log, messaging.ReplaySafeHandlerNames(handlers))}
}

func publishTestEvents(t *testing.T, pool *pgxpool.Pool, ns ...int) {
	t.Helper()
	ctx := t.Context()
	for _, n := range ns {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := messaging.PublishOutbox(ctx, tx, "test.events", uuid.New(), []pubsubTestEvent{{N: n}}); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

// waitUntil polls cond until it holds, failing after d with what.
func waitUntil(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	// arch-test:wait-justified — bounded poll for an async delivery
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not within %s: %s", d, what)
		}
		time.Sleep(20 * time.Millisecond) // arch-test:wait-justified — poll interval inside a deadline-bounded loop
	}
}

var fastRetry = messaging.RetryConfig{MaxRetries: 5, InitialInterval: 10 * time.Millisecond, MaxInterval: 20 * time.Millisecond, Multiplier: 2}

// Three stuck events of one company used to hold the lane for six attempts each, so
// another company's event waited minutes in production. A timed-out event now gets one
// more attempt and is dead-lettered: the lane is held for two attempts per stuck event.
func TestRouter_ThreeStuckEventsDoNotHoldTheLaneForTheirWholeRetryBudget(t *testing.T) {
	pool := inboxFixture(t)
	const timeout = 300 * time.Millisecond
	var stuckAttempts atomic.Int32
	delivered := make(chan int, 4)
	h := cqrs.NewEventHandler("test.lane3.Handler", func(hctx context.Context, e *pubsubTestEvent) error {
		if e.N <= 3 {
			stuckAttempts.Add(1)
			<-hctx.Done()
			return hctx.Err()
		}
		delivered <- e.N
		return nil
	})
	startReplayRig(t, pool, fastRetry, timeout, h)

	start := time.Now()
	publishTestEvents(t, pool, 1, 2, 3, 4)
	select {
	case n := <-delivered:
		if n != 4 {
			t.Fatalf("delivered %d", n)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the other company's event was not delivered; stuck attempts %d", stuckAttempts.Load())
	}
	elapsed := time.Since(start)
	if got := stuckAttempts.Load(); got != 6 {
		t.Fatalf("three stuck events ran %d attempts, want 6 (the timeout and one retry each)", got)
	}
	// Two attempts of 300 ms and a short backoff per stuck event, three events: about 1.9 s.
	// Six attempts each would be about 5.7 s.
	if limit := 3500 * time.Millisecond; elapsed > limit {
		t.Fatalf("the lane was held %s by three stuck events, want under %s", elapsed, limit)
	}
}

// A reserve or consume event that timed out is not lost: once the block clears the
// replay delivers it, the handler succeeds once, the inbox holds one row, the dead letter
// is kept, and nothing is replayed twice.
func TestDeadLetterReplay_ATimedOutEventIsDeliveredOnceTheBlockClears(t *testing.T) {
	pool := inboxFixture(t)
	const handler = "test.replay.once.Handler"
	var blocked atomic.Bool
	blocked.Store(true)
	var successes atomic.Int32
	h := messaging.ReplaySafe(cqrs.NewEventHandler(handler, func(hctx context.Context, e *pubsubTestEvent) error {
		if blocked.Load() {
			<-hctx.Done()
			return hctx.Err()
		}
		successes.Add(1)
		return nil
	}))
	rig := startReplayRig(t, pool, fastRetry, 150*time.Millisecond, h)
	publishTestEvents(t, pool, 1)
	waitUntil(t, 10*time.Second, "the stuck event is dead-lettered", func() bool {
		return messagingtest.DeadLetterCount(t.Context(), t, pool, handler) == 1
	})
	msgID := messagingtest.DeadLetterMessageIDs(t.Context(), t, pool, handler)[0]

	// Before its backoff nothing is replayed.
	n, err := rig.replayer.ReplayDueFor(t.Context(), time.Now().Add(30*time.Second), 10, handler)
	if err != nil || n != 0 {
		t.Fatalf("a replay before the backoff: %d, %v", n, err)
	}

	blocked.Store(false) // the block clears
	n, err = rig.replayer.ReplayDueFor(t.Context(), time.Now().Add(2*time.Minute), 10, handler)
	if err != nil || n != 1 {
		t.Fatalf("replay after the backoff: %d, %v; want 1", n, err)
	}
	waitUntil(t, 10*time.Second, "the replayed event reaches the handler", func() bool { return successes.Load() == 1 })

	if got := messagingtest.InboxCountForMessage(t, pool, msgID, handler); got != 1 {
		t.Fatalf("inbox rows for the replayed event: %d, want 1", got)
	}
	if got := messagingtest.DeadLetterCount(t.Context(), t, pool, handler); got != 1 {
		t.Fatalf("the dead letter must be kept: %d rows", got)
	}
	if got := messagingtest.ReplayCount(t.Context(), t, pool, handler); got != 1 {
		t.Fatalf("replays recorded: %d, want 1", got)
	}
	// The same dead letter is never replayed twice, and the handler never runs again.
	n, err = rig.replayer.ReplayDueFor(t.Context(), time.Now().Add(24*time.Hour), 10, handler)
	if err != nil || n != 0 {
		t.Fatalf("a second replay of the same dead letter: %d, %v; want none", n, err)
	}
	time.Sleep(300 * time.Millisecond) // arch-test:wait-justified — absence of a second delivery has no event to wait for
	if got := successes.Load(); got != 1 {
		t.Fatalf("the handler succeeded %d times, want exactly once", got)
	}
}

// A message that times out every time is replayed a bounded number of times, then left
// in common.dead_letter for a person; every dead letter and every replay stays.
func TestDeadLetterReplay_IsBounded(t *testing.T) {
	pool := inboxFixture(t)
	const handler = "test.replay.bounded.Handler"
	var attempts atomic.Int32
	h := messaging.ReplaySafe(cqrs.NewEventHandler(handler, func(hctx context.Context, e *pubsubTestEvent) error {
		attempts.Add(1)
		<-hctx.Done()
		return hctx.Err()
	}))
	rig := startReplayRig(t, pool, fastRetry, 100*time.Millisecond, h)
	publishTestEvents(t, pool, 1)

	later := time.Now().Add(24 * time.Hour)
	for replay := 1; replay <= messaging.MaxDeadLetterReplays; replay++ {
		waitUntil(t, 10*time.Second, "the next dead letter", func() bool {
			return messagingtest.DeadLetterCount(t.Context(), t, pool, handler) == replay
		})
		n, err := rig.replayer.ReplayDueFor(t.Context(), later, 10, handler)
		if err != nil || n != 1 {
			t.Fatalf("replay %d: %d, %v; want 1", replay, n, err)
		}
	}
	waitUntil(t, 10*time.Second, "the last dead letter", func() bool {
		return messagingtest.DeadLetterCount(t.Context(), t, pool, handler) == messaging.MaxDeadLetterReplays+1
	})
	n, err := rig.replayer.ReplayDueFor(t.Context(), later.Add(24*time.Hour), 10, handler)
	if err != nil || n != 0 {
		t.Fatalf("a replay past the bound: %d, %v; want none", n, err)
	}
	if got := messagingtest.ReplayCount(t.Context(), t, pool, handler); got != messaging.MaxDeadLetterReplays {
		t.Fatalf("replays recorded: %d, want %d", got, messaging.MaxDeadLetterReplays)
	}
}

// Only a timeout is replayed. A poisoned message stays a dead letter, and a replay is for
// the handler that timed out: another handler on the same topic that dead-lettered for its
// own reason is not run again.
func TestDeadLetterReplay_OnlyATimeoutIsReplayedAndOnlyForItsHandler(t *testing.T) {
	pool := inboxFixture(t)
	const stuck, poisoned = "test.replay.target.Stuck", "test.replay.target.Poisoned"
	var mu sync.Mutex
	poisonedRuns := 0
	var blocked atomic.Bool
	blocked.Store(true)
	var successes atomic.Int32
	hStuck := messaging.ReplaySafe(cqrs.NewEventHandler(stuck, func(hctx context.Context, e *pubsubTestEvent) error {
		if blocked.Load() {
			<-hctx.Done()
			return hctx.Err()
		}
		successes.Add(1)
		return nil
	}))
	// The poisoned handler is replay-safe too: only the timeout filter keeps it from running again.
	hPoisoned := messaging.ReplaySafe(cqrs.NewEventHandler(poisoned, func(context.Context, *pubsubTestEvent) error {
		mu.Lock()
		poisonedRuns++
		mu.Unlock()
		return messaging.NonRetryable(errors.New("malformed"))
	}))
	rig := startReplayRig(t, pool, fastRetry, 150*time.Millisecond, hStuck, hPoisoned)
	publishTestEvents(t, pool, 1)
	waitUntil(t, 10*time.Second, "both are dead-lettered", func() bool {
		return messagingtest.DeadLetterCount(t.Context(), t, pool, stuck) == 1 &&
			messagingtest.DeadLetterCount(t.Context(), t, pool, poisoned) == 1
	})

	blocked.Store(false)
	// Replay for EVERY handler: the poisoned one is replay-safe, so only the "a timeout and
	// nothing else" rule keeps its dead letter from being delivered again.
	n, err := rig.replayer.ReplayDue(t.Context(), time.Now().Add(time.Hour), 10)
	if err != nil || n != 1 {
		t.Fatalf("replayed %d, %v; want only the timeout", n, err)
	}
	waitUntil(t, 10*time.Second, "the stuck handler succeeds", func() bool { return successes.Load() == 1 })
	time.Sleep(300 * time.Millisecond) // arch-test:wait-justified — absence of a second run has no event to wait for
	mu.Lock()
	defer mu.Unlock()
	if poisonedRuns != 1 {
		t.Fatalf("the poisoned handler ran %d times, want 1 (a replay is for the handler that timed out)", poisonedRuns)
	}
	if got := messagingtest.DeadLetterCount(t.Context(), t, pool, poisoned); got != 1 {
		t.Fatalf("poisoned dead letters: %d, want 1", got)
	}
}

// Review G2c B2: replay is opt-in. A handler that is not marked replay-safe is never
// delivered again automatically, however it dead-lettered: applying a stale event to an
// order that has moved on corrupts stock. Its dead letter stays for a person.
func TestDeadLetterReplay_AHandlerThatIsNotMarkedReplaySafeIsNeverReplayed(t *testing.T) {
	pool := inboxFixture(t)
	const handler = "test.replay.optin.Handler"
	var runs atomic.Int32
	h := cqrs.NewEventHandler(handler, func(hctx context.Context, e *pubsubTestEvent) error { // not ReplaySafe
		runs.Add(1)
		<-hctx.Done()
		return hctx.Err()
	})
	rig := startReplayRig(t, pool, fastRetry, 100*time.Millisecond, h)
	publishTestEvents(t, pool, 1)
	waitUntil(t, 10*time.Second, "the stuck event is dead-lettered", func() bool {
		return messagingtest.DeadLetterCount(t.Context(), t, pool, handler) == 1
	})
	before := runs.Load()

	n, err := rig.replayer.ReplayDue(t.Context(), time.Now().Add(24*time.Hour), 10)
	if err != nil || n != 0 {
		t.Fatalf("replayed %d, %v; want none for a handler that did not opt in", n, err)
	}
	n, err = rig.replayer.ReplayDueFor(t.Context(), time.Now().Add(24*time.Hour), 10, handler)
	if err != nil || n != 0 {
		t.Fatalf("replayed %d, %v by name; want none", n, err)
	}
	time.Sleep(300 * time.Millisecond) // arch-test:wait-justified — absence of a delivery has no event to wait for
	if got := runs.Load(); got != before {
		t.Fatalf("the handler ran %d more times after the replay job", got-before)
	}
	if got := messagingtest.DeadLetterCount(t.Context(), t, pool, handler); got != 1 {
		t.Fatalf("the dead letter must stay: %d rows", got)
	}
	if got := messagingtest.ReplayCount(t.Context(), t, pool, handler); got != 0 {
		t.Fatalf("replays recorded: %d, want 0", got)
	}
}
