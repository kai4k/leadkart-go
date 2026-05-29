//go:build integration

// outbox_subscriber_test.go — subscriber-based outbox test infrastructure
// for the identity/adapters package.
//
// Strict TDL canon (ADR 0062 Amendment 1): integration tests that need to
// observe outbox emissions do it the way production code does — subscribe
// to the Watermill topic the forwarder publishes to. NEVER read the outbox
// table directly. State-based reads (via messagingtest helpers) were
// retired because:
//
//   - They duplicate what the forwarder + subscriber already verify.
//   - They couple tests to a specific persistence detail (row in
//     identity.outbox at a point in time) instead of the observable
//     contract (event lands on the bus).
//   - They cannot enforce ORDER deterministically without an `id`
//     tiebreaker, which is itself an SQL-level concern callers should
//     not need to know about.
//
// Production parity:
//
//   Handler  →  domain emits Event  →  repository writes outbox row
//          →  OutboxForwarder.ForwardOnce reads + publishes
//          →  Watermill subscriber receives + acks
//
// Tests collapse the chain into one fixture: the test calls the
// production code, then ForwardOnce, then asserts on the messages the
// subscriber received. Same code path as production, deterministic.
//
// Concurrency caveat (TruncateAll + t.Parallel exclusion — canon):
//   Outbox-observing tests share the per-package Postgres container.
//   With multiple parallel fixtures each polling the SAME outbox table,
//   FOR UPDATE SKIP LOCKED partitions rows across forwarders — one
//   test's events can be drained by another's forwarder + published to
//   the OTHER test's pubsub. Result: a parallel test misses its own
//   events.
//
//   Mitigation: outbox-observing tests MUST be SERIAL (no t.Parallel)
//   + call sharedPG.TruncateAll(t). The arch test
//   TestArch_TruncateAllImpliesSerial enforces the negative. Per the
//   user-canon memory rule: "shared-container tests must split into
//   RLS-parallel vs TruncateAll-serial buckets".

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
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
)

// outboxTopic is the Watermill destination the identity forwarder
// publishes to. Per-event routing happens via the `event_type` header
// (HeaderEventType) inside the message metadata.
const outboxTopic = "identity.events"

// forwarderFixedNow is the deterministic instant identity outbox-forwarder
// integration tests pass into NewOutboxForwarder per the clock-injection
// refactor — replaces the prior implicit clock.Now() reliance.
var forwarderFixedNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

// drainSubscriber records every received message. In-memory +
// assertion-friendly; unlike the production audit-log subscriber it
// holds no persistent state.
type drainSubscriber struct {
	mu       sync.Mutex
	received []*message.Message
}

func (d *drainSubscriber) record(msgs <-chan *message.Message) {
	for msg := range msgs {
		d.mu.Lock()
		d.received = append(d.received, msg)
		d.mu.Unlock()
		msg.Ack()
	}
}

func (d *drainSubscriber) snapshot() []*message.Message {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]*message.Message, len(d.received))
	copy(out, d.received)
	return out
}

// waitForCount polls the subscriber's slice until len ≥ want or
// timeout. Returns the recorded slice on success; t.Fatal on timeout.
// arch-test:wait-justified — async event-driven test wait between the
// pgx COMMIT of the forwarder's MarkOutboxEventForwarded and the
// in-process Watermill subscriber goroutine receiving the message;
// neither side exposes a synchronous "ready" signal so polling is the
// canonical Watermill test shape (mirrors the TDL Wild Workouts
// outbox tests + Watermill's own pubsub test suite).
func waitForCount(t *testing.T, drain *drainSubscriber, want int, timeout time.Duration) []*message.Message {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := drain.snapshot(); len(got) >= want {
			return got
		}
		time.Sleep(20 * time.Millisecond) // arch-test:wait-justified - async event-driven test wait
	}
	t.Fatalf("subscriber: timed out waiting for %d messages, got %d", want, len(drain.snapshot()))
	return nil
}

// silentSlog returns a slog.Logger that writes nothing — keeps test
// runs quiet. Watermill's NewSlogLogger wraps it.
func silentSlog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// outboxFixture wires a per-test pubsub + drain + forwarder against
// the shared package pool. Caller pattern:
//
//	sharedPG.TruncateAll(t)  // serial + clean slate
//	fix := newOutboxFixture(t)
//	... drive production code ...
//	msgs := fix.forwardAndWait(t, wantCount)
//	... assert msgs[i].Metadata.Get(messaging.HeaderEventType) etc ...
//
// Holds no exported state — fields are internal to the package.
type outboxFixture struct {
	pool      *pgxpool.Pool
	pubsub    *gochannel.GoChannel
	drain     *drainSubscriber
	forwarder *adapters.OutboxForwarder
}

// newOutboxFixture spins a fresh in-process pubsub + drain goroutine
// + identity outbox forwarder. Auto-closes the pubsub via t.Cleanup
// so the drain goroutine exits cleanly + goleak stays green.
func newOutboxFixture(t *testing.T) *outboxFixture {
	t.Helper()
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)

	pubsub := gochannel.NewGoChannel(gochannel.Config{}, watermill.NewSlogLogger(silentSlog()))
	t.Cleanup(func() { _ = pubsub.Close() })

	msgs, err := pubsub.Subscribe(t.Context(), outboxTopic)
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	drain := &drainSubscriber{}
	go drain.record(msgs)

	forwarder := adapters.NewOutboxForwarder(
		pool, tx, pubsub, outboxTopic, 0,
		func() time.Time { return forwarderFixedNow },
	)
	return &outboxFixture{pool: pool, pubsub: pubsub, drain: drain, forwarder: forwarder}
}

// forwardAndWait drains the outbox once + waits for the subscriber
// goroutine to receive `want` messages. Returns the received slice.
// Asserts on count via t.Fatal — caller must already know how many
// events the production code under test emits.
func (f *outboxFixture) forwardAndWait(t *testing.T, want int) []*message.Message {
	t.Helper()
	if _, err := f.forwarder.ForwardOnce(t.Context()); err != nil {
		t.Fatalf("ForwardOnce: %v", err)
	}
	return waitForCount(t, f.drain, want, 2*time.Second)
}

// eventTypes extracts the canonical event_type header from each
// received message in receive order. Receive order matches
// publish order in GoChannel + matches outbox table order
// (created_at, id) per the forwarder's ListUnforwardedOutboxEvents
// query — which is the canon ordering per the
// TestArch_OutboxSelectsOrderByMonotonicTiebreaker gate.
func eventTypes(msgs []*message.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Metadata.Get(messaging.HeaderEventType)
	}
	return out
}
