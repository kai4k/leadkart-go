// Package paymenttest provides the in-memory FakeRepository
// implementing [payment.Repository]. TDL canon.
package paymenttest

import (
	"context"
	"sort"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
	"github.com/leadkart/leadkart-go/internal/orders/domain/payment"
)

// FakeRepository is an in-memory [payment.Repository].
type FakeRepository struct {
	Store map[payment.ID]*payment.Payment
}

// NewFakeRepository constructs an empty fake.
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{Store: make(map[payment.ID]*payment.Payment)}
}

// Compile-time interface conformance.
var _ payment.Repository = (*FakeRepository)(nil)

// Add satisfies [payment.Repository]. Enforces the partial-unique
// (tenant_id, external_reference) invariant when ExternalReference is
// non-empty.
func (r *FakeRepository) Add(_ context.Context, p *payment.Payment) error {
	if ref := p.ExternalReference(); ref != "" {
		for _, existing := range r.Store {
			if existing.TenantID() == p.TenantID() && existing.ExternalReference() == ref {
				return payment.ErrAlreadyExistsForExternalReference
			}
		}
	}
	r.Store[p.ID()] = p
	return nil
}

// GetByID satisfies [payment.Repository].
func (r *FakeRepository) GetByID(
	_ context.Context, tenantID tenant.ID, id payment.ID,
) (*payment.Payment, error) {
	p, ok := r.Store[id]
	if !ok || p.TenantID() != tenantID {
		return nil, payment.ErrNotFound
	}
	return p, nil
}

// ListByOrder satisfies [payment.Repository]. Returns receipt-order
// (oldest first by ReceivedAt then by ID for determinism).
func (r *FakeRepository) ListByOrder(
	_ context.Context, tenantID tenant.ID, orderID order.ID,
) ([]*payment.Payment, error) {
	out := make([]*payment.Payment, 0)
	for _, p := range r.Store {
		if p.TenantID() == tenantID && p.OrderID() == orderID {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ReceivedAt().Equal(out[j].ReceivedAt()) {
			return out[i].ID() < out[j].ID()
		}
		return out[i].ReceivedAt().Before(out[j].ReceivedAt())
	})
	return out, nil
}
