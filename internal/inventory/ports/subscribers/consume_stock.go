package subscribers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/app/command"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	ordersevents "github.com/leadkart/leadkart-go/internal/orders/integrationevents"
)

// HandlerConsumeStock is the CI-stable handler name. DO NOT rename.
const HandlerConsumeStock = "inventory.subscribers.ConsumeStock"

// arch-test:idempotency-via-outstanding-ledger — consume no-ops when the order's
// outstanding hold is already zero (a prior consume drained it), so a
// redelivered order_packed.v1 ships nothing twice.

// ConsumeStockSubscriber realises an order's reservation as a shipment when the
// order is packed (orders.order_packed.v1): release the hold + outbound the
// units. The ledger drives which batches — the event carries no line snapshot.
type ConsumeStockSubscriber struct {
	consume command.ConsumeOrderStockHandler
	log     *slog.Logger
}

// NewConsumeStockSubscriber wires the subscriber. log is required.
func NewConsumeStockSubscriber(consume command.ConsumeOrderStockHandler, log *slog.Logger) *ConsumeStockSubscriber {
	if log == nil {
		panic("subscribers: NewConsumeStockSubscriber log required")
	}
	return &ConsumeStockSubscriber{consume: consume, log: log}
}

// Handle is the typed cqrs handler for orders.order_packed.v1.
func (h *ConsumeStockSubscriber) Handle(ctx context.Context, evt *ordersevents.OrderPackedV1) error {
	res, err := h.consume.Handle(ctx, command.ConsumeOrderStockCommand{
		TenantID:          tenant.ID(evt.TenantID),
		OrderID:           evt.OrderID,
		ActorMembershipID: membership.ID(evt.PackedByMembershipID),
	})
	if err != nil {
		return consumeError(err)
	}
	if res.Consumed {
		h.log.InfoContext(ctx, "inventory: reservation consumed as shipment", "order_id", evt.OrderID, "batches", res.BatchCount)
	} else {
		h.log.DebugContext(ctx, "inventory: nothing to consume for packed order (idempotent no-op)", "order_id", evt.OrderID)
	}
	return nil
}

// consumeError classifies a consume failure. Expired stock can never ship, so
// retrying cannot change the answer: the event dead-letters at once and the stuck
// order is visible, not a retry storm. Any other failure is retried.
func consumeError(err error) error {
	if errors.Is(err, batch.ErrExpired) {
		return messaging.NonRetryable(fmt.Errorf("inventory consume-stock: %w", err))
	}
	return fmt.Errorf("inventory consume-stock: %w", err)
}
