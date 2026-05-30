//go:build integration

// outbox_subscriber_test.go — outbox assertion fixture for the identity
// adapters package.
//
// Post-ADR-0064: producers write an enveloped row to the shared
// common.outbox relay in the same tx as the aggregate write; one library
// Watermill Forwarder (cmd/worker) drains + republishes. Tests assert the
// PRODUCER contract — the right enveloped rows landed in common.outbox —
// via messagingtest.DrainOutbox, which reads the relay + unwraps the
// forwarder envelope. The forwarder hop itself is library code (tested by
// watermill-sql), not ours to re-verify.

package adapters_test

import (
	"testing"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/messaging/messagingtest"
)

// drainOutboxRows exposes the raw unwrapped relay rows (incl. the
// destination topic) for tests that need to assert the forwarder routing
// target, not just the inner message.
func drainOutboxRows(t *testing.T, f *outboxFixture) []messagingtest.OutboxRow {
	t.Helper()
	return messagingtest.DrainOutbox(t.Context(), t, f.pool)
}

// outboxFixture holds the shared package pool. Caller pattern:
//
//	sharedPG.TruncateAll(t)  // serial + clean slate
//	fix := newOutboxFixture(t)
//	... drive production code (repository Add/UpdateByID) ...
//	msgs := fix.forwardAndWait(t, wantCount)
//	... assert msgs[i].Metadata.Get(messaging.HeaderEventType) etc ...
type outboxFixture struct {
	pool *pgxpool.Pool
}

// newOutboxFixture returns the shared package-scoped Postgres pool wrapper.
func newOutboxFixture(t *testing.T) *outboxFixture {
	t.Helper()
	return &outboxFixture{pool: repoFixture(t)}
}

// forwardAndWait reads the enveloped rows the producer wrote to
// common.outbox, unwraps them, and asserts the count matches want (the
// number of events the production code under test emits). Returns the
// unwrapped Watermill messages in drain order ("offset" ASC) so callers
// assert on metadata + payload exactly as the forwarder would republish
// them. Name kept for caller compatibility; post-ADR-0064 there is no
// forwarder hop in the assertion — the relay is read directly.
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

// eventTypes extracts the event_type header from each message in drain
// order — the common "which events did the producer emit" assertion.
func eventTypes(msgs []*message.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Metadata.Get(messaging.HeaderEventType)
	}
	return out
}
