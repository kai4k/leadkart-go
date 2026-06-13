package adapters

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/inventory/integrationevents"
)

// OutboxEnqueuer satisfies command.OutboxEnqueuer: it writes enriched
// integration events (StockReservationFailedV1 — carrying derived shortfall
// data no aggregate emits) to the shared transactional outbox under the active
// UoW tx from ctx. Stateless: the tx travels via ctx.
type OutboxEnqueuer struct{}

// NewOutboxEnqueuer returns a stateless enqueuer.
func NewOutboxEnqueuer() *OutboxEnqueuer { return &OutboxEnqueuer{} }

// ErrNoActiveTx is returned when EnqueueInTx runs outside a UoW.WithinTx
// closure (programmer bug).
var ErrNoActiveTx = errors.New("inventory outbox: no active tx in ctx (call from UoW.WithinTx)")

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

// tenantOfEvents returns the tenant to stamp: the first TenantScoped event's
// tenant, else uuid.Nil.
func tenantOfEvents(events []integrationevents.Event) uuid.UUID {
	for _, ev := range events {
		if ts, ok := ev.(integrationevents.TenantScoped); ok {
			return ts.TenantID()
		}
	}
	return uuid.Nil
}

// writeOutboxEvents writes integration events to common.outbox inside tx
// (same transaction as the aggregate mutation). ADR 0064/0067: one shared
// relay drained by the Watermill Forwarder. This wrapper supplies the
// inventory destination topic.
func writeOutboxEvents(
	ctx context.Context,
	tx pgx.Tx,
	tenantID uuid.UUID,
	events []integrationevents.Event,
) error {
	return messaging.PublishOutbox(ctx, tx, integrationevents.Topic, tenantID, events)
}

// mapDomainEvents maps domain events to canonical integration events for
// outbox storage. Returns nil, nil when the slice is empty.
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
