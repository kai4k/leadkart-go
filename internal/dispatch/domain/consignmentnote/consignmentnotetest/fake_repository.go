// Package consignmentnotetest provides the in-memory FakeRepository
// implementing [consignmentnote.Repository]. TDL canon.
package consignmentnotetest

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// FakeRepository is an in-memory [consignmentnote.Repository].
type FakeRepository struct {
	Store         map[consignmentnote.ID]*consignmentnote.ConsignmentNote
	ByOrderID     map[consignmentnote.OrderID]consignmentnote.ID
	DrainedEvents []consignmentnote.Event
}

// NewFakeRepository constructs an empty fake.
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{
		Store:     make(map[consignmentnote.ID]*consignmentnote.ConsignmentNote),
		ByOrderID: make(map[consignmentnote.OrderID]consignmentnote.ID),
	}
}

// Compile-time interface conformance.
var _ consignmentnote.Repository = (*FakeRepository)(nil)

// Add satisfies [consignmentnote.Repository]. Enforces the partial-
// unique invariant.
func (r *FakeRepository) Add(_ context.Context, cn *consignmentnote.ConsignmentNote) error {
	if _, ok := r.ByOrderID[cn.OrderID()]; ok {
		return consignmentnote.ErrAlreadyExistsForOrder
	}
	r.Store[cn.ID()] = cn
	r.ByOrderID[cn.OrderID()] = cn.ID()
	r.DrainedEvents = append(r.DrainedEvents, cn.PullEvents()...)
	return nil
}

// GetByID satisfies [consignmentnote.Repository].
func (r *FakeRepository) GetByID(
	_ context.Context, tenantID tenant.ID, id consignmentnote.ID,
) (*consignmentnote.ConsignmentNote, error) {
	cn, ok := r.Store[id]
	if !ok || cn.TenantID() != tenantID {
		return nil, consignmentnote.ErrNotFound
	}
	return cn, nil
}

// GetByOrderID satisfies [consignmentnote.Repository].
func (r *FakeRepository) GetByOrderID(
	_ context.Context, tenantID tenant.ID, orderID consignmentnote.OrderID,
) (*consignmentnote.ConsignmentNote, error) {
	id, ok := r.ByOrderID[orderID]
	if !ok {
		return nil, consignmentnote.ErrNotFound
	}
	cn := r.Store[id]
	if cn.TenantID() != tenantID {
		return nil, consignmentnote.ErrNotFound
	}
	return cn, nil
}

// UpdateByID satisfies [consignmentnote.Repository].
func (r *FakeRepository) UpdateByID(
	ctx context.Context, tenantID tenant.ID, id consignmentnote.ID,
	mutator func(*consignmentnote.ConsignmentNote) (bool, error),
) error {
	cn, err := r.GetByID(ctx, tenantID, id)
	if err != nil {
		return err
	}
	changed, mErr := mutator(cn)
	if mErr != nil {
		return mErr
	}
	if !changed {
		return nil
	}
	r.DrainedEvents = append(r.DrainedEvents, cn.PullEvents()...)
	return nil
}
