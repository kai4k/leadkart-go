package adapters

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/identity/integrationevents"
)

// writeOutboxEvents persists integration events to the transactional
// outbox inside tx — the same pgx.Tx as the aggregate state mutation, so
// the rows commit atomically with the write (Brandur "Transactionally
// staged job drains"; TDL outbox canon). Every aggregate repository's
// Add / UpdateByID calls this after the state insert/update.
//
// Per ADR 0064/0067 the outbox is a single shared relay (common.outbox)
// drained by one Watermill Forwarder; the destination topic
// (identity.events) + tenant_id + occurred_at + the RFC 8693 act_* claim
// all travel as message metadata, not as queryable columns. The shared
// messaging.PublishOutbox helper stamps all of that — including the act
// claim pulled from ctx (ADR 0056) — so this wrapper just supplies the
// module's destination topic.
func writeOutboxEvents(
	ctx context.Context,
	tx pgx.Tx,
	tenantID uuid.UUID,
	events []integrationevents.Event,
) error {
	return messaging.PublishOutbox(ctx, tx, integrationevents.Topic, tenantID, events)
}

// mapDomainEvents translates a slice of domain events into the canonical
// integration events for outbox storage. Surfaces a clear error if the
// mapper hasn't been taught a new domain-event type (the test suite
// catches this in CI). Skips events the mapper deliberately suppresses
// (returns nil) — used when an orchestrating service emits the
// integration event directly rather than via aggregate-driven drain.
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
