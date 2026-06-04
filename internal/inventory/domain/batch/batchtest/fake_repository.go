// Package batchtest provides the in-memory FakeRepository implementing
// [batch.Repository]. Used by app-layer handler tests + downstream
// integration scenarios that need a working Batch store without a
// Postgres dependency.
//
// TDL CANON (ThreeDotsLabs Wild Workouts + "Go with the Domain"):
//
//   - The fake lives in a sibling package <aggregate>test/, co-located
//     with the domain aggregate it implements an interface from. Same
//     visibility surface as the aggregate itself; no special test-side
//     plumbing.
//   - The fake is a FAITHFUL implementation of [batch.Repository] —
//     not a mock-with-canned-responses. It honors every contract
//     guarantee: ErrBatchNumberTaken on duplicate live (product_id,
//     batch_number) per the partial unique index
//     `uq_batches_product_number_live`, ErrNotFound on missing /
//     soft-deleted GetByID, AnyLiveWithStockForProduct as the
//     application-layer guard for product soft-delete.
//   - Optimistic-concurrency drill: [FakeRepository.ConflictsBeforeSuccess]
//     lets a test simulate the optimistic-version UPDATE returning 0
//     rows ([batch.ErrConcurrencyConflict]) the first N commits before
//     allowing the (N+1)-th to succeed. Exercises the application-
//     layer retry loop without spinning up a real adapter.
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
package batchtest

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/product"
)

// FakeRepository is the in-memory implementation of [batch.Repository].
// Zero-value-NOT-usable — construct via [NewFakeRepository] so the
// internal map is initialised. Single-test-owner: do NOT share one
// instance across tests; each test creates its own.
type FakeRepository struct {
	// Batches is the live + soft-deleted batch index by ID. Soft-
	// deleted rows stay in the map so re-create-with-same-ID flows
	// work; GetByID filters them.
	Batches map[batch.ID]*batch.Batch

	// AddErr, when non-nil, is returned by Add INSTEAD of the normal
	// insert. Drives "non-domain repo error" propagation tests.
	AddErr error

	// UpdateErr, when non-nil, is returned by UpdateByID INSTEAD of the
	// normal load-mutate-persist. Drives error-propagation tests.
	UpdateErr error

	// ConflictsBeforeSuccess: when > 0, UpdateByID rejects the first N
	// commits with [batch.ErrConcurrencyConflict] before the (N+1)-th
	// commit succeeds. Drives the optimistic-concurrency retry-loop
	// test without needing goroutines — the real contention test in
	// adapters/ uses goroutines + the pg row-lock + version-check
	// machinery. Mirrors the adapter's `rowsAffected == 0 →
	// ErrConcurrencyConflict` branch.
	ConflictsBeforeSuccess int

	// AnyLiveStockFor + AnyLiveStockOn drive the return of
	// AnyLiveWithStockForProduct. When AnyLiveStockFor matches the
	// supplied productID, the method returns AnyLiveStockOn; otherwise
	// it returns false. Drives the DeleteProductHandler "no live
	// stock" guard tests.
	AnyLiveStockFor product.ID
	AnyLiveStockOn  bool

	// AddCalls counts the number of times Add was entered. Drives
	// "Add MUST NOT be called when X" assertions.
	AddCalls int

	// UpdateCalls counts the number of times UpdateByID was entered.
	// Drives retry-loop assertions.
	UpdateCalls int
}

// NewFakeRepository returns an empty in-memory batch repository.
// Single-test-owner — each test should construct its own instance;
// do NOT share one fake across parallel tests (no internal sync).
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{Batches: make(map[batch.ID]*batch.Batch)}
}

// Compile-time interface conformance gate. Drift in
// [batch.Repository] (a method renamed, signature changed) breaks
// at build time before any test runs.
var _ batch.Repository = (*FakeRepository)(nil)

// Add persists a brand-new Batch. Returns [batch.ErrBatchNumberTaken]
// when a LIVE row with the same (product_id, batch_number) already
// exists — mirrors the partial unique index
// `uq_batches_product_number_live` semantics (soft-deleted homonyms
// do NOT block). Drains aggregate events via PullEvents to match the
// adapter's outbox-drain on commit.
func (r *FakeRepository) Add(_ context.Context, b *batch.Batch) error {

	r.AddCalls++
	if r.AddErr != nil {
		return r.AddErr
	}
	for _, existing := range r.Batches {
		if existing.ProductID() == b.ProductID() && existing.BatchNumber() == b.BatchNumber() && !existing.IsDeleted() {
			return batch.ErrBatchNumberTaken
		}
	}
	_ = b.PullEvents()
	r.Batches[b.ID()] = b
	return nil
}

