// Package stockmovementtest provides the in-memory FakeRepository
// implementing [stockmovement.Repository]. Used by app-layer handler
// tests + downstream integration scenarios that need a working
// Movement store without a Postgres dependency.
//
// TDL CANON (ThreeDotsLabs Wild Workouts + "Go with the Domain"):
//
//   - The fake lives in a sibling package <aggregate>test/, co-located
//     with the domain aggregate it implements an interface from. Same
//     visibility surface as the aggregate itself; no special test-side
//     plumbing.
//   - The fake is a FAITHFUL implementation of
//     [stockmovement.Repository] — not a mock-with-canned-responses.
//     It honors every contract guarantee: append-only Add, ErrNotFound
//     on missing GetByID. Movements aren't soft-deleted; rows are
//     append-only by design.
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
package stockmovementtest

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/batch"
	"github.com/leadkart/leadkart-go/internal/inventory/domain/stockmovement"
)

// FakeRepository is the in-memory implementation of
// [stockmovement.Repository]. Zero-value-NOT-usable — construct via
// [NewFakeRepository] so the internal map is initialised.
// Single-test-owner: do NOT share one instance across tests; each test
// creates its own.
type FakeRepository struct {
	// Movements is the append-only ledger keyed by Movement ID.
	// Movements aren't soft-deleted — the ledger is immutable
	// post-insert.
	Movements map[stockmovement.ID]*stockmovement.Movement

	// AddErr, when non-nil, is returned by Add INSTEAD of the normal
	// append. Drives "non-domain repo error" propagation tests.
	AddErr error

	// AddCalls counts the number of times Add was entered. Drives
	// "movement MUST NOT be persisted on reject" assertions.
	AddCalls int
}

// NewFakeRepository returns an empty in-memory movement repository.
// Single-test-owner — each test should construct its own instance;
// do NOT share one fake across parallel tests (no internal sync).
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{Movements: make(map[stockmovement.ID]*stockmovement.Movement)}
}

// Compile-time interface conformance gate. Drift in
// [stockmovement.Repository] (a method renamed, signature changed)
// breaks at build time before any test runs.
var _ stockmovement.Repository = (*FakeRepository)(nil)

// Add appends a new ledger row. Drains the aggregate's events via
// PullEvents to mirror the adapter's outbox-drain on commit.
func (r *FakeRepository) Add(_ context.Context, m *stockmovement.Movement) error {

	r.AddCalls++
	if r.AddErr != nil {
		return r.AddErr
	}
	_ = m.PullEvents()
	r.Movements[m.ID()] = m
	return nil
}

// GetByID returns the movement (scoped to tenantID) or
// [stockmovement.ErrNotFound]. Movements in other tenants are hidden —
// mirrors the SQL adapter's RLS-bound behavior.
func (r *FakeRepository) GetByID(_ context.Context, tenantID tenant.ID, id stockmovement.ID) (*stockmovement.Movement, error) {

	m, ok := r.Movements[id]
	if !ok {
		return nil, stockmovement.ErrNotFound
	}
	if m.TenantID() != tenantID {
		return nil, stockmovement.ErrNotFound
	}
	return m, nil
}

// ListByBatchPage is not used by command handlers (the query-layer
// tests exercise the listing path against a real testcontainers DB).
// The stub returns the zero page so the interface stays satisfied
// without a list fake nobody exercises from the command layer.
func (r *FakeRepository) ListByBatchPage(_ context.Context, _ tenant.ID, _ batch.ID, _ stockmovement.PageRequest) (pagination.Page[*stockmovement.Movement], error) {
	return pagination.Page[*stockmovement.Movement]{Items: []*stockmovement.Movement{}}, nil
}
