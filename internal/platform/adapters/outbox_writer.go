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

// OutboxEnqueuer satisfies internal/platform/app/command.OutboxEnqueuer.
// Writes integration events to the shared transactional outbox inside the
// active UoW tx (pulled from ctx via pg.TxFromContext).
//
// Concrete struct + factory keep wiring boundary-clean: cmd/api builds
// this with a single pool dep and passes it to handlers as the
// OutboxEnqueuer interface.
type OutboxEnqueuer struct{}

// NewOutboxEnqueuer returns a zero-stateful enqueuer; the tx travels via
// ctx so no pool is held.
func NewOutboxEnqueuer() *OutboxEnqueuer { return &OutboxEnqueuer{} }

// ErrNoActiveTx surfaces when EnqueueInTx is called outside a
// UoW.WithinTx closure. Programmer bug — surfaces in tests immediately.
var ErrNoActiveTx = errors.New("platform outbox: no active tx in ctx (call from UoW.WithinTx)")

// EnqueueInTx satisfies the app-layer OutboxEnqueuer interface — writes
// each integration event to the outbox under the active tx.
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

// writeOutboxEvents persists integration events to the transactional
// outbox inside tx (same pgx.Tx as the aggregate mutation). Per ADR
// 0064/0067 the outbox is the single shared common.outbox relay drained
// by one Watermill Forwarder; tenant_id / occurred_at / act_* travel as
// message metadata stamped by messaging.PublishOutbox. This wrapper just
// supplies the platform destination topic.
//
// tenantID == uuid.Nil omits the tenant_id metadata (platform-scoped
// event with no owning tenant).
func writeOutboxEvents(
	ctx context.Context,
	tx pgx.Tx,
	tenantID uuid.UUID,
	events []integrationevents.Event,
) error {
	return messaging.PublishOutbox(ctx, tx, integrationevents.Topic, tenantID, events)
}

// tenantOfEvents picks the tenant to stamp on the outbox metadata. Reads
// the first TenantScoped event's tenant; Platform events return uuid.Nil
// (no tenant metadata). Handlers SHOULD NOT mix platform + tenant-scoped
// events in one EnqueueInTx call.
//
// Malformed TenantID (non-UUID) is treated as Platform-scoped (Nil) —
// defensive vs corrupt domain state; the outbox write MUST NOT fail on a
// misshapen identifier (audit-log outage doctrine).
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

// drainEventsToOutbox is the shared "map + persist" helper used by every
// repository: maps domain events through integrationevents.FromDomainEvent
// (which may suppress with nil) + persists the result to the outbox under
// tx. Each repository's Add / UpdateByID calls this once per persist.
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

// mapDomainEvents translates domain events through FromDomainEvent.
// Suppresses (returns nil) when the mapper does — used for events emitted
// directly by the handler with derived data (e.g. LeadVerifiedV1 with
// snapshot).
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
