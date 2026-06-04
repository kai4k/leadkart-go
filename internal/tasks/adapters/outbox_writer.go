package adapters

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/tasks/integrationevents"
)

// writeOutboxEvents persists integration events to the transactional
// outbox inside tx (same pgx.Tx as the aggregate mutation). Per ADR
// 0064/0067 the outbox is the single shared common.outbox relay drained
// by one Watermill Forwarder; tenant_id / occurred_at / act_* travel as
// message metadata stamped by messaging.PublishOutbox. This wrapper just
// supplies the Tasks destination topic.
func writeOutboxEvents(
	ctx context.Context,
	tx pgx.Tx,
	tenantID uuid.UUID,
	events []integrationevents.Event,
) error {
	return messaging.PublishOutbox(ctx, tx, integrationevents.Topic, tenantID, events)
}

// mapDomainEvents translates a slice of Tasks domain events into V1
// integration events for outbox storage. Filters out events that have
// no wire counterpart (e.g. StartedEvent — see
// integrationevents.IsNotMapped); surfaces any other mapping error.
func mapDomainEvents(domainEvents []any) ([]integrationevents.Event, error) {
	if len(domainEvents) == 0 {
		return nil, nil
	}
	out := make([]integrationevents.Event, 0, len(domainEvents))
	for _, d := range domainEvents {
		ie, err := integrationevents.FromDomainEvent(d)
		if err != nil {
			if integrationevents.IsNotMapped(err) {
				continue
			}
			return nil, err
		}
		out = append(out, ie)
	}
	return out, nil
}
