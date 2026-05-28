// Package quotationtest provides the in-memory FakeRepository
// implementing [quotation.Repository]. Used by app-layer command +
// query handler tests + downstream integration scenarios that need a
// working Quotation store without a Postgres dependency.
//
// TDL canon (ThreeDotsLabs Wild Workouts + "Go with the Domain") —
// per-aggregate fakes live in <aggregate>test/ sibling packages of the
// aggregate they fake. Single-test-owner pattern: each test constructs
// its own FakeRepository via [NewFakeRepository]; no shared mutable
// state across tests; no sync primitives (domain-subtree concurrency-
// free per `TestArch_NoGoroutinesInDomain`).
package quotationtest

import (
	"context"
	"errors"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation"
)

// FakeRepository is the in-memory implementation of
// [quotation.Repository]. Zero-value-NOT-usable — construct via
// [NewFakeRepository] so the internal map is initialised.
type FakeRepository struct {
	// Store keys aggregates by their ID. The composite (tenant_id, id)
	// uniqueness is enforced by Add — duplicate ID returns
	// [ErrAlreadyExists].
	Store map[quotation.ID]*quotation.Quotation

	// DrainedEvents captures every domain event pulled off any
	// aggregate at Add or UpdateByID commit time. Tests assert against
	// this slice to verify the right events fired.
	DrainedEvents []quotation.Event

	// ForceAddError, when non-nil, makes the NEXT Add return this
	// error + then clears itself. Drives "broker retry of duplicate
	// Add" or "DB hiccup" tests.
	ForceAddError error
}

// ErrAlreadyExists is the fake-side analogue of the adapter's
// 23505-on-PK translation. Tests assert this to verify the natural-key
// dedup path.
var ErrAlreadyExists = errors.New("quotationtest: already exists")

// NewFakeRepository returns an empty in-memory quotation repository.
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{
		Store: make(map[quotation.ID]*quotation.Quotation),
	}
}

// Compile-time interface conformance — drift breaks at build.
var _ quotation.Repository = (*FakeRepository)(nil)

// Add satisfies [quotation.Repository]. Drains events. Returns
// [ErrAlreadyExists] on duplicate (tenant_id, id) — mirrors the
// adapter's 23505 translation.
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

// GetByID satisfies [quotation.Repository]. Returns [quotation.ErrNotFound]
// when the (tenant_id, id) pair has no row.
func (r *FakeRepository) GetByID(
	_ context.Context, tenantID tenant.ID, id quotation.ID,
) (*quotation.Quotation, error) {
	q, ok := r.Store[id]
	if !ok {
		return nil, quotation.ErrNotFound
	}
	if q.TenantID() != tenantID {
		// RLS-equivalent: a foreign tenant's read MUST look identical
		// to "no such row" — the adapter binds RLS via GUC; the fake
		// honours the same behaviour explicitly.
		return nil, quotation.ErrNotFound
	}
	return q, nil
}

// UpdateByID satisfies [quotation.Repository]. Mutates in-place when
// the mutator returns (true, nil); drains events on the same path.
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
		// No-op path — domain event slice already empty (mutator
		// returned false BEFORE any state change).
		return nil
	}
	r.DrainedEvents = append(r.DrainedEvents, q.PullEvents()...)
	return nil
}
