//go:build integration

// outbox_subscriber_test.go — subscriber-based outbox test infrastructure
// for the platform/adapters package. Strict TDL canon per ADR 0062
// Amendment 1. See identity/adapters/outbox_subscriber_test.go for the
// full rationale.
//
// Platform note: platform-scoped events persist with tenant_id = NULL
// (migration 20260601000002). The forwarder omits the tenant_id
// metadata header for those rows, so subscriber-side tests assert the
// header is EMPTY for platform-scoped events + non-empty for
// tenant-scoped events.

package adapters_test

import (
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/ThreeDotsLabs/watermill/pubsub/gochannel"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/platform/adapters"
)

const platformOutboxTopic = "platform.events"

type platformDrainSubscriber struct {
	mu       sync.Mutex
	received []*message.Message
}

func (d *platformDrainSubscriber) record(msgs <-chan *message.Message) {
	for msg := range msgs {
		d.mu.Lock()
		d.received = append(d.received, msg)
		d.mu.Unlock()
		msg.Ack()
	}
}

func (d *platformDrainSubscriber) snapshot() []*message.Message {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*message.Message, len(d.received))
	copy(out, d.received)
	return out
}

// platformWaitForCount polls the drain until len ≥ want or timeout.
// arch-test:wait-justified — async COMMIT-to-subscriber boundary;
// canonical Watermill test pattern.
func platformWaitForCount(t *testing.T, drain *platformDrainSubscriber, want int, timeout time.Duration) []*message.Message {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := drain.snapshot(); len(got) >= want {
			return got
		}
		time.Sleep(20 * time.Millisecond) // arch-test:wait-justified - async event-driven test wait
	}
	t.Fatalf("platform subscriber: timed out waiting for %d messages, got %d", want, len(drain.snapshot()))
	return nil
}

func platformSilentSlog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// platformOutboxFixture wires per-test pubsub + drain + forwarder
// against the shared package pool. Mirror of identity's outboxFixture.
type platformOutboxFixture struct {
	pool      *pgxpool.Pool
	pubsub    *gochannel.GoChannel
	drain     *platformDrainSubscriber
	forwarder *adapters.OutboxForwarder
}

func newPlatformOutboxFixture(t *testing.T) *platformOutboxFixture {
	t.Helper()
	pool := platformPool(t)
	tx := pg.NewTransactor(pool)

	pubsub := gochannel.NewGoChannel(gochannel.Config{}, watermill.NewSlogLogger(platformSilentSlog()))
	t.Cleanup(func() { _ = pubsub.Close() })

	msgs, err := pubsub.Subscribe(t.Context(), platformOutboxTopic)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	drain := &platformDrainSubscriber{}
	go drain.record(msgs)

	forwarder := adapters.NewOutboxForwarder(
		pool, tx, pubsub, platformOutboxTopic, 0,
		func() time.Time { return nowUTC() },
	)
	return &platformOutboxFixture{pool: pool, pubsub: pubsub, drain: drain, forwarder: forwarder}
}

func (f *platformOutboxFixture) forwardAndWait(t *testing.T, want int) []*message.Message {
	t.Helper()
	if _, err := f.forwarder.ForwardOnce(t.Context()); err != nil {
		t.Fatalf("ForwardOnce: %v", err)
	}
	return platformWaitForCount(t, f.drain, want, 2*time.Second)
}

func platformEventTypes(msgs []*message.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Metadata.Get(messaging.HeaderEventType)
	}
	return out
}
