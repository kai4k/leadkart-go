// Package invoicetest provides the in-memory FakeRepository
// implementing [invoice.Repository]. TDL canon.
package invoicetest

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoice"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
)

// FakeRepository is an in-memory [invoice.Repository]. Zero-value-NOT-
// usable — construct via [NewFakeRepository].
type FakeRepository struct {
	Store      map[invoice.ID]*invoice.Invoice
	ByOrderID  map[order.ID]invoice.ID
}

// NewFakeRepository constructs an empty fake.
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{
		Store:     make(map[invoice.ID]*invoice.Invoice),
		ByOrderID: make(map[order.ID]invoice.ID),
	}
}

// Compile-time interface conformance.
var _ invoice.Repository = (*FakeRepository)(nil)

// Add satisfies [invoice.Repository]. Rejects duplicate order_id.
func (r *FakeRepository) Add(_ context.Context, inv *invoice.Invoice) error {
	if _, ok := r.ByOrderID[inv.OrderID()]; ok {
		return invoice.ErrAlreadyExistsForOrder
	}
	r.Store[inv.ID()] = inv
	r.ByOrderID[inv.OrderID()] = inv.ID()
	return nil
}

// GetByID satisfies [invoice.Repository].
func (r *FakeRepository) GetByID(
	_ context.Context, tenantID tenant.ID, id invoice.ID,
) (*invoice.Invoice, error) {
	inv, ok := r.Store[id]
	if !ok || inv.TenantID() != tenantID {
		return nil, invoice.ErrNotFound
	}
	return inv, nil
}

// GetByOrderID satisfies [invoice.Repository].
func (r *FakeRepository) GetByOrderID(
	_ context.Context, tenantID tenant.ID, orderID order.ID,
) (*invoice.Invoice, error) {
	id, ok := r.ByOrderID[orderID]
	if !ok {
		return nil, invoice.ErrNotFound
	}
	inv := r.Store[id]
	if inv.TenantID() != tenantID {
		return nil, invoice.ErrNotFound
	}
	return inv, nil
}
