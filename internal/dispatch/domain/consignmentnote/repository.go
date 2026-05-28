package consignmentnote

import (
	"context"
	"errors"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ErrNotFound — no row for the (tenantID, id) pair. Map to HTTP 404.
var ErrNotFound = errors.New("consignmentnote: not found")

// ErrAlreadyExistsForOrder — partial-unique invariant: at most one
// ConsignmentNote per Order. Re-creating from a retried OrderPacked
// subscriber surfaces this; the subscriber treats it as success
// (idempotent — the slot already exists).
var ErrAlreadyExistsForOrder = errors.New("consignmentnote: already exists for order")

// Repository persists [ConsignmentNote] aggregates.
type Repository interface {
	// Add persists a brand-new note (status=pending). Returns
	// [ErrAlreadyExistsForOrder] when the partial unique index on
	// (tenant_id, order_id) catches a duplicate.
	Add(ctx context.Context, cn *ConsignmentNote) error

	// GetByID returns the aggregate or [ErrNotFound].
	GetByID(ctx context.Context, tenantID tenant.ID, id ID) (*ConsignmentNote, error)

	// GetByOrderID returns the (zero or one) note attached to an order.
	// [ErrNotFound] when no note exists.
	GetByOrderID(ctx context.Context, tenantID tenant.ID, orderID OrderID) (*ConsignmentNote, error)

	// UpdateByID runs mutator inside a UoW tx; (true, nil) persists +
	// drains events; (false, nil) is a no-op; non-nil err rolls back.
	UpdateByID(ctx context.Context, tenantID tenant.ID, id ID,
		mutator func(*ConsignmentNote) (bool, error)) error
}
