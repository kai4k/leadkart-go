package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/platform/integrationevents"
)

// OutboxEnqueuer satisfies command.OutboxEnqueuer, writing integration events
// to the shared transactional outbox under the active UoW tx (from ctx).
// Stateless: the tx travels via ctx, so no pool is held.
type OutboxEnqueuer struct{}

// NewOutboxEnqueuer returns a stateless enqueuer.
func NewOutboxEnqueuer() *OutboxEnqueuer { return &OutboxEnqueuer{} }

// ErrNoActiveTx is returned when EnqueueInTx is called outside a UoW.WithinTx
// closure (programmer bug).
var ErrNoActiveTx = errors.New("platform outbox: no active tx in ctx (call from UoW.WithinTx)")

// EnqueueInTx satisfies the app-layer OutboxEnqueuer interface.
func (e *OutboxEnqueuer) EnqueueInTx(ctx context.Context, events ...integrationevents.Event) error {
	if len(events) == 0 {
		return nil
	}
	tx, ok := pg.TxFromContext(ctx)
	if !ok {
		return ErrNoActiveTx
	}
	return writeOutboxEvents(ctx, tx, tenantOfEvents(events), events)
}

// writeOutboxEvents persists events to the shared common.outbox relay inside
// tx (ADR 0064/0067), drained by one Watermill Forwarder; tenant_id /
// occurred_at / act_* travel as metadata stamped by messaging.PublishOutbox.
// tenantID == uuid.Nil omits the tenant_id metadata (platform-scoped event).
func writeOutboxEvents(
	ctx context.Context,
	tx pgx.Tx,
	tenantID uuid.UUID,
	events []integrationevents.Event,
) error {
	return messaging.PublishOutbox(ctx, tx, integrationevents.Topic, tenantID, events)
}

// tenantOfEvents returns the tenant to stamp: the first TenantScoped event's
// tenant, else uuid.Nil. Handlers should not mix platform + tenant-scoped
// events in one call. A malformed TenantID is treated as platform-scoped
// (Nil) — the outbox write must not fail on a misshapen id.
func tenantOfEvents(events []integrationevents.Event) uuid.UUID {
	for _, ev := range events {
		ts, ok := ev.(integrationevents.TenantScoped)
		if !ok {
			continue
		}
		raw := ts.TenantIDString()
		if raw == "" {
			continue
		}
		parsed, err := uuid.Parse(raw)
		if err != nil {
			continue
		}
		return parsed
	}
	return uuid.Nil
}

// drainEventsToOutbox maps domain events via FromDomainEvent (may suppress
// with nil) and persists the result to the outbox under tx. Shared by every
// repository's Add / UpdateByID.
func drainEventsToOutbox(
	ctx context.Context,
	tx pgx.Tx,
	tenantID uuid.UUID,
	domainEvents []any,
) error {
	mapped, err := mapDomainEvents(domainEvents)
	if err != nil {
		return err
	}
	return writeOutboxEvents(ctx, tx, tenantID, mapped)
}

// mapDomainEvents translates domain events through FromDomainEvent, skipping
// any the mapper suppresses (events the handler emits directly with derived
// data, e.g. LeadVerifiedV1 with snapshot).
func mapDomainEvents(domainEvents []any) ([]integrationevents.Event, error) {
	if len(domainEvents) == 0 {
		return nil, nil
	}
	out := make([]integrationevents.Event, 0, len(domainEvents))
	for _, d := range domainEvents {
		ie, err := integrationevents.FromDomainEvent(d)
		if err != nil {
			return nil, fmt.Errorf("platform repo: map domain event %T: %w", d, err)
		}
		if ie == nil {
			continue
		}
		out = append(out, ie)
	}
	return out, nil
}
