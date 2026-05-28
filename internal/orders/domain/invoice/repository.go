package invoice

import (
	"context"
	"errors"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
)

// ErrNotFound is returned by [Repository.GetByID] when no row exists
// for the supplied (tenantID, id) pair. Map to HTTP 404.
var ErrNotFound = errors.New("invoice: not found")

// ErrAlreadyExistsForOrder is returned by [Repository.Add] when an
// invoice already exists for the supplied OrderID. Per BRD §A-014:
// one Invoice per Order (cancellation produces a CreditNote, not a
// second Invoice).
var ErrAlreadyExistsForOrder = errors.New("invoice: already exists for order")

// Repository persists [Invoice] aggregates. APPEND-ONLY — there is no
// UpdateByID method because Invoice rows are never mutated post-issue.
//
// `GetByOrderID` is the lookup the cancellation flow needs ("does this
// Order already have an Invoice? → mint CreditNote vs CancellationNote
// per ADR 0063 §4").
type Repository interface {
	// Add persists a brand-new Invoice. Returns
	// [ErrAlreadyExistsForOrder] when the order_id partial unique
	// index catches a second invoice for the same order.
	Add(ctx context.Context, inv *Invoice) error

	// GetByID returns the aggregate or [ErrNotFound].
	GetByID(ctx context.Context, tenantID tenant.ID, id ID) (*Invoice, error)

	// GetByOrderID returns the (zero or one) Invoice attached to the
	// supplied Order. [ErrNotFound] when none exists.
	GetByOrderID(ctx context.Context, tenantID tenant.ID, orderID order.ID) (*Invoice, error)
}
