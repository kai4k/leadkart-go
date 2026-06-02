// Package quotationtest provides an in-memory [quotation.Repository] fake
// for app-layer handler tests without a Postgres dependency.
// TDL canon: per-aggregate fakes live in <aggregate>test/ siblings; no sync
// primitives (domain-subtree concurrency-free).
package quotationtest

import (
	"context"
	"errors"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation"
)

// FakeRepository is an in-memory [quotation.Repository].
// Not zero-value-safe — use [NewFakeRepository].
type FakeRepository struct {
	// Store holds aggregates keyed by ID; Add enforces (tenant_id, id) uniqueness.
	Store map[quotation.ID]*quotation.Quotation

	// DrainedEvents accumulates events pulled from aggregates at Add/UpdateByID.
	DrainedEvents []quotation.Event

	// ForceAddError, when non-nil, causes the next Add to return it and clears itself.
	ForceAddError error
}

// ErrAlreadyExists mirrors the adapter's SQLSTATE 23505 translation on PK conflict.
var ErrAlreadyExists = errors.New("quotationtest: already exists")

// NewFakeRepository returns an empty in-memory quotation repository.
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{
		Store: make(map[quotation.ID]*quotation.Quotation),
	}
}

// Compile-time interface conformance.
var _ quotation.Repository = (*FakeRepository)(nil)

// Add drains events and returns [ErrAlreadyExists] on duplicate (tenant_id, id).
func (r *FakeRepository) Add(_ context.Context, q *quotation.Quotation) error {
	if r.ForceAddError != nil {
		err := r.ForceAddError
		r.ForceAddError = nil
		return err
	}
	if existing, ok := r.Store[q.ID()]; ok && existing.TenantID() == q.TenantID() {
		return ErrAlreadyExists
	}
	r.Store[q.ID()] = q
	r.DrainedEvents = append(r.DrainedEvents, q.PullEvents()...)
	return nil
}

// GetByID returns [quotation.ErrNotFound] when no row matches (tenant_id, id).
func (r *FakeRepository) GetByID(
	_ context.Context, tenantID tenant.ID, id quotation.ID,
) (*quotation.Quotation, error) {
	q, ok := r.Store[id]
	if !ok {
		return nil, quotation.ErrNotFound
	}
	if q.TenantID() != tenantID {
		// Mirror RLS: cross-tenant read is indistinguishable from not-found.
		return nil, quotation.ErrNotFound
	}
	return q, nil
}

// UpdateByID mutates in-place when the mutator returns (true, nil); drains events on commit.
func (r *FakeRepository) UpdateByID(
	ctx context.Context, tenantID tenant.ID, id quotation.ID,
	mutator func(*quotation.Quotation) (bool, error),
) error {
	q, err := r.GetByID(ctx, tenantID, id)
	if err != nil {
		return err
	}
	changed, mErr := mutator(q)
	if mErr != nil {
		return mErr
	}
	if !changed {
		// Mutator returned false before any state change; event slice is empty.
		return nil
	}
	r.DrainedEvents = append(r.DrainedEvents, q.PullEvents()...)
	return nil
}
