// Package bridges holds the composition-root bindings that let one module ask
// another a question without importing it. Both host binaries (cmd/api, cmd/worker)
// build the same ones from here.
package bridges

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	inventoryquery "github.com/leadkart/leadkart-go/internal/inventory/app/query"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
)

// HeldStockExpiry binds the Orders expiry guard (does this order hold stock past its
// printed expiry) to the Inventory query. Orders asks it at the pack and again when it
// issues the invoice.
type HeldStockExpiry struct {
	Held inventoryquery.OrderHoldExpiryHandler
}

// HoldsExpiredStock answers Orders' question from Inventory's read.
func (b HeldStockExpiry) HoldsExpiredStock(ctx context.Context, tenantID tenant.ID, orderID order.ID) (bool, error) {
	return b.Held.Handle(ctx, inventoryquery.OrderHoldExpiryQuery{TenantID: tenantID, OrderID: orderID.String()})
}
