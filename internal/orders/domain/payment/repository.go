package payment

import (
	"context"
	"errors"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
)

// ErrNotFound — no row for the supplied (tenantID, id). Map to HTTP 404.
var ErrNotFound = errors.New("payment: not found")

// ErrAlreadyExistsForExternalReference — a payment with the same
// (tenant_id, external_reference) already exists. Idempotency catch
// for retried webhooks. Only fires when ExternalReference is non-empty.
var ErrAlreadyExistsForExternalReference = errors.New("payment: already exists for external reference")

// Repository persists [Payment] aggregates. APPEND-ONLY — no
// UpdateByID.
type Repository interface {
	// Add persists a brand-new payment. Returns
	// [ErrAlreadyExistsForExternalReference] when the partial unique
	// index catches a duplicate ExternalReference.
	Add(ctx context.Context, p *Payment) error

	// GetByID returns the aggregate or [ErrNotFound].
	GetByID(ctx context.Context, tenantID tenant.ID, id ID) (*Payment, error)

	// ListByOrder returns every payment attached to the order in
	// receipt order. Empty list (not error) when none exist.
	ListByOrder(ctx context.Context, tenantID tenant.ID, orderID order.ID) ([]*Payment, error)
}
