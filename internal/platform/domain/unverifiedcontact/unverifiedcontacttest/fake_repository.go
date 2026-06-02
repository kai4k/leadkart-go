// Package unverifiedcontacttest provides the in-memory FakeRepository
// implementing [unverifiedcontact.Repository] for handler and integration
// tests that need a working store without Postgres.
//
// TDL canon (Wild Workouts, "Go with the Domain"): a faithful fake, not a
// mock. It honors the full contract (ErrNotFound, event drain on commit) so
// tests couple to the contract, not the SUT's call pattern. No sync primitives:
// single-test-owner means each test makes its own instance, keeping the
// domain subtree concurrency-free (TestArch_NoGoroutinesInDomain).
package unverifiedcontacttest

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
)

// FakeRepository is the in-memory [unverifiedcontact.Repository]. It records
// events drained on Add and committed UpdateByID so tests can assert on them.
//
// Not usable zero-valued: construct via [NewFakeRepository]. Single-test-owner;
// do not share across tests.
type FakeRepository struct {
	// Store indexes contacts by ID. No soft-delete; the aggregate carries a
	// State enum (New / InCall / Busy / Verified / Rejected).
	Store map[unverifiedcontact.ID]*unverifiedcontact.UnverifiedContact

	// DrainedEvents holds every event pulled off the aggregate at Add and
	// committed-UpdateByID time, standing in for the outbox.
	DrainedEvents []unverifiedcontact.Event
}

// NewFakeRepository returns an empty repository. Single-test-owner: do not
// share one fake across parallel tests (no internal sync).
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{
		Store: make(map[unverifiedcontact.ID]*unverifiedcontact.UnverifiedContact),
	}
}

// Compile-time conformance gate: interface drift breaks the build.
var _ unverifiedcontact.Repository = (*FakeRepository)(nil)

// Add persists a new contact and drains its events into DrainedEvents.
func (r *FakeRepository) Add(_ context.Context, c *unverifiedcontact.UnverifiedContact) error {

	r.Store[c.ID()] = c
	r.DrainedEvents = append(r.DrainedEvents, c.PullEvents()...)
	return nil
}

// UpdateByID loads, runs updateFn, then persists (commit=true) or skips.
// Returns [unverifiedcontact.ErrNotFound] when the row is absent; drains
// events on commit. No snapshot-rollback: the aggregate isn't deep-copied, so
// the caller sees mutations even on (false, nil), matching the pg adapter,
// which re-checks invariants at persist time.
func (r *FakeRepository) UpdateByID(
	_ context.Context,
	id unverifiedcontact.ID,
	updateFn func(*unverifiedcontact.UnverifiedContact) (bool, error),
) error {

	c, ok := r.Store[id]
	if !ok {
		return unverifiedcontact.ErrNotFound
	}
	shouldPersist, err := updateFn(c)
	if err != nil {
		return err
	}
	if !shouldPersist {
		return nil
	}
	r.DrainedEvents = append(r.DrainedEvents, c.PullEvents()...)
	return nil
}

// GetByID returns the contact, or [unverifiedcontact.ErrNotFound] if absent.
func (r *FakeRepository) GetByID(
	_ context.Context, id unverifiedcontact.ID,
) (*unverifiedcontact.UnverifiedContact, error) {

	c, ok := r.Store[id]
	if !ok {
		return nil, unverifiedcontact.ErrNotFound
	}
	return c, nil
}

// Snapshot satisfies the platformtest.TransactionalFake contract so this fake
// can be registered with the FakeUnitOfWork. It captures Store + DrainedEvents;
// the returned closure restores them on closure error, modelling Postgres
// ROLLBACK. Aggregates are deep-copied via UnmarshalFromDB so an in-place
// mutation (e.g. MarkVerified) doesn't leak past rollback — without this, the
// MarkVerified handler's UoW (which co-writes this contact and a PlatformLead)
// would leave the contact verified even when the surrounding tx aborts.
func (r *FakeRepository) Snapshot() func() {

	store := make(map[unverifiedcontact.ID]*unverifiedcontact.UnverifiedContact, len(r.Store))
	for k, v := range r.Store {
		store[k] = unverifiedcontact.UnmarshalFromDB(unverifiedcontact.Snapshot{
			ID:                     v.ID(),
			Form:                   v.Form(),
			State:                  v.State(),
			RejectionReason:        v.RejectionReason(),
			BusyCallbackAt:         v.BusyCallbackAt(),
			BusyCallbackEndAt:      v.BusyCallbackEndAt(),
			PlatformLeadID:         v.PlatformLeadID(),
			CreatedAt:              v.CreatedAt(),
			CreatedByMembershipID:  v.CreatedByMembershipID(),
			VerifiedAt:             v.VerifiedAt(),
			VerifiedByMembershipID: v.VerifiedByMembershipID(),
			RejectedAt:             v.RejectedAt(),
			RejectedByMembershipID: v.RejectedByMembershipID(),
		})
	}
	drained := make([]unverifiedcontact.Event, len(r.DrainedEvents))
	copy(drained, r.DrainedEvents)
	return func() {
		r.Store = store
		r.DrainedEvents = drained
	}
}
