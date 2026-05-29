//go:build integration

// outbox_subscriber_test.go — subscriber-based outbox test infrastructure
// for the crm/adapters package. Strict TDL canon per ADR 0062 Amendment 1.
// See identity/adapters/outbox_subscriber_test.go for the full rationale.

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
	"github.com/leadkart/leadkart-go/internal/crm/adapters"
)

const crmOutboxTopic = "crm.events"

var crmForwarderFixedNow = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

type crmDrainSubscriber struct {
	mu       sync.Mutex
	received []*message.Message
}

func (d *crmDrainSubscriber) record(msgs <-chan *message.Message) {
	for msg := range msgs {
		d.mu.Lock()
		d.received = append(d.received, msg)
		d.mu.Unlock()
		msg.Ack()
	}
}

func (d *crmDrainSubscriber) snapshot() []*message.Message {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*message.Message, len(d.received))
	copy(out, d.received)
	return out
}

// crmWaitForCount polls the drain until len ≥ want or timeout.
// arch-test:wait-justified — async COMMIT-to-subscriber boundary per
// the identity package shape; canonical Watermill test pattern.
func crmWaitForCount(t *testing.T, drain *crmDrainSubscriber, want int, timeout time.Duration) []*message.Message {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := drain.snapshot(); len(got) >= want {
			return got
		}
		time.Sleep(20 * time.Millisecond) // arch-test:wait-justified - async event-driven test wait
	}
	t.Fatalf("crm subscriber: timed out waiting for %d messages, got %d", want, len(drain.snapshot()))
	return nil
}

func crmSilentSlog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// crmOutboxFixture wires per-test pubsub + drain + forwarder against
// the shared package pool. Mirror of identity's outboxFixture.
type crmOutboxFixture struct {
	pool      *pgxpool.Pool
	pubsub    *gochannel.GoChannel
	drain     *crmDrainSubscriber
	forwarder *adapters.OutboxForwarder
}

func newCrmOutboxFixture(t *testing.T) *crmOutboxFixture {
	t.Helper()
	pool := crmRepoFixture(t)
	tx := pg.NewTransactor(pool)

	pubsub := gochannel.NewGoChannel(gochannel.Config{}, watermill.NewSlogLogger(crmSilentSlog()))
	t.Cleanup(func() { _ = pubsub.Close() })

	msgs, err := pubsub.Subscribe(t.Context(), crmOutboxTopic)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	drain := &crmDrainSubscriber{}
	go drain.record(msgs)

	forwarder := adapters.NewOutboxForwarder(
		pool, tx, pubsub, crmOutboxTopic, 0,
		func() time.Time { return crmForwarderFixedNow },
	)
	return &crmOutboxFixture{pool: pool, pubsub: pubsub, drain: drain, forwarder: forwarder}
}

func (f *crmOutboxFixture) forwardAndWait(t *testing.T, want int) []*message.Message {
	t.Helper()
	if _, err := f.forwarder.ForwardOnce(t.Context()); err != nil {
		t.Fatalf("ForwardOnce: %v", err)
	}
	return crmWaitForCount(t, f.drain, want, 2*time.Second)
}

func crmEventTypes(msgs []*message.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Metadata.Get(messaging.HeaderEventType)
	}
	return out
}
