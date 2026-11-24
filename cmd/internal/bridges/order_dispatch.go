package bridges

import (
	"context"
	"errors"

	dispatchcommand "github.com/leadkart/leadkart-go/internal/dispatch/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	inventoryquery "github.com/leadkart/leadkart-go/internal/inventory/app/query"
	ordersquery "github.com/leadkart/leadkart-go/internal/orders/app/query"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
)

// OrderDispatch binds the Dispatch gate (may goods leave for this order) to the Orders
// order read. Goods leave only for an order that is `invoiced` or `dispatched` (the saga
// moves the order to `dispatched` while the note is still pending). An order with no
// invoice, or none for this company, is not invoiced; one that has an invoice but is
// cancelled or past dispatch is in the wrong state (an invoice is never removed, so a
// cancelled order still has one). A read that fails is returned as an error: the caller
// fails closed.
type OrderDispatch struct {
	Orders ordersquery.GetOrderHandler
}

// OrderDispatchStatus answers Dispatch's question from Orders' read.
func (b OrderDispatch) OrderDispatchStatus(ctx context.Context, tenantID tenant.ID, orderID string) (dispatchcommand.OrderDispatchStatus, error) {
	v, err := b.Orders.Handle(ctx, ordersquery.GetOrderQuery{TenantID: tenantID, OrderID: order.ID(orderID)})
	if errors.Is(err, ordersquery.ErrOrderNotFound) {
		return dispatchcommand.OrderNotInvoiced, nil
	}
	if err != nil {
		return dispatchcommand.OrderNotInvoiced, err
	}
	switch v.State {
	case order.StateInvoiced.String(), order.StateDispatched.String():
		return dispatchcommand.OrderDispatchable, nil
	case order.StateCancelled.String():
		return dispatchcommand.OrderWrongState, nil
	}
	if v.InvoiceID == "" {
		return dispatchcommand.OrderNotInvoiced, nil
	}
	return dispatchcommand.OrderWrongState, nil
}

// ReservedStockExpiry binds the Dispatch expiry guard (is a batch this order reserved past
// its printed expiry) to the Inventory query that counts every reservation, consumed or not.
type ReservedStockExpiry struct {
	Reserved inventoryquery.OrderReservedExpiryHandler
}

// ReservedStockExpired answers Dispatch's question from Inventory's read.
func (b ReservedStockExpiry) ReservedStockExpired(ctx context.Context, tenantID tenant.ID, orderID string) (bool, error) {
	return b.Reserved.Handle(ctx, inventoryquery.OrderHoldExpiryQuery{TenantID: tenantID, OrderID: orderID})
}
