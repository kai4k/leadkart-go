// Package leadcredittest provides the in-memory FakeRepository
// implementing [leadcredit.Repository]. Used by app-layer handler
// tests + downstream integration scenarios that need a working
// LeadCredit store without a Postgres dependency.
//
// TDL CANON (ThreeDotsLabs Wild Workouts + "Go with the Domain"):
//
//   - The fake lives in a sibling package <aggregate>test/, co-located
//     with the domain aggregate it implements an interface from. Same
//     visibility surface as the aggregate itself; no special test-side
//     plumbing.
//   - The fake is a FAITHFUL implementation of
//     [leadcredit.Repository] — not a mock-with-canned-responses. It
//     honors the optimistic-concurrency contract: a stored row's
//     version must match the in-aggregate version on UpsertWithVersion,
//     or [leadcredit.ErrConflict] is returned (mirrors the adapter's
//     `WHERE version = $expected` predicate matching 0 rows). The
//     [FakeRepository.ForceConflictOnce] knob lets a single test
//     simulate one ErrConflict + then succeed — drives the retry-loop
//     test.
//   - The fake implements the [TransactionalFake] contract via
//     [FakeRepository.Snapshot] — registered with [FakeUnitOfWork], it
//     supports closure-error rollback. Critical for the PurchaseLead
//     race test where the loser's debit must be undone when the lead
//     UPDATE returns ErrAlreadySold inside the same WithinTx.
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
package leadcredittest

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/platform/domain/leadcredit"
)

// FakeRepository is the in-memory implementation of
// [leadcredit.Repository] with optimistic-version semantics.
// Zero-value-NOT-usable — construct via [NewFakeRepository] so the
// internal maps are initialised. Single-test-owner: do NOT share one
// instance across tests; each test creates its own.
type FakeRepository struct {
	// Store is the per-tenant LeadCredit row index. Holds the latest
	// committed snapshot — reads hydrate a fresh aggregate from this.
	Store map[leadcredit.TenantID]*leadcredit.LeadCredit

	// versions mirrors the stored row's version column. Mutators
	// compare the in-aggregate version against this value to enforce
	// the `WHERE version = $loaded` predicate.
	versions map[leadcredit.TenantID]int64

	// DrainedEvents captures every domain event pulled off the
	// aggregate at UpsertWithVersion-commit time.
	DrainedEvents []leadcredit.Event

	// ForceConflictOnce flips the next UpsertWithVersion to return
	// [leadcredit.ErrConflict] regardless of the version match.
	// Drives the application-layer retry-loop test.
	ForceConflictOnce bool
}

// NewFakeRepository returns an empty in-memory credit repository.
// Single-test-owner — each test should construct its own instance;
// do NOT share one fake across parallel tests (no internal sync).
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{
		Store:    make(map[leadcredit.TenantID]*leadcredit.LeadCredit),
		versions: make(map[leadcredit.TenantID]int64),
	}
}

// Compile-time interface conformance gate. Drift in
// [leadcredit.Repository] (a method renamed, signature changed)
// breaks at build time before any test runs.
var _ leadcredit.Repository = (*FakeRepository)(nil)

// GetByTenant returns a freshly-hydrated snapshot of the stored row
// or [leadcredit.ErrNotFound]. Hydrating fresh prevents callers'
// mutations from leaking across calls — same shape as a real pgx-
// backed read.
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

// UpsertWithVersion either INSERTs a brand-new row (when no stored
// version exists + the in-aggregate Version == 0) or UPDATEs the row
// with the `WHERE version = $loaded` predicate enforced via map
// lookup. Returns [leadcredit.ErrConflict] when:
//
//   - [FakeRepository.ForceConflictOnce] is set (knob consumes itself);
//   - the in-aggregate version doesn't match the stored version;
//   - INSERT path attempted with non-zero starting version.
//
// On success, persists the next-version snapshot + drains aggregate
// events into [FakeRepository.DrainedEvents].
func (r *FakeRepository) UpsertWithVersion(
	_ context.Context, l *leadcredit.LeadCredit,
) error {

	if r.ForceConflictOnce {
		r.ForceConflictOnce = false
		return leadcredit.ErrConflict
	}
	stored, exists := r.versions[l.TenantID()]
	if !exists {
		// INSERT path — aggregate version must be 0.
		if l.Version() != 0 {
			return leadcredit.ErrConflict
		}
	} else if l.Version() != stored {
		return leadcredit.ErrConflict
	}
	// Persist a fresh snapshot with version+1 so subsequent reads see
	// the new state.
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

// Snapshot satisfies the platformtest.TransactionalFake contract.
// Captures the Store + versions maps + DrainedEvents + ForceConflictOnce
// flag at the WithinTx entry point; the returned closure restores all
// four on closure error so the fake models Postgres ROLLBACK semantics.
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
