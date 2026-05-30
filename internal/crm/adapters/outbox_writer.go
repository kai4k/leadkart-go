// Package adapters holds CRM-module outbound adapters per ADR 0002:
// pg-backed repositories + outbox writer. Concrete (non-interface) types
// — domain consumers in internal/crm/app/ depend on the interfaces
// declared in internal/crm/domain/*/repository.go.
package adapters

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/crm/integrationevents"
)

// writeOutboxEvents persists integration events to the transactional
// outbox inside tx (same pgx.Tx as the aggregate mutation). Per ADR
// 0064/0067 the outbox is the single shared common.outbox relay drained
// by one Watermill Forwarder; tenant_id / occurred_at / act_* travel as
// message metadata stamped by messaging.PublishOutbox. This wrapper just
// supplies the CRM destination topic.
func writeOutboxEvents(
	ctx context.Context,
	tx pgx.Tx,
	tenantID uuid.UUID,
	events []integrationevents.Event,
) error {
	return messaging.PublishOutbox(ctx, tx, integrationevents.Topic, tenantID, events)
}

// mapDomainEvents translates a slice of CRM domain events into V1
// integration events for outbox storage. Surfaces a clear error if a
// UUID inside an event fails to parse (reviewer H6); panics if the mapper
// hasn't been taught a new domain-event type (reviewer H5 — fail-loud
// rather than silent-skip), so a nil result is impossible for non-empty
// input.
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
		out = append(out, ie)
	}
	return out, nil
}
