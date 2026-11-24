package bridges

import (
	"context"
	"errors"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	dispatchcommand "github.com/leadkart/leadkart-go/internal/dispatch/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	inventorycommand "github.com/leadkart/leadkart-go/internal/inventory/app/command"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	orderscommand "github.com/leadkart/leadkart-go/internal/orders/app/command"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
)

// ErrNoActiveTx is returned by a dispatch step called with a ctx that carries no
// transaction. Both steps are only ever one part of Dispatch's single transaction
// (ADR 0072); run on their own they would each open a transaction of their own and the
// goods could leave without the stock or the order moving. They fail closed instead.
//
// This is the trap the guard exists for: pg.Transactor.WithinTx and WithinTenantTx always
// open a NEW transaction, they never join one already in ctx. A step that wrapped itself in
// either would silently take a second connection and no longer roll back with the rest.
var ErrNoActiveTx = errors.New("dispatch step: no active transaction in ctx (call from the dispatch transaction)")

// OrderShipper binds Dispatch's order step (move the order invoiced → dispatched when the
// goods leave) to the Orders command. The command uses the order repository, which joins the
// transaction in ctx.
type OrderShipper struct {
	Dispatch orderscommand.DispatchOrderHandler
}

// MarkOrderDispatched runs the Orders step inside the dispatch transaction and translates
// Orders' refusals into Dispatch's. An order that does not exist for this company reads as
// not invoiced (the Orders command already answers so).
func (b OrderShipper) MarkOrderDispatched(ctx context.Context, tenantID tenant.ID, orderID, consignmentNoteID string, actor membership.ID) ([]dispatchcommand.ProductQuantity, error) {
	if _, ok := pg.TxFromContext(ctx); !ok {
		return nil, ErrNoActiveTx
	}
	confirmed, err := b.Dispatch.Dispatch(ctx, orderscommand.DispatchOrderCommand{
		TenantID: tenantID, OrderID: order.ID(orderID), ConsignmentNoteID: consignmentNoteID, TransitionedByMembership: actor,
	})
	switch {
	case err == nil:
		out := make([]dispatchcommand.ProductQuantity, 0, len(confirmed))
		for _, c := range confirmed {
			out = append(out, dispatchcommand.ProductQuantity{ProductID: c.ProductID, Quantity: c.Quantity})
		}
		return out, nil
	case errors.Is(err, orderscommand.ErrDispatchNotInvoiced):
		return nil, fmt.Errorf("%w: %w", dispatchcommand.ErrOrderNotInvoiced, err)
	case errors.Is(err, orderscommand.ErrDispatchWrongState):
		return nil, fmt.Errorf("%w: %w", dispatchcommand.ErrOrderNotDispatchable, err)
	}
	return nil, err
}

// StockShipper binds Dispatch's stock step (ship the order's hold when the goods leave) to
// the Inventory command. The command and every repository it calls join the transaction in
// ctx; Inventory's batch rule is the one authority on expiry.
type StockShipper struct {
	Ship inventorycommand.ShipOrderStockHandler
}

// ShipOrderStock runs the Inventory step inside the dispatch transaction and translates
// Inventory's refusals into Dispatch's.
func (b StockShipper) ShipOrderStock(ctx context.Context, tenantID tenant.ID, orderID string, confirmed []dispatchcommand.ProductQuantity, actor membership.ID) error {
	if _, ok := pg.TxFromContext(ctx); !ok {
		return ErrNoActiveTx
	}
	expected := make([]inventorycommand.ProductQuantity, 0, len(confirmed))
	for _, c := range confirmed {
		expected = append(expected, inventorycommand.ProductQuantity{ProductID: c.ProductID, Quantity: c.Quantity})
	}
	_, err := b.Ship.Handle(ctx, inventorycommand.ShipOrderStockCommand{
		TenantID: tenantID, OrderID: orderID, ActorMembershipID: actor, Expected: expected,
	})
	switch {
	case err == nil:
		return nil
	case errors.Is(err, inventorycommand.ErrOrderStockNotHeld):
		return fmt.Errorf("%w: %w", dispatchcommand.ErrStockNotHeld, err)
	case errors.Is(err, inventorycommand.ErrOrderStockMismatch):
		return fmt.Errorf("%w: %w", dispatchcommand.ErrStockMismatch, err)
	case errors.Is(err, batch.ErrExpired):
		return fmt.Errorf("%w: %w", dispatchcommand.ErrStockExpired, err)
	}
	return err
}
