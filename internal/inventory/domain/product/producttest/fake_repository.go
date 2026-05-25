// Package producttest provides the in-memory FakeRepository implementing
// [product.Repository]. Used by app-layer handler tests + downstream
// integration scenarios that need a working Product store without a
// Postgres dependency.
//
// TDL CANON (ThreeDotsLabs Wild Workouts + "Go with the Domain"):
//
//   - The fake lives in a sibling package <aggregate>test/, co-located
//     with the domain aggregate it implements an interface from. Same
//     visibility surface as the aggregate itself; no special test-side
//     plumbing.
//   - The fake is a FAITHFUL implementation of [product.Repository] —
//     not a mock-with-canned-responses. It honors every contract
//     guarantee: ErrSKUTaken on duplicate live (tenant_id, sku) per the
//     partial unique index `uq_products_tenant_sku_live`, ErrNotFound
//     on missing / soft-deleted GetByID, and the soft-delete filter on
//     reads.
//   - Single-test-owner pattern: each test creates its OWN
//     FakeRepository via [NewFakeRepository] — no shared mutable state
//     across tests. t.Parallel is naturally safe because no two tests
//     share the same fake instance. This is TDL canon: fakes don't
//     need sync primitives because they're per-test, and putting sync
//     in domain-co-located test packages would trip
//     TestArch_NoGoroutinesInDomain (domain layer is concurrency-free
//     by design — Bryan Mills "Rethinking Concurrency Patterns").
//
// Why fakes, not mocks: per TDL "Go with the Domain" ch. 8, mocks
// couple the test to the call-pattern of the SUT (Subject Under Test);
// fakes couple to the CONTRACT. Refactoring the SUT to use the
// interface differently breaks mock-tests but leaves fake-tests
// green. The contract is the load-bearing thing.
package producttest

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// FakeRepository is the in-memory implementation of [product.Repository].
// Zero-value-NOT-usable — construct via [NewFakeRepository] so the
// internal map is initialised. Single-test-owner: do NOT share one
// instance across tests; each test creates its own.
type FakeRepository struct {
	// Products is the live + soft-deleted Product index by ID. Soft-
	// deleted rows stay in the map so re-create-with-same-ID flows
	// work; reads (GetByID / ListPage) filter them.
	Products map[product.ID]*product.Product

	// GetErr, when non-nil, is returned by GetByID INSTEAD of the
	// normal lookup. Drives "repo unavailable" error-propagation tests.
	GetErr error

	// AddErr, when non-nil, is returned by Add INSTEAD of the normal
	// insert. Drives "non-domain repo error" propagation tests.
	AddErr error

	// UpdateErr, when non-nil, is returned by UpdateByID INSTEAD of the
	// normal load-mutate-persist. Drives error-propagation tests.
	UpdateErr error

	// AddCalls counts the number of times Add was entered (whether or
	// not it ultimately returned an error). Drives "Add MUST NOT be
	// called when X" assertions.
	AddCalls int

	// UpdateCalls counts the number of times UpdateByID was entered.
	UpdateCalls int
}

// NewFakeRepository returns an empty in-memory product repository.
// Single-test-owner — each test should construct its own instance;
// do NOT share one fake across parallel tests (no internal sync).
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{Products: make(map[product.ID]*product.Product)}
}

// Compile-time interface conformance gate. Drift in
// [product.Repository] (a method renamed, signature changed) breaks
// at build time before any test runs.
var _ product.Repository = (*FakeRepository)(nil)

// Add persists a brand-new Product. Returns [product.ErrSKUTaken] when
// a LIVE row with the same (tenant_id, sku) already exists — mirrors
// the partial unique index `uq_products_tenant_sku_live` semantics
// (soft-deleted homonyms do NOT block). Drains the aggregate's events
// via PullEvents to match the adapter's outbox-drain on commit.
func (r *FakeRepository) Add(_ context.Context, p *product.Product) error {

	r.AddCalls++
	if r.AddErr != nil {
		return r.AddErr
	}
	for _, existing := range r.Products {
		if existing.TenantID() == p.TenantID() && existing.SKU() == p.SKU() && !existing.IsDeleted() {
			return product.ErrSKUTaken
		}
	}
	_ = p.PullEvents()
	r.Products[p.ID()] = p
	return nil
}

// UpdateByID loads, mutates via updateFn, then either persists
// (commit=true) or rolls back (commit=false / err). Returns
// [product.ErrNotFound] when the row doesn't exist.
//
// The fake doesn't deep-copy the Product before passing to updateFn;
// the caller observes mutations even if it returns (false, nil). This
// mirrors the pg adapter's behavior — both rely on the aggregate's
// invariants being re-checked at persist time, not snapshot-rollback.
func (r *FakeRepository) UpdateByID(_ context.Context, id product.ID, updateFn func(*product.Product) (bool, error)) error {

	r.UpdateCalls++
	if r.UpdateErr != nil {
		return r.UpdateErr
	}
	p, ok := r.Products[id]
	if !ok {
		return product.ErrNotFound
	}
	commit, err := updateFn(p)
	if err != nil {
		return err
	}
	if commit {
		_ = p.PullEvents()
	}
	return nil
}

// GetByID returns the LIVE product or [product.ErrNotFound]. Soft-
// deleted rows are hidden. Honours the GetErr knob for error-
// propagation drills.
func (r *FakeRepository) GetByID(_ context.Context, id product.ID) (*product.Product, error) {

	if r.GetErr != nil {
		return nil, r.GetErr
	}
	p, ok := r.Products[id]
	if !ok || p.IsDeleted() {
		return nil, product.ErrNotFound
	}
	return p, nil
}

// ListPage is not used by command handlers (the query-layer tests
// exercise the listing path against a real testcontainers DB). The
// stub returns the zero page so the interface stays satisfied without
// a list fake nobody exercises from the command layer.
func (r *FakeRepository) ListPage(_ context.Context, _ tenant.ID, _ product.ListFilter, _ pagination.Cursor, _ int) (pagination.Page[*product.Product], error) {
	return pagination.Page[*product.Product]{Items: []*product.Product{}}, nil
}
