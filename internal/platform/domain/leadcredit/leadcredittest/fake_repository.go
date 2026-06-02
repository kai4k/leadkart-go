// Package leadcredittest provides the in-memory FakeRepository implementing
// [leadcredit.Repository], for app-layer tests that need a working LeadCredit
// store without Postgres.
//
// TDL canon (Wild Workouts + "Go with the Domain"): the fake is a faithful
// implementation of the interface, not a mock. It honors the optimistic-concurrency
// contract — a stored row's version must match the in-aggregate version on
// UpsertWithVersion or [leadcredit.ErrConflict] is returned (mirrors the adapter's
// WHERE version = $expected matching 0 rows). [FakeRepository.ForceConflictOnce]
// drives the retry-loop test. [FakeRepository.Snapshot] satisfies TransactionalFake
// for closure-error rollback — required by the PurchaseLead race test where the
// loser's debit must unwind when the lead UPDATE returns ErrAlreadySold in the same
// WithinTx.
//
// Single-test-owner: each test constructs its own instance, so no sync is needed and
// t.Parallel is safe. Sync in a domain-co-located package would trip
// TestArch_NoGoroutinesInDomain (domain is concurrency-free — Bryan Mills).
package leadcredittest

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/platform/domain/leadcredit"
)

// FakeRepository is the in-memory [leadcredit.Repository] with optimistic-version
// semantics. Zero value unusable — construct via [NewFakeRepository]. Single-test-owner:
// do not share an instance across tests.
type FakeRepository struct {
	// Store holds the latest committed snapshot per tenant; reads hydrate fresh from it.
	Store map[leadcredit.TenantID]*leadcredit.LeadCredit

	// versions mirrors the stored version column to enforce WHERE version = $loaded.
	versions map[leadcredit.TenantID]int64

	// DrainedEvents captures events pulled off the aggregate at commit time.
	DrainedEvents []leadcredit.Event

	// ForceConflictOnce makes the next UpsertWithVersion return [leadcredit.ErrConflict]
	// regardless of version match; drives the app-layer retry-loop test.
	ForceConflictOnce bool
}

// NewFakeRepository returns an empty in-memory credit repository. Single-test-owner:
// construct one per test; no internal sync.
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{
		Store:    make(map[leadcredit.TenantID]*leadcredit.LeadCredit),
		versions: make(map[leadcredit.TenantID]int64),
	}
}

// Compile-time interface conformance gate.
var _ leadcredit.Repository = (*FakeRepository)(nil)

// GetByTenant returns a freshly-hydrated snapshot of the stored row, or
// [leadcredit.ErrNotFound]. Fresh hydration stops caller mutations leaking across
// calls, matching a real pgx-backed read.
func (r *FakeRepository) GetByTenant(
	_ context.Context, id leadcredit.TenantID,
) (*leadcredit.LeadCredit, error) {

	c, ok := r.Store[id]
	if !ok {
		return nil, leadcredit.ErrNotFound
	}
	snap := leadcredit.Snapshot{
		TenantID:  c.TenantID(),
		Balance:   c.Balance(),
		Version:   c.Version(),
		CreatedAt: c.CreatedAt(),
		UpdatedAt: c.UpdatedAt(),
	}
	return leadcredit.UnmarshalFromDB(snap), nil
}

// UpsertWithVersion INSERTs a new row (no stored version, in-aggregate Version == 0)
// or UPDATEs under the WHERE version = $loaded predicate. Returns
// [leadcredit.ErrConflict] when ForceConflictOnce is set (consumes itself), the
// version mismatches, or an INSERT is attempted with a non-zero version. On success
// persists the next-version snapshot and drains events into DrainedEvents.
func (r *FakeRepository) UpsertWithVersion(
	_ context.Context, l *leadcredit.LeadCredit,
) error {

	if r.ForceConflictOnce {
		r.ForceConflictOnce = false
		return leadcredit.ErrConflict
	}
	stored, exists := r.versions[l.TenantID()]
	if !exists {
		// INSERT path: aggregate version must be 0.
		if l.Version() != 0 {
			return leadcredit.ErrConflict
		}
	} else if l.Version() != stored {
		return leadcredit.ErrConflict
	}
	// Persist version+1 so subsequent reads see the new state.
	snap := leadcredit.Snapshot{
		TenantID:  l.TenantID(),
		Balance:   l.Balance(),
		Version:   l.Version() + 1,
		CreatedAt: l.CreatedAt(),
		UpdatedAt: l.UpdatedAt(),
	}
	r.Store[l.TenantID()] = leadcredit.UnmarshalFromDB(snap)
	r.versions[l.TenantID()] = l.Version() + 1
	r.DrainedEvents = append(r.DrainedEvents, l.PullEvents()...)
	return nil
}

// Snapshot satisfies platformtest.TransactionalFake. It captures Store, versions,
// DrainedEvents, and ForceConflictOnce; the returned closure restores all four on
// closure error, modeling Postgres ROLLBACK.
func (r *FakeRepository) Snapshot() func() {

	store := make(map[leadcredit.TenantID]*leadcredit.LeadCredit, len(r.Store))
	for k, v := range r.Store {
		store[k] = v
	}
	versions := make(map[leadcredit.TenantID]int64, len(r.versions))
	for k, v := range r.versions {
		versions[k] = v
	}
	drained := make([]leadcredit.Event, len(r.DrainedEvents))
	copy(drained, r.DrainedEvents)
	forceConflict := r.ForceConflictOnce
	return func() {
		r.Store = store
		r.versions = versions
		r.DrainedEvents = drained
		r.ForceConflictOnce = forceConflict
	}
}
