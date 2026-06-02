// Package platformleadtest provides the in-memory FakeRepository implementing
// [platformlead.Repository], for app-layer handler tests and integration
// scenarios that need a PlatformLead store without Postgres.
//
// TDL canon (Wild Workouts + "Go with the Domain"):
//   - Co-located in a sibling <aggregate>test/ package.
//   - Faithful contract implementation, not a canned-response mock: ErrNotFound
//     on missing IDs, availability-filtered MarketplaceBrowse (purchase count
//     below the effective sale limit, per ADR 0065), event drain on each
//     commit, and pending-purchase drain into Purchases (mirrors the adapter
//     inserting lead_purchases rows).
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

// defaultTierLimits mirrors the platform.lead_tiers seed so MarketplaceBrowse
// availability matches the adapter without a tier-config dependency.
func defaultTierLimits() map[platformlead.Tier]int {
	return map[platformlead.Tier]int{
		platformlead.TierStandard: 6,
		platformlead.TierPriority: 4,
		platformlead.TierPremium:  2,
	}
}

// FakeRepository is the in-memory [platformlead.Repository]. Construct via
// [NewFakeRepository] (zero value unusable). Single-test-owner: do not share
// across tests.
type FakeRepository struct {
	// Store indexes live leads by ID. A lead carries its buyer set in-memory;
	// RecordPurchase appends to it, so subsequent loads see prior buyers.
	Store map[platformlead.ID]*platformlead.PlatformLead

	// Purchases collects the LeadPurchase rows drained on each committed
	// RecordPurchase — the in-memory stand-in for platform.lead_purchases.
	Purchases []*platformlead.LeadPurchase

	// DrainedEvents collects events pulled at Add and committed UpdateByID.
	DrainedEvents []platformlead.Event

	// TierLimits is the per-tier default sale limit used by MarketplaceBrowse.
	TierLimits map[platformlead.Tier]int

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
		Store:      make(map[platformlead.ID]*platformlead.PlatformLead),
		TierLimits: defaultTierLimits(),
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

// UpdateByID loads, applies updateFn, and on persist drains events + pending
// purchases. Returns [platformlead.ErrNotFound] when the row is absent.
//
// No deep-copy before updateFn: the caller observes mutations even on
// (false, nil), matching the pg adapter — both re-check invariants at persist
// time rather than snapshot-rollback (the FakeUnitOfWork handles tx rollback
// via Snapshot).
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
	r.Purchases = append(r.Purchases, l.PullPendingPurchases()...)
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

// MarketplaceBrowse returns still-available leads (purchase count below the
// effective sale limit) up to pageSize. Filters are exercised by the
// integration suite, not here.
func (r *FakeRepository) MarketplaceBrowse(
	_ context.Context,
	_ platformlead.MarketplaceFilter,
	_ pagination.Cursor,
	pageSize int,
) ([]*platformlead.PlatformLead, error) {

	var out []*platformlead.PlatformLead
	for _, l := range r.Store {
		if l.IsAvailable(r.TierLimits[l.Tier()]) {
			out = append(out, l)
		}
		if len(out) >= pageSize {
			break
		}
	}
	return out, nil
}

// Snapshot satisfies the platformtest.TransactionalFake contract. It captures
// Store (deep-copied, buyer set included), Purchases, and DrainedEvents; the
// returned closure restores them on error, modelling Postgres ROLLBACK so an
// in-place RecordPurchase doesn't leak past a rolled-back tx.
func (r *FakeRepository) Snapshot() func() {

	store := make(map[platformlead.ID]*platformlead.PlatformLead, len(r.Store))
	for k, v := range r.Store {
		store[k] = platformlead.UnmarshalFromDB(platformlead.Snapshot{
			ID:                     v.ID(),
			SourceContactID:        v.SourceContactID(),
			Form:                   v.Form(),
			GstVerified:            v.GstVerified(),
			Tier:                   v.Tier(),
			SaleLimit:              v.SaleLimit(),
			BuyerTenantIDs:         v.BuyerTenantIDs(),
			VerifiedAt:             v.VerifiedAt(),
			VerifiedByMembershipID: v.VerifiedByMembershipID(),
			CreatedAt:              v.CreatedAt(),
		})
	}
	purchases := make([]*platformlead.LeadPurchase, len(r.Purchases))
	copy(purchases, r.Purchases)
	drained := make([]platformlead.Event, len(r.DrainedEvents))
	copy(drained, r.DrainedEvents)
	return func() {
		r.Store = store
		r.Purchases = purchases
		r.DrainedEvents = drained
	}
}
