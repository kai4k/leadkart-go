package payment

import (
	"context"
	"errors"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
)

// ErrNotFound — no row for the supplied (tenantID, id). Map to HTTP 404.
var ErrNotFound = errors.New("payment: not found")

// ErrAlreadyExistsForExternalReference — duplicate (tenant_id,
// external_reference). Idempotency catch for retried webhooks; fires only when
// ExternalReference is non-empty.
var ErrAlreadyExistsForExternalReference = errors.New("payment: already exists for external reference")

// Repository persists [Payment] aggregates. Append-only — no UpdateByID.
type Repository interface {
	// Add persists a new payment, returning
	// [ErrAlreadyExistsForExternalReference] on a duplicate ExternalReference.
	Add(ctx context.Context, p *Payment) error

	// GetByID returns the aggregate or [ErrNotFound].
	GetByID(ctx context.Context, tenantID tenant.ID, id ID) (*Payment, error)

	// ListByOrder returns the order's payments in receipt order; empty, not
	// error, when none.
	ListByOrder(ctx context.Context, tenantID tenant.ID, orderID order.ID) ([]*Payment, error)
}
