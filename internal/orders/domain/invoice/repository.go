package invoice

import (
	"context"
	"errors"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
)

// ErrNotFound is returned when no row matches the supplied (tenantID, id). Map to HTTP 404.
var ErrNotFound = errors.New("invoice: not found")

// ErrAlreadyExistsForOrder is returned by [Repository.Add] when an invoice already
// exists for the order. BRD §A-014: one Invoice per Order; cancellation mints a
// CreditNote, not a second Invoice.
var ErrAlreadyExistsForOrder = errors.New("invoice: already exists for order")

// Repository persists [Invoice] aggregates. APPEND-ONLY — no UpdateByID.
//
// GetByOrderID is used by the cancellation flow to decide CreditNote vs
// CancellationNote (ADR 0063 §4).
type Repository interface {
	// Add inserts a new Invoice. Returns [ErrAlreadyExistsForOrder] on
	// order_id partial-unique-index conflict.
	Add(ctx context.Context, inv *Invoice) error

	// GetByID returns the aggregate or [ErrNotFound].
	GetByID(ctx context.Context, tenantID tenant.ID, id ID) (*Invoice, error)

	// GetByOrderID returns the Invoice for the order, or [ErrNotFound].
	GetByOrderID(ctx context.Context, tenantID tenant.ID, orderID order.ID) (*Invoice, error)
}
