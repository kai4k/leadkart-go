// Package roletest provides the in-memory FakeRepository implementing
// [role.Repository]. Used by app-layer handler tests + downstream
// integration scenarios that need a working role store without a
// Postgres dependency.
//
// TDL CANON (ThreeDotsLabs Wild Workouts + "Go with the Domain"):
//
//   - The fake lives in a sibling package <aggregate>test/, co-located
//     with the domain aggregate it implements an interface from. Same
//     visibility surface as the aggregate itself; no special test-side
//     plumbing.
//   - The fake is a FAITHFUL implementation of [role.Repository] — not
//     a mock-with-canned-responses. It honors every contract guarantee:
//     ErrNotFound on missing IDs, ErrNameTaken on a duplicate ACTIVE name,
//     soft-delete filtering on GetByID, hierarchy_level + name ordering
//     on ListByTenant.
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
package roletest

import (
	"cmp"
	"context"
	"slices"

	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// FakeRepository is the in-memory implementation of [role.Repository].
// Zero-value-NOT-usable — construct via [NewFakeRepository] so the
// internal maps are initialised. Single-test-owner: do NOT share one
// instance across tests; each test creates its own.
type FakeRepository struct {
	// rows is the role index by ID, active and inactive (a role is never deleted, ADR 0073).
	rows map[role.ID]*role.Role
}

// NewFakeRepository returns an empty in-memory role repository.
// Single-test-owner — each test should construct its own instance;
// do NOT share one fake across parallel tests (no internal sync).
func NewFakeRepository() *FakeRepository {
	return &FakeRepository{
		rows: make(map[role.ID]*role.Role),
	}
}

// Compile-time interface conformance gate. Drift in [role.Repository]
// (a method renamed, signature changed) breaks at build time before
// any test runs.
var _ role.Repository = (*FakeRepository)(nil)

// Add persists a brand-new role. Returns [role.ErrNameTaken] if an ACTIVE role with the same
// (tenant_id, name) already exists (the unique index `uq_roles_tenant_name` is partial on is_active).
func (f *FakeRepository) Add(_ context.Context, r *role.Role) error {
	if r.IsActive() && f.activeHolder(r.TenantID(), r.Name(), r.ID()) {
		return role.ErrNameTaken
	}
	f.rows[r.ID()] = r
	return nil
}

// activeHolder reports whether an active role other than except holds the name.
func (f *FakeRepository) activeHolder(tid tenant.ID, name string, except role.ID) bool {
	for id, o := range f.rows {
		if id != except && o.IsActive() && o.TenantID() == tid && o.Name() == name {
			return true
		}
	}
	return false
}

// UpdateByID loads (scoped to tenantID), mutates via fn, then persists
// (commit=true) or rolls back (commit=false / err). Inactive roles are
// loaded too. Roles outside the supplied tenant return ErrNotFound — mirrors the SQL adapter's RLS-bound behavior.
//
// The fake doesn't deep-copy the role before passing to fn; the caller
// observes mutations even if it returns (false, nil). This mirrors
// the pg adapter — both rely on aggregate invariants being re-checked
// at persist time, not snapshot-rollback.
func (f *FakeRepository) UpdateByID(_ context.Context, tenantID tenant.ID, id role.ID, fn func(*role.Role) (bool, error)) error {

	r, ok := f.rows[id]
	if !ok {
		return role.ErrNotFound
	}
	if r.TenantID() != tenantID {
		return role.ErrNotFound
	}
	commit, err := fn(r)
	if err != nil {
		return err
	}
	if !commit {
		return nil
	}
	// Mirror the partial unique index: an active role's name must be free among the active ones.
	if r.IsActive() && f.activeHolder(r.TenantID(), r.Name(), id) {
		return role.ErrNameTaken
	}
	return nil
}

// GetByID returns the role (scoped to tenantID, active or inactive) or
// [role.ErrNotFound]. Roles in other tenants are hidden — mirrors the SQL
// adapter's RLS-bound behavior.
func (f *FakeRepository) GetByID(_ context.Context, tenantID tenant.ID, id role.ID) (*role.Role, error) {

	r, ok := f.rows[id]
	if !ok {
		return nil, role.ErrNotFound
	}
	if r.TenantID() != tenantID {
		return nil, role.ErrNotFound
	}
	return r, nil
}

// GetByTenantAndName returns the role with the supplied (tenant, name), the active holder first,
// then the newest inactive one, or [role.ErrNotFound].
func (f *FakeRepository) GetByTenantAndName(_ context.Context, tid tenant.ID, name string) (*role.Role, error) {
	var best *role.Role
	for _, r := range f.rows {
		if r.TenantID() != tid || r.Name() != name {
			continue
		}
		if best == nil || (r.IsActive() && !best.IsActive()) ||
			(r.IsActive() == best.IsActive() && r.CreatedAt().After(best.CreatedAt())) {
			best = r
		}
	}
	if best == nil {
		return nil, role.ErrNotFound
	}
	return best, nil
}

// GetByIDs hydrates the supplied IDs scoped to tenantID. Inactive
// roles + roles outside the tenant are silently dropped — mirrors the
// SQL adapter's `AND is_active` filter under RLS. Order of the
// result is unspecified.
func (f *FakeRepository) GetByIDs(_ context.Context, tenantID tenant.ID, ids []role.ID) ([]*role.Role, error) {

	out := make([]*role.Role, 0, len(ids))
	for _, id := range ids {
		r, ok := f.rows[id]
		if !ok || !r.IsActive() {
			continue
		}
		if r.TenantID() != tenantID {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// GetByIDsIncludingInactive is GetByIDs without the active filter.
func (f *FakeRepository) GetByIDsIncludingInactive(_ context.Context, tenantID tenant.ID, ids []role.ID) ([]*role.Role, error) {
	out := make([]*role.Role, 0, len(ids))
	for _, id := range ids {
		r, ok := f.rows[id]
		if !ok || r.TenantID() != tenantID {
			continue
		}
		out = append(out, r)
	}
	return out, nil
}

// ListByTenant returns every role (active and inactive) for the tenant, ordered by
// (hierarchy_level, name) — same order as the SQL adapter so callers
// can rely on stable ordering.
func (f *FakeRepository) ListByTenant(_ context.Context, tid tenant.ID) ([]*role.Role, error) {

	var out []*role.Role
	for _, r := range f.rows {
		if r.TenantID() != tid {
			continue
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b *role.Role) int {
		return cmp.Or(
			cmp.Compare(a.HierarchyLevel(), b.HierarchyLevel()), // hierarchy_level ASC
			cmp.Compare(a.Name(), b.Name()),                     // name ASC
		)
	})
	return out, nil
}
