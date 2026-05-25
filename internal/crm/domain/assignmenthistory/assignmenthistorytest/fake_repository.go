// Package assignmenthistorytest provides the in-memory FakeRepository
// implementing [assignmenthistory.Repository]. Used by app-layer
// handler tests + downstream integration scenarios that need a working
// AssignmentHistory store without a Postgres dependency.
//
// TDL CANON (ThreeDotsLabs Wild Workouts + "Go with the Domain"):
//
//   - The fake lives in a sibling package <aggregate>test/, co-located
//     with the domain aggregate it implements an interface from. Same
//     visibility surface as the aggregate itself; no special test-side
//     plumbing.
//   - The fake is a FAITHFUL implementation of
//     [assignmenthistory.Repository] — not a mock-with-canned-responses.
//     It honors every contract guarantee: append-only Add (no
//     UpdateByID surface — assignment history is forensic-trail
//     immutable), ErrNotFound on missing GetByID, per-lead ListByLead
//     filter.
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
package assignmenthistorytest

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/crm/domain/assignmenthistory"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
)

// FakeRepository is the in-memory implementation of
// [assignmenthistory.Repository]. Zero-value-NOT-usable — construct
// via [NewFakeRepository] so the internal map is initialised.
// Single-test-owner: do NOT share one instance across tests; each test
// creates its own.
type FakeRepository struct {
	// ByID is the live entry index. Entries are append-only — no
	// soft-delete surface; the table is forensic-trail immutable.
	ByID map[assignmenthistory.ID]*assignmenthistory.Entry
}

// NewFakeRepository returns an empty in-memory AssignmentHistory
// repository. Single-test-owner — each test should construct its own
// instance; do NOT share one fake across parallel tests (no internal
// sync).
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{ByID: map[assignmenthistory.ID]*assignmenthistory.Entry{}}
}

// Compile-time interface conformance gate. Drift in
// [assignmenthistory.Repository] (a method renamed, signature changed)
// breaks at build time before any test runs.
var _ assignmenthistory.Repository = (*FakeRepository)(nil)

// Add persists a brand-new Entry. Slice 1 emits no integration event
// from this aggregate directly — the parent CrmLead's Assign path
// emits the wire-side `crm.lead_assigned.v1` event, so there are no
// events to drain here.
func (r *FakeRepository) Add(_ context.Context, e *assignmenthistory.Entry) error {

	r.ByID[e.ID()] = e
	return nil
}

// GetByID returns the entry or [assignmenthistory.ErrNotFound].
func (r *FakeRepository) GetByID(_ context.Context, id assignmenthistory.ID) (*assignmenthistory.Entry, error) {

	e, ok := r.ByID[id]
	if !ok {
		return nil, assignmenthistory.ErrNotFound
	}
	return e, nil
}

// ListByLead returns every assignment-history entry for the supplied
// leadID in unspecified order. Slice 1 unit tests don't exercise the
// newest-first ordering; that's covered by adapter integration tests.
func (r *FakeRepository) ListByLead(_ context.Context, leadID crmlead.ID) ([]*assignmenthistory.Entry, error) {

	out := []*assignmenthistory.Entry{}
	for _, e := range r.ByID {
		if e.LeadID() == leadID {
			out = append(out, e)
		}
	}
	return out, nil
}
