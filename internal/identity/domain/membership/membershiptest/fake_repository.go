// Package membershiptest provides the in-memory FakeRepository
// implementing [membership.Repository]. Used by app-layer handler tests
// + downstream integration scenarios that need a working Membership
// store without a Postgres dependency.
//
// TDL CANON (ThreeDotsLabs Wild Workouts + "Go with the Domain"):
//
//   - The fake lives in a sibling package <aggregate>test/, co-located
//     with the domain aggregate it implements an interface from. Same
//     visibility surface as the aggregate itself; no special test-side
//     plumbing.
//   - The fake is a FAITHFUL implementation of [membership.Repository]
//     — not a mock-with-canned-responses. It honors every contract
//     guarantee: ErrNotFound on missing IDs, ErrAlreadyActive on
//     re-Add of a second-Active for the same Person (mirrors the SQL
//     partial-unique index uq_memberships_person_active), joined_at
//     ordering on ListForTenant, status='active' + tuple-comparison
//     keyset on ListForTenantPage, active-only filter on
//     HasActiveSuperAdmin (the fake answers conservatively false since
//     it has no role-flag knowledge — see method doc).
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
package membershiptest

import (
	"context"
	"sort"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// FakeRepository is the in-memory implementation of
// [membership.Repository]. Zero-value-NOT-usable — construct via
// [NewFakeRepository] so the internal map is initialised. Single-test-
// owner: do NOT share one instance across tests; each test creates its
// own.
type FakeRepository struct {
	// rows is the Membership index by ID. Memberships are not
	// row-level soft-deleted; the aggregate carries a Status enum
	// (Active / Inactive). Lookups by ID succeed for every row
	// regardless of status — the contract method that filters is
	// the `_active_only` variants (ListForTenantPage, GetActiveForPerson,
	// HasActiveSuperAdmin).
	rows map[membership.ID]*membership.Membership
}

// NewFakeRepository returns an empty in-memory membership repository.
// Single-test-owner — each test should construct its own instance;
// do NOT share one fake across parallel tests (no internal sync).
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{rows: make(map[membership.ID]*membership.Membership)}
}

// Compile-time interface conformance gate. Drift in
// [membership.Repository] (a method renamed, signature changed) breaks
// at build time before any test runs.
var _ membership.Repository = (*FakeRepository)(nil)

// Add persists a brand-new Membership. Returns
// [membership.ErrAlreadyActive] if the PersonID already has an Active
// Membership (mirrors the SQL adapter's translation of SQLSTATE 23505
// from the partial unique index uq_memberships_person_active —
// `WHERE status='active' AND NOT is_deleted`).
func (f *FakeRepository) Add(_ context.Context, m *membership.Membership) error {

	if m.Status() == membership.StatusActive {
		for _, existing := range f.rows {
			if existing.PersonID() == m.PersonID() && existing.Status() == membership.StatusActive {
				return membership.ErrAlreadyActive
			}
		}
	}
	f.rows[m.ID()] = m
	return nil
}

// UpdateByID loads, mutates via updateFn, persists. Returns
// [membership.ErrNotFound] if the Membership doesn't exist.
//
// The fake doesn't deep-copy the Membership before passing to updateFn;
// the caller observes mutations even if it returns (false, nil). This
// mirrors the pg adapter's behavior — both rely on the aggregate's
// invariants being re-checked at persist time, not snapshot-rollback.
func (f *FakeRepository) UpdateByID(_ context.Context, tenantID tenant.ID, id membership.ID, updateFn func(*membership.Membership) (bool, error)) error {

	m, ok := f.rows[id]
	if !ok {
		return membership.ErrNotFound
	}
	if m.TenantID() != tenantID {
		return membership.ErrNotFound
	}
	commit, err := updateFn(m)
	if err != nil {
		return err
	}
	_ = commit // mutator writes back to *m directly; no separate persist step in fake
	return nil
}

// GetByID returns the Membership or [membership.ErrNotFound]. Returns
// any status (Active or Inactive); active-only filtering is the
// caller's responsibility (mirrors the SQL adapter's RLS-scoped read
// which returns rows of any status).
func (f *FakeRepository) GetByID(_ context.Context, tenantID tenant.ID, id membership.ID) (*membership.Membership, error) {

	m, ok := f.rows[id]
	if !ok {
		return nil, membership.ErrNotFound
	}
	if m.TenantID() != tenantID {
		return nil, membership.ErrNotFound
	}
	return m, nil
}