// UpdateByID loads (scoped to tenantID), mutates via fn, then either
// persists (commit=true) or rolls back (commit=false / err). Returns
// [batch.ErrNotFound] when the row doesn't exist, is soft-deleted, OR
// lives in a different tenant — mirrors the SQL adapter's RLS-bound
// behavior.
//
// Honours [FakeRepository.ConflictsBeforeSuccess] — when > 0, returns
// [batch.ErrConcurrencyConflict] and decrements the counter, mirroring
// the adapter's optimistic-version check failing under contention.
//
// The fake doesn't deep-copy the Batch before passing to fn; the
// caller observes mutations even if it returns (false, nil). This
// mirrors the pg adapter's behavior — both rely on the aggregate's
// invariants being re-checked at persist time, not snapshot-rollback.
func (r *FakeRepository) UpdateByID(_ context.Context, tenantID tenant.ID, id batch.ID, fn func(*batch.Batch) (bool, error)) error {

	r.UpdateCalls++
	if r.UpdateErr != nil {
		return r.UpdateErr
	}
	b, ok := r.Batches[id]
	if !ok {
		return batch.ErrNotFound
	}
	if b.TenantID() != tenantID {
		return batch.ErrNotFound
	}
	commit, err := fn(b)
	if err != nil {
		return err
	}
	if !commit {
		return nil
	}
	if r.ConflictsBeforeSuccess > 0 {
		r.ConflictsBeforeSuccess--
		// Drain emitted-but-not-persisted events to mirror the
		// adapter's behavior — the events generated during the
		// rejected attempt do NOT leak into the next retry's drain.
		_ = b.PullEvents()
		return batch.ErrConcurrencyConflict
	}
	_ = b.PullEvents()
	return nil
}

// GetByID returns the LIVE batch (scoped to tenantID) or
// [batch.ErrNotFound]. Soft-deleted rows + batches in other tenants
// are hidden — mirrors the SQL adapter's RLS-bound behavior.
func (r *FakeRepository) GetByID(_ context.Context, tenantID tenant.ID, id batch.ID) (*batch.Batch, error) {

	b, ok := r.Batches[id]
	if !ok || b.IsDeleted() {
		return nil, batch.ErrNotFound
	}
	if b.TenantID() != tenantID {
		return nil, batch.ErrNotFound
	}
	return b, nil
}

// ListByProductPage is not used by command handlers (the query-layer
// tests exercise the listing path against a real testcontainers DB).
// The stub returns the zero page so the interface stays satisfied
// without a list fake nobody exercises from the command layer.
func (r *FakeRepository) ListByProductPage(_ context.Context, _ tenant.ID, _ product.ID, _ batch.ListFilter, _ pagination.Cursor, _ int) (pagination.Page[*batch.Batch], error) {
	return pagination.Page[*batch.Batch]{Items: []*batch.Batch{}}, nil
}

// AnyLiveWithStockForProduct reports whether any LIVE batch with
// quantity_on_hand > 0 exists for productID in the supplied tenant.
// Honours the AnyLiveStockFor + AnyLiveStockOn knobs — when
// AnyLiveStockFor matches productID, returns AnyLiveStockOn; otherwise
// returns false. Mirrors the application-layer guard used by
// DeleteProductHandler.
func (r *FakeRepository) AnyLiveWithStockForProduct(_ context.Context, _ tenant.ID, productID product.ID) (bool, error) {

	if r.AnyLiveStockFor == productID {
		return r.AnyLiveStockOn, nil
	}
	return false, nil
}

// ListFefoForProduct returns the live, in-stock, not-yet-expired
// batches for productID in tenantID, ordered (expiry_date ASC, id ASC)
// — mirrors the SQL adapter's
// `WHERE NOT is_deleted AND quantity_on_hand > 0 AND expiry_date > now`
// filter + ORDER BY clause.
//
// FEFO ordering canon: the dispatch picker pulls the soonest-to-expire
// batch first. `now` is the wall-clock instant the caller threads in;
// the fake compares strict greater-than (matches the SQL `expiry_date
// > $2`).
func (r *FakeRepository) ListFefoForProduct(_ context.Context, tenantID tenant.ID, productID product.ID, now time.Time) ([]*batch.Batch, error) {
	out := make([]*batch.Batch, 0)
	for _, b := range r.Batches {
		if b.IsDeleted() {
			continue
		}
		if b.TenantID() != tenantID {
			continue
		}
		if b.ProductID() != productID {
			continue
		}
		if b.QuantityOnHand() <= 0 {
			continue
		}
		if !b.ExpiryDate().After(now) {
			continue
		}
		out = append(out, b)
	}
	slices.SortStableFunc(out, func(a, b *batch.Batch) int {
		if !a.ExpiryDate().Equal(b.ExpiryDate()) {
			if a.ExpiryDate().Before(b.ExpiryDate()) {
				return -1
			}
			return 1
		}
		return cmp.Compare(a.ID(), b.ID())
	})
	return out, nil
}
