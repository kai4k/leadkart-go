// Package platformleadtest provides the in-memory FakeRepository implementing
// [platformlead.Repository], for app-layer handler tests and integration
// scenarios that need a PlatformLead store without Postgres.
//
// TDL canon (Wild Workouts + "Go with the Domain"):
//   - Co-located in a sibling <aggregate>test/ package.
//   - Faithful contract implementation, not a canned-response mock: ErrNotFound
//     on missing IDs, unsold-only MarketplaceBrowse (mirrors the adapter's
//     WHERE sold_to_tenant_id IS NULL), event drain on each commit.
//   - Implements the TransactionalFake contract via [FakeRepository.Snapshot]:
//     registered with FakeUnitOfWork, it rolls back on closure error to model
//     Postgres ROLLBACK under the PurchaseLead race.
//   - Single-test-owner: each test builds its own fake, so t.Parallel is safe
//     without sync. Sync here would also trip TestArch_NoGoroutinesInDomain
//     (domain is concurrency-free — Bryan Mills, "Rethinking Concurrency").
//
// Fakes over mocks ("Go with the Domain" ch. 8): mocks couple tests to the
// SUT's call pattern, fakes couple to the contract — refactors leave fake tests
// green.
package platformleadtest

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/platform/domain/platformlead"
)

// FakeRepository is the in-memory [platformlead.Repository]. Construct via
// [NewFakeRepository] (zero value unusable). Single-test-owner: do not share
// across tests.
type FakeRepository struct {
	// Store indexes live leads by ID. Leads go Available → Sold via Purchase;
	// not soft-deleted.
	Store map[platformlead.ID]*platformlead.PlatformLead

	// DrainedEvents collects events pulled at Add and committed UpdateByID.
	DrainedEvents []platformlead.Event

	// FailAddOnce, when non-nil, makes the next Add return it (and clears
	// itself) instead of persisting. Drives rollback regression tests that
	// need the co-written lead Add to fail inside a UoW closure — mirrors a
	// Postgres INSERT error (e.g. 23505) aborting the tx.
	FailAddOnce error
}

// NewFakeRepository returns an empty in-memory lead repository. Single-test-owner:
// each test builds its own; no internal sync.
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{
		Store: make(map[platformlead.ID]*platformlead.PlatformLead),
	}
}

// Contract gate: interface drift breaks the build before any test runs.
var _ platformlead.Repository = (*FakeRepository)(nil)

// Add persists a new lead and drains its events into DrainedEvents.
func (r *FakeRepository) Add(_ context.Context, l *platformlead.PlatformLead) error {

	if r.FailAddOnce != nil {
		err := r.FailAddOnce
		r.FailAddOnce = nil
		return err
	}
	r.Store[l.ID()] = l
	r.DrainedEvents = append(r.DrainedEvents, l.PullEvents()...)
	return nil
}

// UpdateByID loads, applies updateFn, and on persist drains events. Returns
// [platformlead.ErrNotFound] when the row is absent.
//
// No deep-copy before updateFn: the caller observes mutations even on
// (false, nil), matching the pg adapter — both re-check invariants at persist
// time rather than snapshot-rollback.
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

// MarketplaceBrowse returns unsold leads up to pageSize. Filters are exercised
// by the integration suite, not here.
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

// Snapshot satisfies the platformtest.TransactionalFake contract. It captures
// Store and DrainedEvents; the returned closure restores them on error,
// modelling Postgres ROLLBACK. Aggregates are deep-copied via UnmarshalFromDB
// so in-place mutations (e.g. Purchase) don't leak past rollback.
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
