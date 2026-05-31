//go:build integration

// Outbox assertion shim for the platform/adapters package.
//
// Post-ADR-0064: producers write an enveloped row to the shared common.outbox
// relay in the aggregate's tx; one Watermill Forwarder drains + republishes.
// Tests assert the PRODUCER contract via messagingtest.DrainOutbox (relay read
// + envelope unwrap). Platform-scoped events carry no tenant FK, so the
// producer omits the tenant_id header for them.

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

// forwardAndWait drains common.outbox, unwraps the rows, asserts the count is
// want, and returns the inner messages in drain order. Post-ADR-0064 this
// reads the relay directly (no forwarder hop); the name is kept for callers.
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
