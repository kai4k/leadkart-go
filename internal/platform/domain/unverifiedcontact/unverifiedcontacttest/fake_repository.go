// Package unverifiedcontacttest provides the in-memory FakeRepository
// implementing [unverifiedcontact.Repository]. Used by app-layer
// handler tests + downstream integration scenarios that need a working
// UnverifiedContact store without a Postgres dependency.
//
// TDL CANON (ThreeDotsLabs Wild Workouts + "Go with the Domain"):
//
//   - The fake lives in a sibling package <aggregate>test/, co-located
//     with the domain aggregate it implements an interface from. Same
//     visibility surface as the aggregate itself; no special test-side
//     plumbing.
//   - The fake is a FAITHFUL implementation of
//     [unverifiedcontact.Repository] — not a mock-with-canned-responses.
//     It honors every contract guarantee: ErrNotFound on missing IDs,
//     append-only Add (state-machine transitions happen via
//     UpdateByID's mutator closure), per-aggregate event drain on
//     each commit.
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
package unverifiedcontacttest

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
)

// FakeRepository is the in-memory implementation of
// [unverifiedcontact.Repository]. Also captures pulled domain events
// on Add / committed UpdateByID into [FakeRepository.DrainedEvents] so
// tests can assert they were drained.
//
// Zero-value-NOT-usable — construct via [NewFakeRepository] so the
// internal map is initialised. Single-test-owner: do NOT share one
// instance across tests; each test creates its own.
type FakeRepository struct {
	// Store is the live contact index by ID. Contacts are not row-
	// level soft-deleted; the aggregate carries a State enum (New /
	// InCall / Busy / Verified / Rejected).
	Store map[unverifiedcontact.ID]*unverifiedcontact.UnverifiedContact

	// DrainedEvents captures every domain event pulled off the
	// aggregate at Add + committed-UpdateByID time. Tests assert on
	// the slice's length + per-element type to verify the integration-
	// event surface without a real outbox.
	DrainedEvents []unverifiedcontact.Event
}

// NewFakeRepository returns an empty in-memory contact repository.
// Single-test-owner — each test should construct its own instance;
// do NOT share one fake across parallel tests (no internal sync).
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{
		Store: make(map[unverifiedcontact.ID]*unverifiedcontact.UnverifiedContact),
	}
}

// Compile-time interface conformance gate. Drift in
// [unverifiedcontact.Repository] (a method renamed, signature changed)
// breaks at build time before any test runs.
var _ unverifiedcontact.Repository = (*FakeRepository)(nil)

// Add persists a brand-new contact created via [unverifiedcontact.New].
// Drains the aggregate's events via PullEvents into
// [FakeRepository.DrainedEvents] — the integration-event mapping is
// covered by the integrationevents arch tests at adapter time.
func (r *FakeRepository) Add(_ context.Context, c *unverifiedcontact.UnverifiedContact) error {

	r.Store[c.ID()] = c
	r.DrainedEvents = append(r.DrainedEvents, c.PullEvents()...)
	return nil
}

// UpdateByID loads, mutates via updateFn, then either persists
// (commit=true) or rolls back (commit=false / err). Returns
// [unverifiedcontact.ErrNotFound] when the row doesn't exist.
//
// On commit, drains the aggregate's events into
// [FakeRepository.DrainedEvents]. The fake doesn't deep-copy the
// aggregate before passing to updateFn; the caller observes mutations
// even if it returns (false, nil). This mirrors the pg adapter's
// behavior — both rely on the aggregate's invariants being re-checked
// at persist time, not snapshot-rollback.
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

// GetByID returns the contact or [unverifiedcontact.ErrNotFound].
func (r *FakeRepository) GetByID(
	_ context.Context, id unverifiedcontact.ID,
) (*unverifiedcontact.UnverifiedContact, error) {

	c, ok := r.Store[id]
	if !ok {
		return nil, unverifiedcontact.ErrNotFound
	}
	return c, nil
}
