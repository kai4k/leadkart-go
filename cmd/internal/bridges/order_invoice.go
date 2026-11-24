package bridges

import (
	"context"
	"errors"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	ordersquery "github.com/leadkart/leadkart-go/internal/orders/app/query"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
)

// OrderInvoice binds the Dispatch dispatch gate (does this order have a tax invoice) to the
// Orders invoice-by-order read. A read that fails is returned as an error, never as "no
// invoice" and never as "invoiced": the caller fails closed.
type OrderInvoice struct {
	Invoices ordersquery.GetInvoiceByOrderHandler
}

// OrderHasInvoice answers Dispatch's question from Orders' read.
func (b OrderInvoice) OrderHasInvoice(ctx context.Context, tenantID tenant.ID, orderID string) (bool, error) {
	_, err := b.Invoices.Handle(ctx, ordersquery.GetInvoiceByOrderQuery{TenantID: tenantID, OrderID: order.ID(orderID)})
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, ordersquery.ErrInvoiceNotFound):
		return false, nil
	default:
		return false, err
	}
}
