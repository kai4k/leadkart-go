package adapters

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/leadkart/leadkart-go/internal/common/ids"
)

// outboxEvent is the structural contract every domain event must satisfy
// to be writable to the Identity outbox. Each domain package
// (tenant, person, membership, refreshtoken) defines its own concrete
// Event interface with these two methods plus public fields that
// json-marshal cleanly.
//
// Lives in the adapters package (not BuildingBlocks) per TDL canon —
// outbox shape is a persistence concern.
type outboxEvent interface {
	Topic() string
	OccurredAt() time.Time
}

// writeOutboxEvents persists a batch of domain events to identity.outbox
// inside the supplied transaction. Called by every aggregate repository's
// Add / UpdateByID after the state mutation insert/update.
//
// tenantID anchors the row to its tenant for RLS scoping + per-tenant
// audit queries (ADR 0027). Event payload is the domain event struct
// JSON-marshaled — public fields only (verified by reflection at the
// callsite is unnecessary because Go's json package handles unexported
// fields by ignoring them).
//
// Invariants:
//   - Each event becomes one row.
//   - id is UUIDv7 generated here (not derived from event content) so
//     the row insertion order matches occurrence order in B-tree scans.
//   - occurred_at is from the event itself (domain time), distinct from
//     created_at which the column DEFAULTs to now() (insert time).
func writeOutboxEvents(
	ctx context.Context,
	tx pgx.Tx,
	tenantID uuid.UUID,
	events []outboxEvent,
) error {
	if len(events) == 0 {
		return nil
	}
	q := New(tx)
	for _, e := range events {
		payload, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("outbox: marshal %T: %w", e, err)
		}
		err = q.InsertOutboxEvent(ctx, InsertOutboxEventParams{
			ID:         pgUUID(ids.NewV7()),
			TenantID:   pgUUID(tenantID),
			Topic:      e.Topic(),
			Payload:    payload,
			OccurredAt: pgRequiredTimestamp(e.OccurredAt()),
		})
		if err != nil {
			return fmt.Errorf("outbox: insert %s: %w", e.Topic(), err)
		}
	}
	return nil
}
