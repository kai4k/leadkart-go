//go:build integration

// arch-test:no-timeout-needed — every wait below is bounded by its own deadline.
//
// arch-test:no-synctest — the outbox row is read across the pgx wire protocol,
// which synctest cannot model.

package messaging_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/google/uuid"

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
	t.Cleanup(func() { cancel(); <-done })

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
