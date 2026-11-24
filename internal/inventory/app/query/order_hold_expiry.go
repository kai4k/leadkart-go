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

// ReservedStockReader counts the batches an order ever reserved units on whose printed
// expiry month is over on a given business day, releases and consumes not netted.
type ReservedStockReader interface {
	CountExpiredBatchesEverReservedByOrder(ctx context.Context, tenantID tenant.ID, orderRef string, today time.Time) (int64, error)
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

// OrderReservedExpiryHandler answers Dispatch's question while a consignment note is still
// pending: do the goods of this order sit on a batch that is past its printed expiry? A
// consume drains the hold when the order is packed, but the goods stay in the warehouse
// until the carrier takes them, so this counts every batch the order EVER reserved on. It
// uses the same business day and the same "past" ([sellability.Resolver], the common/expiry
// bound) as [OrderHoldExpiryHandler]: no second expiry rule.
type OrderReservedExpiryHandler struct {
	reader ReservedStockReader
	window sellability.Resolver
}

// NewOrderReservedExpiryHandler wires the handler.
func NewOrderReservedExpiryHandler(reader ReservedStockReader, window sellability.Resolver) OrderReservedExpiryHandler {
	if reader == nil {
		panic("query: NewOrderReservedExpiryHandler reader required")
	}
	return OrderReservedExpiryHandler{reader: reader, window: window}
}

// Handle is true when any batch the order ever reserved is past its printed expiry on the
// company's business day. An order that reserved nothing is false.
func (h OrderReservedExpiryHandler) Handle(ctx context.Context, q OrderHoldExpiryQuery) (bool, error) {
	if q.TenantID.IsZero() {
		return false, errors.New("order reserved expiry: tenant id required")
	}
	if q.OrderID == "" {
		return false, errors.New("order reserved expiry: order id required")
	}
	w, err := h.window.Resolve(ctx, q.TenantID)
	if err != nil {
		return false, fmt.Errorf("order reserved expiry: %w", err)
	}
	n, err := h.reader.CountExpiredBatchesEverReservedByOrder(ctx, q.TenantID, q.OrderID, w.Today)
	if err != nil {
		return false, fmt.Errorf("order reserved expiry: %w", err)
	}
	return n > 0, nil
}
