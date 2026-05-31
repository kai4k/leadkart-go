package adapters

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/identity/integrationevents"
)

// writeOutboxEvents persists integration events to common.outbox inside
// the supplied tx (same tx as the aggregate write) so they commit
// atomically. ADR 0064/0067: one Watermill Forwarder drains the relay;
// messaging.PublishOutbox stamps destination topic, tenant_id, and act
// claims. This wrapper supplies the module's destination topic.
func writeOutboxEvents(
	ctx context.Context,
	tx pgx.Tx,
	tenantID uuid.UUID,
	events []integrationevents.Event,
) error {
	return messaging.PublishOutbox(ctx, tx, integrationevents.Topic, tenantID, events)
}

// mapDomainEvents translates domain events to integration events for outbox
// storage. Returns an error when the mapper encounters an unknown event type.
// Skips nil-mapped events (emitted directly by an orchestrating service).
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
