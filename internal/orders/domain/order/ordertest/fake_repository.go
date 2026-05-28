// Package ordertest provides the in-memory FakeRepository implementing
// [order.Repository]. TDL canon — per-aggregate fakes live in
// <aggregate>test/ sibling packages of the aggregate they fake.
// Single-test-owner pattern.
package ordertest

import (
	"context"
	"errors"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
)

// FakeRepository is the in-memory implementation of [order.Repository].
// Zero-value-NOT-usable — use [NewFakeRepository].
type FakeRepository struct {
	Store         map[order.ID]*order.Order
	DrainedEvents []order.Event

	// ForceAddError is consumed by the next Add call + then clears.
	ForceAddError error
}

// ErrAlreadyExists mirrors the adapter's 23505-on-PK translation.
var ErrAlreadyExists = errors.New("ordertest: already exists")

// NewFakeRepository constructs an empty fake.
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{Store: make(map[order.ID]*order.Order)}
}

// Compile-time interface conformance.
var _ order.Repository = (*FakeRepository)(nil)

// Add satisfies [order.Repository].
func (r *FakeRepository) Add(_ context.Context, o *order.Order) error {
	if r.ForceAddError != nil {
		err := r.ForceAddError
		r.ForceAddError = nil
		return err
	}
	if existing, ok := r.Store[o.ID()]; ok && existing.TenantID() == o.TenantID() {
		return ErrAlreadyExists
	}
	r.Store[o.ID()] = o
	r.DrainedEvents = append(r.DrainedEvents, o.PullEvents()...)
	return nil
}

// GetByID satisfies [order.Repository].
func (r *FakeRepository) GetByID(
	_ context.Context, tenantID tenant.ID, id order.ID,
) (*order.Order, error) {
	o, ok := r.Store[id]
	if !ok || o.TenantID() != tenantID {
		return nil, order.ErrNotFound
	}
	return o, nil
}

// UpdateByID satisfies [order.Repository].
func (r *FakeRepository) UpdateByID(
	ctx context.Context, tenantID tenant.ID, id order.ID,
	mutator func(*order.Order) (bool, error),
) error {
	o, err := r.GetByID(ctx, tenantID, id)
	if err != nil {
		return err
	}
	changed, mErr := mutator(o)
	if mErr != nil {
		return mErr
	}
	if !changed {
		return nil
	}
	r.DrainedEvents = append(r.DrainedEvents, o.PullEvents()...)
	return nil
}
