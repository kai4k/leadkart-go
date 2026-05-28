package creditnote

import (
	"context"
	"errors"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoice"
)

// ErrNotFound — no row for the supplied (tenantID, id). Map to HTTP 404.
var ErrNotFound = errors.New("creditnote: not found")

// ErrCancellationAlreadyExists — a CancellationNote already exists for
// the supplied invoice. Partial unique index `(tenant_id, invoice_id)
// WHERE kind = 'cancellation_note'` catches this. CreditNotes (post-
// delivery returns) can stack multiply, so no equivalent ban for them.
var ErrCancellationAlreadyExists = errors.New("creditnote: cancellation note already exists for invoice")

// Repository persists [CreditNote] aggregates. APPEND-ONLY — no
// UpdateByID.
type Repository interface {
	// Add persists a brand-new CreditNote. Returns
	// [ErrCancellationAlreadyExists] when the partial unique index
	// catches a second CancellationNote on the same invoice.
	Add(ctx context.Context, c *CreditNote) error

	// GetByID returns the aggregate or [ErrNotFound].
	GetByID(ctx context.Context, tenantID tenant.ID, id ID) (*CreditNote, error)

	// ListByInvoice returns every CreditNote attached to the supplied
	// Invoice in issue order. Empty list (not error) when none exist.
	ListByInvoice(ctx context.Context, tenantID tenant.ID, invoiceID invoice.ID) ([]*CreditNote, error)
}
