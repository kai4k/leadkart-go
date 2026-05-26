// Package calllogtest provides the in-memory FakeRepository implementing
// [calllog.Repository]. Used by app-layer handler tests + downstream
// integration scenarios that need a working CallLog store without a
// Postgres dependency.
//
// TDL CANON (ThreeDotsLabs Wild Workouts + "Go with the Domain"):
//
//   - The fake lives in a sibling package <aggregate>test/, co-located
//     with the domain aggregate it implements an interface from. Same
//     visibility surface as the aggregate itself; no special test-side
//     plumbing.
//   - The fake is a FAITHFUL implementation of [calllog.Repository] —
//     not a mock-with-canned-responses. It honors every contract
//     guarantee: append-only Add (no UpdateByID surface), ErrNotFound
//     on missing GetByID, per-lead ListByLead filter, tenant-scoped
//     reads return ErrNotFound for cross-tenant rows (mirrors the
//     SQL adapter's RLS-bound behavior).
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
package calllogtest

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/crm/domain/calllog"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// FakeRepository is the in-memory implementation of
// [calllog.Repository]. Zero-value-NOT-usable — construct via
// [NewFakeRepository] so the internal map is initialised.
// Single-test-owner: do NOT share one instance across tests; each test
// creates its own.
type FakeRepository struct {
	// ByID is the live call-log index. CallLogs are append-only — no
	// soft-delete surface at slice 1.
	ByID map[calllog.ID]*calllog.CallLog
}

// NewFakeRepository returns an empty in-memory CallLog repository.
// Single-test-owner — each test should construct its own instance;
// do NOT share one fake across parallel tests (no internal sync).
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{ByID: map[calllog.ID]*calllog.CallLog{}}
}

// Compile-time interface conformance gate. Drift in
// [calllog.Repository] (a method renamed, signature changed) breaks
// at build time before any test runs.
var _ calllog.Repository = (*FakeRepository)(nil)

// Add appends a new call-log row. Drains the aggregate's events via
// PullEvents to mirror the adapter's outbox-drain on commit (the fake
// discards them — assertions on the wire shape belong in
// integrationevents arch tests).
func (r *FakeRepository) Add(_ context.Context, c *calllog.CallLog) error {

	r.ByID[c.ID()] = c
	_ = c.PullEvents() // discard for fake
	return nil
}

// GetByID returns the call log from the supplied tenant or
// [calllog.ErrNotFound]. Cross-tenant rows return ErrNotFound to
// mirror the SQL adapter's RLS-bound behavior.
func (r *FakeRepository) GetByID(_ context.Context, tenantID tenant.ID, id calllog.ID) (*calllog.CallLog, error) {

	c, ok := r.ByID[id]
	if !ok {
		return nil, calllog.ErrNotFound
	}
	if c.TenantID() != tenantID {
		return nil, calllog.ErrNotFound
	}
	return c, nil
}

// ListByLead returns every call log for the supplied (tenant, leadID)
// in unspecified order. Slice 1 unit tests don't exercise the newest-
// first ordering; that's covered by adapter integration tests against
// the `logged_at DESC` index. Cross-tenant rows are filtered out to
// mirror the SQL adapter's RLS-bound behavior.
func (r *FakeRepository) ListByLead(_ context.Context, tenantID tenant.ID, leadID crmlead.ID) ([]*calllog.CallLog, error) {

	out := []*calllog.CallLog{}
	for _, c := range r.ByID {
		if c.TenantID() != tenantID {
			continue
		}
		if c.LeadID() == leadID {
			out = append(out, c)
		}
	}
	return out, nil
}
