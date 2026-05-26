// Package platformleadtest provides the in-memory FakeRepository
// implementing [platformlead.Repository]. Used by app-layer handler
// tests + downstream integration scenarios that need a working
// PlatformLead store without a Postgres dependency.
//
// TDL CANON (ThreeDotsLabs Wild Workouts + "Go with the Domain"):
//
//   - The fake lives in a sibling package <aggregate>test/, co-located
//     with the domain aggregate it implements an interface from. Same
//     visibility surface as the aggregate itself; no special test-side
//     plumbing.
//   - The fake is a FAITHFUL implementation of
//     [platformlead.Repository] — not a mock-with-canned-responses.
//     It honors every contract guarantee: ErrNotFound on missing IDs,
//     unsold-only filter on MarketplaceBrowse (mirrors the adapter's
//     `WHERE sold_to_tenant_id IS NULL` predicate), per-aggregate
//     event drain on each commit.
//   - The fake implements the [TransactionalFake] contract via
//     [FakeRepository.Snapshot] — registered with [FakeUnitOfWork], it
//     supports closure-error rollback so tests verify "loser's
//     mutation is undone when the winning UPDATE rejects in the same
//     UoW" behaviour (Postgres ROLLBACK semantics under the
//     PurchaseLead race).
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
package platformleadtest

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/platform/domain/platformlead"
)

// FakeRepository is the in-memory implementation of
// [platformlead.Repository]. Zero-value-NOT-usable — construct via
// [NewFakeRepository] so the internal map is initialised.
// Single-test-owner: do NOT share one instance across tests; each test
// creates its own.
type FakeRepository struct {
	// Store is the live lead index by ID. PlatformLeads transition
	// from Available → Sold via [platformlead.PlatformLead.Purchase];
	// not row-level soft-deleted.
	Store map[platformlead.ID]*platformlead.PlatformLead

	// DrainedEvents captures every domain event pulled off the
	// aggregate at Add + committed-UpdateByID time.
	DrainedEvents []platformlead.Event
}

// NewFakeRepository returns an empty in-memory lead repository.
// Single-test-owner — each test should construct its own instance;
// do NOT share one fake across parallel tests (no internal sync).
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{
		Store: make(map[platformlead.ID]*platformlead.PlatformLead),
	}
}

// Compile-time interface conformance gate. Drift in
// [platformlead.Repository] (a method renamed, signature changed)
// breaks at build time before any test runs.
var _ platformlead.Repository = (*FakeRepository)(nil)

// Add persists a brand-new lead. Drains the aggregate's events via
// PullEvents into [FakeRepository.DrainedEvents].
func (r *FakeRepository) Add(_ context.Context, l *platformlead.PlatformLead) error {

	r.Store[l.ID()] = l
	r.DrainedEvents = append(r.DrainedEvents, l.PullEvents()...)
	return nil
}

// UpdateByID loads, mutates via updateFn, persists. Returns
// [platformlead.ErrNotFound] when the row doesn't exist.
//
// On commit, drains the aggregate's events into
// [FakeRepository.DrainedEvents]. The fake doesn't deep-copy the
// aggregate before passing to updateFn; the caller observes mutations
// even if it returns (false, nil). This mirrors the pg adapter's
// behavior — both rely on the aggregate's invariants being re-checked
// at persist time, not snapshot-rollback.
func (r *FakeRepository) UpdateByID(
	_ context.Context,
	id platformlead.ID,
	updateFn func(*platformlead.PlatformLead) (bool, error),
) error {

	l, ok := r.Store[id]
	if !ok {
		return platformlead.ErrNotFound
	}
	shouldPersist, err := updateFn(l)
	if err != nil {
		return err
	}
	if !shouldPersist {
		return nil
	}
	r.DrainedEvents = append(r.DrainedEvents, l.PullEvents()...)
	return nil
}

// GetByID returns the lead or [platformlead.ErrNotFound].
func (r *FakeRepository) GetByID(
	_ context.Context, id platformlead.ID,
) (*platformlead.PlatformLead, error) {

	l, ok := r.Store[id]
	if !ok {
		return nil, platformlead.ErrNotFound
	}
	return l, nil
}

// MarketplaceBrowse returns ALL unsold leads in insertion order up to
// pageSize. Slice 1 tests don't exercise the filter set; the
// integration test suite does.
func (r *FakeRepository) MarketplaceBrowse(
	_ context.Context,
	_ platformlead.MarketplaceFilter,
	_ pagination.Cursor,
	pageSize int,
) ([]*platformlead.PlatformLead, error) {

	var out []*platformlead.PlatformLead
	for _, l := range r.Store {
		if l.IsAvailable() {
			out = append(out, l)
		}
		if len(out) >= pageSize {
			break
		}
	}
	return out, nil
}

// Snapshot satisfies the platformtest.TransactionalFake contract.
// Captures the Store map + DrainedEvents at the WithinTx entry point;
// the returned closure restores both on closure error so the fake
// models Postgres ROLLBACK semantics. Deep-copies aggregate pointers
// via UnmarshalFromDB so closures that mutated the aggregate in-place
// (e.g. Purchase) don't leak past the rollback.
func (r *FakeRepository) Snapshot() func() {

	store := make(map[platformlead.ID]*platformlead.PlatformLead, len(r.Store))
	for k, v := range r.Store {
		store[k] = platformlead.UnmarshalFromDB(platformlead.Snapshot{
			ID:                     v.ID(),
			SourceContactID:        v.SourceContactID(),
			Form:                   v.Form(),
			GstVerified:            v.GstVerified(),
			SoldToTenantID:         v.SoldToTenantID(),
			SoldAt:                 v.SoldAt(),
			SoldToMembershipID:     v.SoldToMembershipID(),
			AmountPaisa:            v.AmountPaisa(),
			VerifiedAt:             v.VerifiedAt(),
			VerifiedByMembershipID: v.VerifiedByMembershipID(),
			CreatedAt:              v.CreatedAt(),
		})
	}
	drained := make([]platformlead.Event, len(r.DrainedEvents))
	copy(drained, r.DrainedEvents)
	return func() {
		r.Store = store
		r.DrainedEvents = drained
	}
}