// GetActiveForPerson returns the single Active Membership for a Person
// or [membership.ErrNotFound]. Mirrors the SQL adapter's lookup via the
// partial-unique index uq_memberships_person_active — guaranteed at-
// most-one row.
func (f *FakeRepository) GetActiveForPerson(_ context.Context, personID person.ID) (*membership.Membership, error) {

	for _, m := range f.rows {
		if m.PersonID() == personID && m.Status() == membership.StatusActive {
			return m, nil
		}
	}
	return nil, membership.ErrNotFound
}

// ListForTenant returns all Memberships under the supplied tenant
// (every status) ordered by joined_at — same ORDER BY clause as the
// SQL adapter's ListMembershipsInCurrentTenant.
func (f *FakeRepository) ListForTenant(_ context.Context, tenantID tenant.ID) ([]*membership.Membership, error) {

	var out []*membership.Membership
	for _, m := range f.rows {
		if m.TenantID() == tenantID {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].JoinedAt().Before(out[j].JoinedAt())
	})
	return out, nil
}

// ListForTenantPage returns the keyset-paginated slice of ACTIVE
// Memberships per ADR 0038. Mirrors the SQL adapter's
// ListActiveMembershipsInTenantPage:
//   - filter: status = 'active'
//   - cursor predicate: (joined_at, id) < (beforeJoinedAt, beforeID)
//   - order: joined_at DESC, id DESC
//   - limit applied AFTER ordering
//
// Note: the SQL adapter takes a beforeID as uuid; the fake compares as
// string. limit must be positive (mirrors the adapter's validation).
func (f *FakeRepository) ListForTenantPage(_ context.Context, tenantID tenant.ID, beforeJoinedAt time.Time, beforeID string, limit int) ([]*membership.Membership, error) {

	var out []*membership.Membership
	for _, m := range f.rows {
		if m.TenantID() != tenantID {
			continue
		}
		if m.Status() != membership.StatusActive {
			continue
		}
		// (joined_at, id) < (before_joined_at, before_id) — strict
		// lexicographic tuple comparison.
		if m.JoinedAt().Before(beforeJoinedAt) {
			out = append(out, m)
			continue
		}
		if m.JoinedAt().Equal(beforeJoinedAt) && m.ID().String() < beforeID {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].JoinedAt().Equal(out[j].JoinedAt()) {
			return out[i].JoinedAt().After(out[j].JoinedAt()) // DESC
		}
		return out[i].ID().String() > out[j].ID().String() // DESC
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ListAllForPerson returns every Membership (Active + Inactive) the
// supplied Person holds across all tenants. Empty slice (not
// ErrNotFound) when none exist — matches the SQL adapter's
// ListMembershipsForPerson semantics.
func (f *FakeRepository) ListAllForPerson(_ context.Context, personID person.ID) ([]*membership.Membership, error) {

	var out []*membership.Membership
	for _, m := range f.rows {
		if m.PersonID() == personID {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].JoinedAt().Before(out[j].JoinedAt())
	})
	return out, nil
}

// HasActiveSuperAdmin always returns false in the fake — the
// Membership aggregate doesn't expose the role's is_super_admin flag
// (that joins through identity.role_assignments + identity.roles in
// the SQL adapter). Tests that depend on a true return value should
// either:
//
//   - use a dedicated test double (a per-test override map keyed by
//     tenant.ID — see the `permissions` package's resolver test for
//     prior art), or
//   - run the test as an integration test against testcontainers (the
//     SQL adapter's ListSuperAdminMembershipsInTenant query is the
//     source of truth).
//
// The conservative-false answer mirrors the empty-tenant case in
// production — i.e. a tenant with no SuperAdmin role assignments. It
// does NOT bypass platform-tenant deletion guards; the deletion
// commands consult this method via the SQL adapter at runtime.
func (f *FakeRepository) HasActiveSuperAdmin(_ context.Context, _ tenant.ID) (bool, error) {

	return false, nil
}
