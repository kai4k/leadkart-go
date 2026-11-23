//go:build integration

// arch-test:no-timeout-needed — every wait below is bounded by its own deadline.
//
// arch-test:no-synctest — the outbox row is read across the pgx wire protocol,
// which synctest cannot model.

package messaging_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/components/cqrs"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/audit"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/messaging/messagingtest"
)

type pubsubTestEvent struct {
	N int `json:"n"`
}

func (pubsubTestEvent) Topic() string         { return "test.pubsub_event.v1" }
func (pubsubTestEvent) OccurredAt() time.Time { return time.Now().UTC() }

// An event must not leave the outbox until a subscriber has acknowledged it. With a
// non-blocking in-process bus the forwarder deletes the row the moment Publish
// returns: an event published while no subscriber is attached (the forwarder
// starts before the router) or while a handler is mid-flight at shutdown exists
// nowhere afterwards, with no dead letter either (review H2).
func TestPubSub_AnEventStaysInTheOutboxUntilASubscriberAcknowledgesIt(t *testing.T) {
	pool := inboxFixture(t)
	ctx := t.Context()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := messaging.PublishOutbox(ctx, tx, "test.pubsub.topic", uuid.New(), []pubsubTestEvent{{N: 1}}); err != nil {
		t.Fatalf("PublishOutbox: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	bus := messaging.NewPubSub(watermill.NopLogger{})
	t.Cleanup(func() { _ = bus.Close() })
	msgs, err := bus.Subscribe(ctx, "test.pubsub.topic")
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	fwd, err := messaging.NewOutboxForwarder(pool, bus, watermill.NopLogger{})
	if err != nil {
		t.Fatalf("NewOutboxForwarder: %v", err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := fwd.Run(runCtx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("forwarder: %v", err)
		}
	}()
	t.Cleanup(func() { cancel(); <-done; _ = fwd.Close() }) // Close: a forwarder left subscribed would claim a later test's outbox row

	// The subscriber receives the event but has not finished (a handler is
	// mid-flight, as at shutdown): the row must still be in the outbox.
	var held *messageHolder
	select {
	case m := <-msgs:
		held = &messageHolder{m: m}
	case <-time.After(10 * time.Second):
		t.Fatal("the subscriber never received the event")
	}
	time.Sleep(2 * time.Second) // arch-test:wait-justified — absence of an effect has no event to wait for
	if rows := messagingtest.RowsForTopic(messagingtest.DrainOutbox(ctx, t, pool), "test.pubsub.topic"); len(rows) != 1 {
		t.Fatalf("outbox rows while the handler is unfinished = %d, want 1: the event would be lost if the process stopped now", len(rows))
	}

	// Once the handler acknowledges, the row goes.
	held.m.Ack()
	deadline := time.Now().Add(10 * time.Second)
	// arch-test:wait-justified — bounded poll for the forwarder to delete the acknowledged row
	for time.Now().Before(deadline) {
		if len(messagingtest.RowsForTopic(messagingtest.DrainOutbox(ctx, t, pool), "test.pubsub.topic")) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond) // arch-test:wait-justified — poll interval inside a deadline-bounded loop
	}
	t.Fatal("the acknowledged event stayed in the outbox")
}

type messageHolder struct{ m *message.Message }

// Review B4: one stuck handler must not stop delivery for every company. The bus
// delivers one event at a time and the forwarder waits for each, so a handler stuck on
// a row lock held the whole lane. Each ATTEMPT now has a timeout (Retry runs outside it),
// so the stuck event burns its small retry budget, is dead-lettered, and the next
// event (another company's) is delivered, with no redelivery of the first.
func TestRouter_AStuckHandlerDoesNotStopDeliveryForOtherCompanies(t *testing.T) {
	pool := inboxFixture(t)
	ctx := t.Context()
	log := silentLog()
	bus := messaging.NewPubSub(watermill.NopLogger{})
	t.Cleanup(func() { _ = bus.Close() })

	const attemptTimeout = 300 * time.Millisecond
	retry := messaging.RetryConfig{MaxRetries: 2, InitialInterval: 10 * time.Millisecond, MaxInterval: 20 * time.Millisecond, Multiplier: 2}
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
	var stuckAttempts atomic.Int32
	delivered := make(chan int, 4)
	h := cqrs.NewEventHandler("test.stuck.Handler", func(hctx context.Context, e *pubsubTestEvent) error {
		if e.N == 1 { // company A's event: waits on a lock that is never released
			stuckAttempts.Add(1)
			<-hctx.Done()
			return hctx.Err()
		}
		delivered <- e.N
		return nil
	})
	if err := router.AddCqrsHandler(ep, h); err != nil {
		t.Fatal(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = router.Run(runCtx) // arch-test:ignore-err — stops with the test
	}()
	t.Cleanup(func() { cancel(); <-done })
	<-router.Running()

	for _, n := range []int{1, 2} {
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
	t.Cleanup(func() { fcancel(); <-fdone; _ = fwd.Close() }) // Close: a forwarder left subscribed would claim a later test's outbox row

	budget := messaging.RetryBudget(retry, attemptTimeout)
	select {
	case n := <-delivered:
		if n != 2 {
			t.Fatalf("delivered %d", n)
		}
	case <-time.After(budget + 5*time.Second):
		t.Fatalf("not delivered within the stuck event budget (%s), stuck attempts=%d", budget, stuckAttempts.Load())
	}
	// A timeout is retried at most once (review G2b H1): two attempts, each cut at the timeout.
	if got := stuckAttempts.Load(); got != 2 {
		t.Fatalf("the stuck event ran %d attempts, want 2 (each cut at the timeout)", got)
	}
	deadline := time.Now().Add(5 * time.Second)
	// arch-test:wait-justified — bounded poll for the async dead-letter write
	for messagingtest.DeadLetterCount(ctx, t, pool, "test.stuck.Handler") == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the stuck event was never dead-lettered")
		}
		time.Sleep(20 * time.Millisecond) // arch-test:wait-justified — poll interval inside a deadline-bounded loop
	}
	if budget >= 30*time.Second {
		t.Fatalf("the retry budget %s must stay below the forwarder ack deadline floor", budget)
	}
	// No redelivery of the stuck event: it ran exactly its attempts, no more.
	time.Sleep(500 * time.Millisecond) // arch-test:wait-justified — absence of a redelivery has no event to wait for
	if got := stuckAttempts.Load(); got != 2 {
		t.Fatalf("the stuck event was redelivered: %d attempts", got)
	}
}
