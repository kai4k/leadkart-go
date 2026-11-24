package query

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/app/sellability"
)

// HeldStockReader counts the batches an order still holds units on whose printed
// expiry month is over on a given business day.
type HeldStockReader interface {
	CountExpiredBatchesHeldByOrder(ctx context.Context, tenantID tenant.ID, orderRef string, today time.Time) (int64, error)
}

// OrderHoldExpiryHandler answers Orders' pre-pack question: does this order hold
// stock that may no longer be sold? The composition root binds Orders' port to it.
type OrderHoldExpiryHandler struct {
	reader HeldStockReader
	window sellability.Resolver
}

// NewOrderHoldExpiryHandler wires the handler.
func NewOrderHoldExpiryHandler(reader HeldStockReader, window sellability.Resolver) OrderHoldExpiryHandler {
	if reader == nil {
		panic("query: NewOrderHoldExpiryHandler reader required")
	}
	return OrderHoldExpiryHandler{reader: reader, window: window}
}

// OrderHoldExpiryQuery names the order whose held stock is checked.
type OrderHoldExpiryQuery struct {
	TenantID tenant.ID
	OrderID  string
}

// Handle is true when any batch the order holds is past its printed expiry on
// the company's business day. An order that holds nothing is false.
func (h OrderHoldExpiryHandler) Handle(ctx context.Context, q OrderHoldExpiryQuery) (bool, error) {
	tenantID, orderID := q.TenantID, q.OrderID
	if tenantID.IsZero() {
		return false, errors.New("order hold expiry: tenant id required")
	}
	if orderID == "" {
		return false, errors.New("order hold expiry: order id required")
	}
	w, err := h.window.Resolve(ctx, tenantID)
	if err != nil {
		return false, fmt.Errorf("order hold expiry: %w", err)
	}
	n, err := h.reader.CountExpiredBatchesHeldByOrder(ctx, tenantID, orderID, w.Today)
	if err != nil {
		return false, fmt.Errorf("order hold expiry: %w", err)
	}
	return n > 0, nil
}
