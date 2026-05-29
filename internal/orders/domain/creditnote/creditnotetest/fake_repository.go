// Package creditnotetest provides the in-memory FakeRepository
// implementing [creditnote.Repository]. TDL canon.
package creditnotetest

import (
	"cmp"
	"context"
	"slices"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/creditnote"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoice"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoicenumber"
)

// FakeRepository is an in-memory [creditnote.Repository].
type FakeRepository struct {
	Store map[creditnote.ID]*creditnote.CreditNote
}

// NewFakeRepository constructs an empty fake.
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{Store: make(map[creditnote.ID]*creditnote.CreditNote)}
}

// Compile-time interface conformance.
var _ creditnote.Repository = (*FakeRepository)(nil)

// Add satisfies [creditnote.Repository]. Enforces the
// "one cancellation_note per invoice" partial-unique invariant.
func (r *FakeRepository) Add(_ context.Context, c *creditnote.CreditNote) error {
	if c.Kind() == invoicenumber.KindCancellationNote {
		for _, existing := range r.Store {
			if existing.TenantID() == c.TenantID() &&
				existing.InvoiceID() == c.InvoiceID() &&
				existing.Kind() == invoicenumber.KindCancellationNote {
				return creditnote.ErrCancellationAlreadyExists
			}
		}
	}
	r.Store[c.ID()] = c
	return nil
}

// GetByID satisfies [creditnote.Repository].
func (r *FakeRepository) GetByID(
	_ context.Context, tenantID tenant.ID, id creditnote.ID,
) (*creditnote.CreditNote, error) {
	c, ok := r.Store[id]
	if !ok || c.TenantID() != tenantID {
		return nil, creditnote.ErrNotFound
	}
	return c, nil
}

// ListByInvoice satisfies [creditnote.Repository]. Returns issue-order
// (oldest first by IssuedAt then by ID for determinism).
func (r *FakeRepository) ListByInvoice(
	_ context.Context, tenantID tenant.ID, invoiceID invoice.ID,
) ([]*creditnote.CreditNote, error) {
	out := make([]*creditnote.CreditNote, 0)
	for _, c := range r.Store {
		if c.TenantID() == tenantID && c.InvoiceID() == invoiceID {
			out = append(out, c)
		}
	}
	slices.SortFunc(out, func(a, b *creditnote.CreditNote) int {
		return cmp.Or(
			a.IssuedAt().Compare(b.IssuedAt()), // issued_at ASC
			cmp.Compare(a.ID(), b.ID()),        // id ASC tiebreaker
		)
	})
	return out, nil
}
