// Package verificationcalltest provides the in-memory FakeRepository
// implementing [verificationcall.Repository]. Used by app-layer
// handler tests + downstream integration scenarios that need a working
// VerificationCall store without a Postgres dependency.
//
// TDL CANON (ThreeDotsLabs Wild Workouts + "Go with the Domain"):
//
//   - The fake lives in a sibling package <aggregate>test/, co-located
//     with the domain aggregate it implements an interface from. Same
//     visibility surface as the aggregate itself; no special test-side
//     plumbing.
//   - The fake is a FAITHFUL implementation of
//     [verificationcall.Repository] — not a mock-with-canned-responses.
//     It honors every contract guarantee: append-only Add (Verification
//     calls are immutable post-insert; no UpdateByID surface), per-
//     contact ListByContact filter ordered newest-first.
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
package verificationcalltest

import (
	"context"
	"sort"

	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
	"github.com/leadkart/leadkart-go/internal/platform/domain/verificationcall"
)

// FakeRepository is the in-memory implementation of
// [verificationcall.Repository]. Zero-value-NOT-usable for the
// DrainedEvents-append flow — construct via [NewFakeRepository].
// Single-test-owner: do NOT share one instance across tests; each test
// creates its own.
type FakeRepository struct {
	// Store is the append-only verification-call slice. Inserted in
	// call-order; ListByContact sorts newest-first on read.
	Store []*verificationcall.VerificationCall

	// DrainedEvents captures every domain event pulled off each call
	// at Add time. Tests assert on the slice for integration-event
	// shape verification.
	DrainedEvents []verificationcall.Event
}

// NewFakeRepository returns an empty in-memory call repository.
// Single-test-owner — each test should construct its own instance;
// do NOT share one fake across parallel tests (no internal sync).
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{}
}

// Compile-time interface conformance gate. Drift in
// [verificationcall.Repository] (a method renamed, signature changed)
// breaks at build time before any test runs.
var _ verificationcall.Repository = (*FakeRepository)(nil)

// Add appends a new call. Drains the aggregate's events via PullEvents
// into [FakeRepository.DrainedEvents] to mirror the adapter's outbox-
// drain on commit.
func (r *FakeRepository) Add(_ context.Context, c *verificationcall.VerificationCall) error {

	r.Store = append(r.Store, c)
	r.DrainedEvents = append(r.DrainedEvents, c.PullEvents()...)
	return nil
}

// ListByContact returns every call for the supplied contactID sorted
// newest-first (logged_at DESC) — same order as the adapter's
// `ORDER BY logged_at DESC` clause.
func (r *FakeRepository) ListByContact(
	_ context.Context, contactID unverifiedcontact.ID,
) ([]*verificationcall.VerificationCall, error) {

	var out []*verificationcall.VerificationCall
	for _, c := range r.Store {
		if c.ContactID() == contactID {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].LoggedAt().After(out[j].LoggedAt())
	})
	return out, nil
}
