//go:build integration

// outbox_subscriber_test.go — outbox assertion fixture for the identity
// adapters package (post-ADR-0064).
//
// Producers write an enveloped row to common.outbox in the same tx; the
// library Watermill Forwarder drains it. Tests assert the PRODUCER contract
// via messagingtest.DrainOutbox. The forwarder hop is library code.

package adapters_test

import (
	"testing"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/messaging/messagingtest"
)

// drainOutboxRows returns the raw relay rows for tests that need to
// assert the forwarder routing target as well as the inner message.
func drainOutboxRows(t *testing.T, f *outboxFixture) []messagingtest.OutboxRow {
	t.Helper()
	return messagingtest.DrainOutbox(t.Context(), t, f.pool)
}

// outboxFixture holds the shared package pool.
// Usage: TruncateAll(t) → newOutboxFixture(t) → drive repo → forwardAndWait.
type outboxFixture struct {
	pool *pgxpool.Pool
}

// newOutboxFixture wraps the shared package-scoped pool.
func newOutboxFixture(t *testing.T) *outboxFixture {
	t.Helper()
	return &outboxFixture{pool: repoFixture(t)}
}

// forwardAndWait reads and unwraps rows from common.outbox, asserting
// len == want. Returns messages in drain order. Name kept for caller
// compatibility; post-ADR-0064 the relay is read directly (no forwarder hop).
func (f *outboxFixture) forwardAndWait(t *testing.T, want int) []*message.Message {
	t.Helper()
	rows := messagingtest.DrainOutbox(t.Context(), t, f.pool)
	if len(rows) != want {
		t.Fatalf("outbox rows: got %d want %d", len(rows), want)
	}
	out := make([]*message.Message, len(rows))
	for i, r := range rows {
		out[i] = r.Message
	}
	return out
}

// eventTypes extracts the event_type header from each message in drain order.
func eventTypes(msgs []*message.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Metadata.Get(messaging.HeaderEventType)
	}
	return out
}
