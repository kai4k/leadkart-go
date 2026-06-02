//go:build integration

// Package messagingtest provides integration-test helpers for asserting
// on the transactional outbox without driving the production Watermill
// Forwarder (which is library code, tested by watermill-sql itself).
//
// The canonical assertion after ADR 0064: the producer wrote an enveloped
// row to common.outbox in the same tx as the aggregate write. DrainOutbox
// reads those rows + unwraps the forwarder envelope so tests can assert on
// the destination topic, event_type / tenant metadata, and payload that
// the forwarder will republish — the observable contract, minus the broker
// hop.
package messagingtest

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
)

// OutboxRow is one unwrapped outbox entry: the destination module topic
// the Forwarder would republish to, plus the inner Watermill message
// (payload + metadata) the subscriber would receive.
type OutboxRow struct {
	DestinationTopic string
	Message          *message.Message
}

// envelope mirrors the watermill forwarder's wire envelope (an unexported
// type in components/forwarder, so the JSON shape is re-declared here —
// keep in lockstep with components/forwarder/envelope.go).
type envelope struct {
	DestinationTopic string            `json:"destination_topic"`
	UUID             string            `json:"uuid"`
	Payload          []byte            `json:"payload"`
	Metadata         map[string]string `json:"metadata"`
}

// DrainOutbox reads every row currently in common.outbox (oldest first by
// "offset"), unwraps each forwarder envelope, and returns them. Read-only
// — it does NOT delete rows, so callers may assert repeatedly. Ordering
// matches the Forwarder's drain order ("offset" ASC).
//
// Use after driving production code that writes to the outbox (a repository
// Add/UpdateByID, or a handler's EnqueueInTx) to assert the
// transactional-outbox contract.
func DrainOutbox(ctx context.Context, t *testing.T, pool *pgxpool.Pool) []OutboxRow {
	t.Helper()

	//nolint:gosec // test helper; table name is a package constant, not user input.
	q := fmt.Sprintf(`SELECT payload FROM %s ORDER BY "offset" ASC`, messaging.OutboxTable)
	rows, err := pool.Query(ctx, q)
	if err != nil {
		t.Fatalf("messagingtest: query %s: %v", messaging.OutboxTable, err)
	}
	defer rows.Close()

	var out []OutboxRow
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("messagingtest: scan outbox row: %v", err)
		}
		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("messagingtest: unmarshal envelope: %v", err)
		}
		msg := message.NewMessage(env.UUID, env.Payload)
		msg.Metadata = env.Metadata
		out = append(out, OutboxRow{DestinationTopic: env.DestinationTopic, Message: msg})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("messagingtest: iterate outbox rows: %v", err)
	}
	return out
}

// RowsForTopic returns only the rows whose destination topic matches —
// the canonical way to assert on THIS module's event after ADR 0064, since
// the shared common.outbox relay also carries events from cross-module
// test seeds (e.g. an inventory test that seeds a tenant emits an identity
// event into the same relay). Filter to your module's topic, then count.
func RowsForTopic(rows []OutboxRow, topic string) []OutboxRow {
	out := make([]OutboxRow, 0, len(rows))
	for _, r := range rows {
		if r.DestinationTopic == topic {
			out = append(out, r)
		}
	}
	return out
}

// EventTypes returns the event_type metadata header of each row, in drain
// order — the common assertion for "which events did the producer emit".
func EventTypes(rows []OutboxRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Message.Metadata.Get(messaging.HeaderEventType)
	}
	return out
}
