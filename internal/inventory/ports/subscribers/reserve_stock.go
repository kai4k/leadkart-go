// Package subscribers holds the Inventory module's inbound event consumers.
// Per ADR 0063 §4 (true-hold reservation), Inventory reacts to the Orders
// lifecycle to keep stock holds honest:
//
//	orders.order_confirmed.v1 → reserve FEFO stock (oversell guard)
//	orders.order_cancelled.v1 → release the hold (if still held)
//
// The hold is shipped by Dispatch's own transaction when the goods leave
// (ADR 0072), not by an event: orders.order_packed.v1 no longer touches stock.
//
// Cross-module by integration event only — Inventory imports the Orders
// integrationevents contract, never its domain/app (the sanctioned EDA seam).
package subscribers

import (
	"context"
	"fmt"
	"log/slog"

	ordersevents "github.com/leadkart/leadkart-go/internal/orders/integrationevents"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/app/command"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// HandlerReserveStock is the CI-stable handler name. DO NOT rename.
const HandlerReserveStock = "inventory.subscribers.ReserveStock"

// arch-test:idempotency-via-reservation-ledger-precheck — the reserve handler's
// CountReservationsForOrder short-circuits a redelivered order_confirmed.v1
// before any reservation is written (router IdempotencyMiddleware is primary).

// ReserveStockSubscriber reserves FEFO stock for a confirmed order
// (orders.order_confirmed.v1). On shortfall the reserve handler emits
// StockReservationFailedV1 (ops alert) and reserves nothing — the order is
// accepted but flagged, never silently oversold.
type ReserveStockSubscriber struct {
	reserve command.ReserveOrderStockHandler
	log     *slog.Logger
}

// NewReserveStockSubscriber wires the subscriber. log is required.
func NewReserveStockSubscriber(reserve command.ReserveOrderStockHandler, log *slog.Logger) *ReserveStockSubscriber {
	if log == nil {
		panic("subscribers: NewReserveStockSubscriber log required")
	}
	return &ReserveStockSubscriber{reserve: reserve, log: log}
}

// Handle is the typed cqrs handler for orders.order_confirmed.v1.
func (h *ReserveStockSubscriber) Handle(ctx context.Context, evt *ordersevents.OrderConfirmedV1) error {
	lines := aggregateLines(evt.Items)
	if len(lines) == 0 {
		h.log.InfoContext(ctx, "inventory: order confirmed with no line items — nothing to reserve", "order_id", evt.OrderID)
		return nil
	}
	res, err := h.reserve.Handle(ctx, command.ReserveOrderStockCommand{
		TenantID:          tenant.ID(evt.TenantID),
		OrderID:           evt.OrderID,
		ActorMembershipID: membership.ID(evt.ConfirmedByMembershipID),
		Lines:             lines,
	})
	if err != nil {
		return fmt.Errorf("inventory reserve-stock: %w", err)
	}
	switch {
	case res.AlreadyReserved:
		h.log.DebugContext(ctx, "inventory: order already reserved (idempotent no-op)", "order_id", evt.OrderID)
	case len(res.Shortfalls) > 0:
		h.log.WarnContext(ctx, "inventory: reservation shortfall — order accepted but stock insufficient, ops alert emitted",
			"order_id", evt.OrderID, "shortfall_lines", len(res.Shortfalls))
	case res.Reserved:
		h.log.InfoContext(ctx, "inventory: stock reserved for order", "order_id", evt.OrderID, "lines", len(lines))
	}
	return nil
}

// aggregateLines sums quantities per product so a duplicate product across
// order lines reserves once against the shared available pool (avoids
// double-counting available at plan time).
func aggregateLines(items []ordersevents.OrderLineItem) []command.ReserveLine {
	byProduct := make(map[string]int64, len(items))
	order := make([]string, 0, len(items))
	for _, it := range items {
		if _, seen := byProduct[it.ProductID]; !seen {
			order = append(order, it.ProductID)
		}
		byProduct[it.ProductID] += int64(it.Quantity)
	}
	lines := make([]command.ReserveLine, 0, len(order))
	for _, pid := range order {
		lines = append(lines, command.ReserveLine{ProductID: product.ID(pid), Quantity: byProduct[pid]})
	}
	return lines
}
