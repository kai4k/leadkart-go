//go:build integration

// outbox_subscriber_test.go — outbox assertion shim for the
// platform/adapters package.
//
// Post-ADR-0064: producers write an enveloped row to the shared
// common.outbox relay in the same tx as the aggregate write; one library
// Watermill Forwarder (cmd/worker) drains + republishes. Tests assert the
// PRODUCER contract via messagingtest.DrainOutbox (relay read + envelope
// unwrap). The thin fixture below preserves the prior call sites
// (newPlatformOutboxFixture / forwardAndWait / platformEventTypes).
//
// Platform note: platform-scoped events persist with no tenant FK, so the
// producer omits the tenant_id metadata header for them; subscriber-side
// tests assert the header is empty for platform-scoped events + non-empty
// for tenant-scoped events.

package adapters_test

import (
	"testing"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/messaging/messagingtest"
)

type platformOutboxFixture struct {
	pool *pgxpool.Pool
}

func newPlatformOutboxFixture(t *testing.T) *platformOutboxFixture {
	t.Helper()
	return &platformOutboxFixture{pool: platformPool(t)}
}

// forwardAndWait reads the enveloped rows the producer wrote to
// common.outbox, unwraps them, and asserts the count matches want.
// Returns the inner Watermill messages in drain order. Name kept for
// caller compatibility; post-ADR-0064 the assertion reads the relay
// directly (no forwarder hop).
func (f *platformOutboxFixture) forwardAndWait(t *testing.T, want int) []*message.Message {
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

func platformEventTypes(msgs []*message.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Metadata.Get(messaging.HeaderEventType)
	}
	return out
}
