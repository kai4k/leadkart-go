// Package rolehierarchytest provides the in-memory FakeRepository
// implementing [rolehierarchy.Repository]. Used by app-layer handler
// tests + downstream integration scenarios that need a working edge
// store without a Postgres dependency.
//
// TDL CANON (ThreeDotsLabs Wild Workouts + "Go with the Domain"):
//
//   - The fake lives in a sibling package <aggregate>test/, co-located
//     with the domain aggregate it implements an interface from. Same
//     visibility surface as the aggregate itself; no special test-side
//     plumbing.
//   - The fake is a FAITHFUL implementation of
//     [rolehierarchy.Repository] — not a mock-with-canned-responses. It
//     honors every contract guarantee: ErrEdgeNotFound on missing
//     IDs / unmatched child lookups, ErrEdgeAlreadyExists on a second
//     active edge for the same child (mirrors uq_role_hierarchy_active
//     _edge_per_child), multi-hop ErrCycle detection (mirrors the
//     edge_check_cycle DB trigger).
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
package rolehierarchytest

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/rolehierarchy"
)

// FakeRepository is the in-memory implementation of
// [rolehierarchy.Repository]. Zero-value-NOT-usable — construct via
// [NewFakeRepository] so the internal map is initialised. Single-test-
// owner: do NOT share one instance across tests; each test creates
// its own.
type FakeRepository struct {
	// edges is the active + removed edge index by ID. The aggregate
	// carries an IsActive flag (removedAt zero == active); reads via
	// GetActiveByChild + ListActiveByParent filter on it.
	edges map[rolehierarchy.ID]*rolehierarchy.Edge
}

// NewFakeRepository returns an empty in-memory edge repository.
// Single-test-owner — each test should construct its own instance;
// do NOT share one fake across parallel tests (no internal sync).
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{edges: make(map[rolehierarchy.ID]*rolehierarchy.Edge)}
}

// Compile-time interface conformance gate. Drift in
// [rolehierarchy.Repository] (a method renamed, signature changed)
// breaks at build time before any test runs.
var _ rolehierarchy.Repository = (*FakeRepository)(nil)

// Add persists a brand-new active Edge. Mirrors the SQL adapter's
// invariant translation:
//
//   - [rolehierarchy.ErrEdgeAlreadyExists] when another active edge
//     already binds this child (uq_role_hierarchy_active_edge_per_child).
//   - [rolehierarchy.ErrCycle] when the edge would close a multi-hop
//     loop in the active-edge graph (edge_check_cycle trigger).
//
// Cross-tenant + self-reference checks are enforced upstream by
// [rolehierarchy.New]; we do NOT re-validate them here, mirroring the
// adapter (where chk_edge_no_self_loop and the composite FK fire at the
// DB level).
func (f *FakeRepository) Add(_ context.Context, e *rolehierarchy.Edge) error {

	// Single-parent invariant — refuse a second active edge for the
	// same child.
	for _, existing := range f.edges {
		if existing.IsActive() && existing.ChildRoleID() == e.ChildRoleID() {
			return rolehierarchy.ErrEdgeAlreadyExists
		}
	}
	// Multi-hop cycle detection — walking the proposed parent upward
	// through existing active edges, would we ever land back on the
	// child?
	if hasCycle(f.edges, e.ChildRoleID(), e.ParentRoleID()) {
		return rolehierarchy.ErrCycle
	}
	f.edges[e.ID()] = e
	return nil
}

// GetActiveByChild returns the single active edge for the supplied
// child, or [rolehierarchy.ErrEdgeNotFound] when the child has no
// parent.
func (f *FakeRepository) GetActiveByChild(_ context.Context, childRoleID role.ID) (*rolehierarchy.Edge, error) {

	for _, e := range f.edges {
		if e.IsActive() && e.ChildRoleID() == childRoleID {
			return e, nil
		}
	}
	return nil, rolehierarchy.ErrEdgeNotFound
}

// UpdateByID loads, mutates via updateFn, persists. Returns
// [rolehierarchy.ErrEdgeNotFound] if the row doesn't exist.
//
// The fake doesn't deep-copy the edge before passing to updateFn; the
// caller observes mutations even if it returns (false, nil). This
// mirrors the pg adapter's behavior — both rely on the aggregate's
// invariants being re-checked at persist time, not snapshot-rollback.
func (f *FakeRepository) UpdateByID(_ context.Context, id rolehierarchy.ID, updateFn func(*rolehierarchy.Edge) (bool, error)) error {

	e, ok := f.edges[id]
	if !ok {
		return rolehierarchy.ErrEdgeNotFound
	}
	commit, err := updateFn(e)
	if err != nil {
		return err
	}
	_ = commit // mutator writes back to *e directly; no separate persist step in fake
	return nil
}

// GetAncestorsByChild walks the active edge chain upward from
// `childRoleID`, returning each ancestor edge in depth order (child's
// parent first → root). Cycle protection via a seen-set so the walk
// terminates even if a stale cycle leaked past Add's guard.
func (f *FakeRepository) GetAncestorsByChild(_ context.Context, childRoleID role.ID) ([]*rolehierarchy.Edge, error) {

	var out []*rolehierarchy.Edge
	cur := childRoleID
	seen := map[role.ID]struct{}{childRoleID: {}}
	for {
		var step *rolehierarchy.Edge
		for _, e := range f.edges {
			if e.IsActive() && e.ChildRoleID() == cur {
				step = e
				break
			}
		}
		if step == nil {
			return out, nil
		}
		if _, dup := seen[step.ParentRoleID()]; dup {
			return out, nil
		}
		seen[step.ParentRoleID()] = struct{}{}
		out = append(out, step)
		cur = step.ParentRoleID()
	}
}

// ListActiveByParent returns every direct active child of
// `parentRoleID`. Order is unspecified — mirrors the SQL adapter's
// ListActiveHierarchyEdgesByParent which delivers rows in index order
// (the test layer relies on equality, not ordering).
func (f *FakeRepository) ListActiveByParent(_ context.Context, parentRoleID role.ID) ([]*rolehierarchy.Edge, error) {

	var out []*rolehierarchy.Edge
	for _, e := range f.edges {
		if e.IsActive() && e.ParentRoleID() == parentRoleID {
			out = append(out, e)
		}
	}
	return out, nil
}

// hasCycle reports whether adding edge child→parent would close a
// loop given the existing edge set. Walks parent upward; if we ever
// land back on child, the edge would create a cycle.
func hasCycle(edges map[rolehierarchy.ID]*rolehierarchy.Edge, child, parent role.ID) bool {

	cur := parent
	seen := map[role.ID]struct{}{child: {}}
	for {
		if _, dup := seen[cur]; dup {
			return true
		}
		seen[cur] = struct{}{}
		var step *rolehierarchy.Edge
		for _, e := range edges {
			if e.IsActive() && e.ChildRoleID() == cur {
				step = e
				break
			}
		}
		if step == nil {
			return false
		}
		cur = step.ParentRoleID()
	}
}
