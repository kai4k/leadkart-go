package adapters

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/adapters/db"
	"github.com/leadkart/leadkart-go/internal/identity/integrationevents"
)

// writeOutboxEvents persists a batch of framework-neutral integration
// events to identity.outbox inside the supplied transaction. Called by
// every aggregate repository's Add / UpdateByID after the state-mutation
// insert/update.
//
// Per ADR 0027 + Brandur Leach "events table" pattern: outbox doubles
// as audit log, single-tx with the aggregate write. Per `messaging.md`
// "Composition, not inheritance": payload carries primitive fields only
// (no domain VOs); envelope metadata travels separately on the
// Watermill envelope at forwarding time.
//
// Invariants:
//   - One row per event.
//   - Row id is UUIDv7 generated here (B-tree locality on insertion
//     order; not derived from event content so the same logical event
//     can be re-emitted under retry without ID collision).
//   - tenant_id anchors the row for RLS scoping + per-tenant audit
//     queries. Platform-scoped events (e.g. PersonAnonymisedV1) use
//     uuid.Nil; outbox INSERT runs under TxScopePlatform so the
//     `WITH CHECK (tenant_id = app.current_tenant() OR app.is_platform())`
//     policy passes regardless.
//   - occurred_at is the domain-time on the event itself (distinct
//     from created_at which DEFAULTs to now() at insert time).
func writeOutboxEvents(
	ctx context.Context,
	tx pgx.Tx,
	tenantID uuid.UUID,
	events []integrationevents.Event,
) error {
	if len(events) == 0 {
		return nil
	}
	q := db.New(tx)
	for _, e := range events {
		payload, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("outbox: marshal %s: %w", e.Topic(), err)
		}
		err = q.InsertOutboxEvent(ctx, db.InsertOutboxEventParams{
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

// mapDomainEvents translates a slice of domain events into the
// canonical integration events for outbox storage. Surfaces a clear
// error if the mapper hasn't been taught a new domain-event type
// (the test suite catches this in CI).
//
// Skips events the mapper deliberately suppresses (returns nil) — used
// when an orchestrating service emits the integration event directly
// rather than via aggregate-driven drain.
func mapDomainEvents(domainEvents []any) ([]integrationevents.Event, error) {
	if len(domainEvents) == 0 {
		return nil, nil
	}
	out := make([]integrationevents.Event, 0, len(domainEvents))
	for _, d := range domainEvents {
		ie, err := integrationevents.FromDomainEvent(d)
		if err != nil {
			return nil, err
		}
		if ie == nil {
			continue
		}
		out = append(out, ie)
	}
	return out, nil
}
