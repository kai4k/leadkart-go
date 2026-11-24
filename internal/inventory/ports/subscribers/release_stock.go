package subscribers

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/app/command"
	ordersevents "github.com/leadkart/leadkart-go/internal/orders/integrationevents"
)

// HandlerReleaseStock is the CI-stable handler name. DO NOT rename.
const HandlerReleaseStock = "inventory.subscribers.ReleaseStock"

// arch-test:idempotency-via-outstanding-ledger — release no-ops when the order's
// outstanding hold is already zero (a prior release drained it, or it was
// shipped at dispatch), so a redelivered order_cancelled.v1 releases nothing twice.

// ReleaseStockSubscriber returns an order's outstanding reservation to
// available when the order is cancelled (orders.order_cancelled.v1). It is
// PriorState-agnostic: the ledger says what is still held, so a cancel before
// dispatch releases the hold while a cancel after the shipment (ledger already zero)
// no-ops — the shipped goods are an operator return, handled elsewhere.
type ReleaseStockSubscriber struct {
	release command.ReleaseOrderStockHandler
	log     *slog.Logger
}

// NewReleaseStockSubscriber wires the subscriber. log is required.
func NewReleaseStockSubscriber(release command.ReleaseOrderStockHandler, log *slog.Logger) *ReleaseStockSubscriber {
	if log == nil {
		panic("subscribers: NewReleaseStockSubscriber log required")
	}
	return &ReleaseStockSubscriber{release: release, log: log}
}

// Handle is the typed cqrs handler for orders.order_cancelled.v1.
func (h *ReleaseStockSubscriber) Handle(ctx context.Context, evt *ordersevents.OrderCancelledV1) error {
	res, err := h.release.Handle(ctx, command.ReleaseOrderStockCommand{
		TenantID:          tenant.ID(evt.TenantID),
		OrderID:           evt.OrderID,
		ActorMembershipID: membership.ID(evt.CancelledByMembership),
		Reason:            evt.Reason,
	})
	if err != nil {
		return fmt.Errorf("inventory release-stock: %w", err)
	}
	if res.Released {
		h.log.InfoContext(ctx, "inventory: reservation released on cancel", "order_id", evt.OrderID, "batches", res.BatchCount)
	} else {
		h.log.DebugContext(ctx, "inventory: nothing to release for cancelled order (idempotent no-op)", "order_id", evt.OrderID)
	}
	return nil
}
