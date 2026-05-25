//go:build integration

// arch-test:no-timeout-needed — every test in this file uses the shared
//   pgtest container (per-package); pgxpool internal conn timeouts +
//   package-level `task ci:test:int -timeout=15m` already bound execution.
//   Per-test context.WithTimeout would be belt-and-suspenders against the
//   shared-pool + parallel-with-RLS canon shape.
//
// arch-test:parallel-safe — every Test* uses the shared pgtest container
//   + a fresh tenant_id per test bound via tenancy.WithID(); RLS isolates
//   rows by tenant so parallel runs cannot see each others state.
//   Brandur "Postgres at scale" + TDL Wild Workouts canon: shared
//   infrastructure + per-test logical isolation = safe parallelism.
//
// SQL-CONTRACT COVERAGE for this file (ADR 0062 + ADR 0058 — adapter
// integration tests are SQL-contract-only; business-rule + state-
// machine coverage lives in rolehierarchytest.FakeRepository):
//
//   - Partial unique index uq_role_hierarchy_active_edge_per_child
//     (WHERE is_active) → SQLSTATE 23505 → ErrEdgeAlreadyExists.
//   - DB trigger edge_check_cycle (SECURITY INVOKER on the edges table)
//     rejects multi-hop cycle closures → ErrCycle.
//   - Composite FK fk_edges_parent_same_tenant declaratively rejects
//     cross-tenant parent references → ErrCrossTenant (declarative
//     replacement for the Wave 9.1d SECURITY DEFINER trigger).
//   - DB CHECK chk_edge_no_self_loop is a backstop when the aggregate's
//     own ErrSelfReference is bypassed via UnmarshalFromDB → translated
//     back to ErrSelfReference at the adapter layer.
//   - Soft-delete partial-index filtering on reads — GetActiveByChild
//     hides removed edges (WHERE removed_at IS NULL) via SQL predicate.
//   - Recursive CTE in GetAncestorsByChild walks upward through the
//     edge graph (Postgres-specific WITH RECURSIVE).

package adapters_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/rolehierarchy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// freshEdge mints an Edge with a stable test timestamp.
func freshEdge(t *testing.T, tid tenant.ID, child, parent role.ID) *rolehierarchy.Edge {
	t.Helper()
	e, err := rolehierarchy.New(
		rolehierarchy.ID(ids.NewV7().String()),
		tid,
		child,
		parent,
		membership.ID(""),
		"integration test edge — seeded by adapter test fixture",
		time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatalf("rolehierarchy.New: %v", err)
	}
	return e
}

// SQL-contract: partial unique index uq_role_hierarchy_active_edge_per_child
// (predicate `WHERE is_active`) raises SQLSTATE 23505 on a second active
// edge for the same child → ErrEdgeAlreadyExists.
func TestEdgeRepository_Add_RejectsDuplicateActiveChildEdge(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tenants := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))
	edges := adapters.NewRoleHierarchyEdgeRepository(pool, pg.NewTransactor(pool))

	tn := seedTenant(t, tenants)
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))

	p1 := newRole(t, tn.ID(), "Parent1")
	p2 := newRole(t, tn.ID(), "Parent2")
	child := newRole(t, tn.ID(), "Junior")
	for _, r := range []*role.Role{p1, p2, child} {
		if err := roles.Add(ctx, r); err != nil {
			t.Fatalf("seed %s: %v", r.Name(), err)
		}
	}
	if err := edges.Add(ctx, freshEdge(t, tn.ID(), child.ID(), p1.ID())); err != nil {
		t.Fatalf("Add first: %v", err)
	}
	err := edges.Add(ctx, freshEdge(t, tn.ID(), child.ID(), p2.ID()))
	if !errors.Is(err, rolehierarchy.ErrEdgeAlreadyExists) {
		t.Fatalf("Add duplicate: got %v want ErrEdgeAlreadyExists", err)
	}
}

// SQL-contract: DB trigger edge_check_cycle (SECURITY INVOKER on the
// edges table) rejects an edge that would close a multi-hop cycle in
// the active-edge graph. Trigger output translated to ErrCycle.
func TestEdgeRepository_Add_RejectsCycle(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tenants := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))
	edges := adapters.NewRoleHierarchyEdgeRepository(pool, pg.NewTransactor(pool))

	tn := seedTenant(t, tenants)
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))

	a := newRole(t, tn.ID(), "RoleA")
	b := newRole(t, tn.ID(), "RoleB")
	for _, r := range []*role.Role{a, b} {
		if err := roles.Add(ctx, r); err != nil {
			t.Fatalf("seed %s: %v", r.Name(), err)
		}
	}
	// b → a (legal).
	if err := edges.Add(ctx, freshEdge(t, tn.ID(), b.ID(), a.ID())); err != nil {
		t.Fatalf("b → a: %v", err)
	}
	// a → b would close the cycle — DB trigger fires.
	err := edges.Add(ctx, freshEdge(t, tn.ID(), a.ID(), b.ID()))
	if !errors.Is(err, rolehierarchy.ErrCycle) {
		t.Fatalf("expected ErrCycle, got %v", err)
	}
}

// SQL-contract: composite FK fk_edges_parent_same_tenant declaratively
// rejects an edge whose parent role belongs to another tenant.
// Declarative cross-tenant safety per ADR 0058 (replaces the Wave 9.1d
// SECURITY DEFINER trigger). Translated to ErrCrossTenant.
func TestEdgeRepository_Add_RejectsCrossTenant(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tenants := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))
	edges := adapters.NewRoleHierarchyEdgeRepository(pool, pg.NewTransactor(pool))

	tnA := seedTenant(t, tenants)
	tnB := seedTenant(t, tenants)
	ctxA := tenancy.WithID(t.Context(), tenancy.ID(tnA.ID().String()))
	ctxB := tenancy.WithID(t.Context(), tenancy.ID(tnB.ID().String()))

	rA := newRole(t, tnA.ID(), "RoleA")
	rB := newRole(t, tnB.ID(), "RoleB")
	if err := roles.Add(ctxA, rA); err != nil {
		t.Fatalf("Add A: %v", err)
	}
	if err := roles.Add(ctxB, rB); err != nil {
		t.Fatalf("Add B: %v", err)
	}

	// Attempt to add an edge in tenant A whose parent is rB (tenant B).
	// Composite FK fk_edges_parent_same_tenant fires — declarative
	// replacement for the Wave 9.1d SECURITY DEFINER trigger per ADR 0058.
	bad, err := rolehierarchy.New(
		rolehierarchy.ID(ids.NewV7().String()),
		tnA.ID(),
		rA.ID(),
		rB.ID(),
		membership.ID(""),
		"",
		time.Now(),
	)
	if err != nil {
		t.Fatalf("New (should succeed at aggregate; DB rejects): %v", err)
	}
	addErr := edges.Add(ctxA, bad)
	if !errors.Is(addErr, rolehierarchy.ErrCrossTenant) {
		t.Fatalf("expected ErrCrossTenant, got %v", addErr)
	}
}

// SQL-contract: soft-deleted edges vanish from GetActiveByChild via the
// `WHERE removed_at IS NULL` filter on the read path — proves the
// partial-index + read predicate agree at the Postgres layer.
func TestEdgeRepository_GetActiveByChild_FiltersSoftDeleted(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tenants := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))
	edges := adapters.NewRoleHierarchyEdgeRepository(pool, pg.NewTransactor(pool))

	tn := seedTenant(t, tenants)
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))

	p := newRole(t, tn.ID(), "Manager")
	c := newRole(t, tn.ID(), "Junior")
	for _, r := range []*role.Role{p, c} {
		if err := roles.Add(ctx, r); err != nil {
			t.Fatalf("seed %s: %v", r.Name(), err)
		}
	}
	e := freshEdge(t, tn.ID(), c.ID(), p.ID())
	if err := edges.Add(ctx, e); err != nil {
		t.Fatalf("Add: %v", err)
	}
	// Remove → must not appear in GetActiveByChild.
	rmErr := edges.UpdateByID(ctx, tn.ID(), e.ID(), func(loaded *rolehierarchy.Edge) (bool, error) {
		return true, loaded.Remove(membership.ID(""), "removal for the active-only check", time.Now())
	})
	if rmErr != nil {
		t.Fatalf("Remove: %v", rmErr)
	}
	_, err := edges.GetActiveByChild(ctx, tn.ID(), c.ID())
	if !errors.Is(err, rolehierarchy.ErrEdgeNotFound) {
		t.Fatalf("expected ErrEdgeNotFound after soft-delete, got %v", err)
	}
}

// SQL-contract: GetAncestorsByChild uses a Postgres-specific WITH
// RECURSIVE CTE to walk upward through the edge graph. The fake
// approximates this with an in-memory loop, but the recursive CTE
// + depth-first ordering is SQL-specific.
func TestEdgeRepository_GetAncestorsByChild_RecursiveCTEWalksUpward(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tenants := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))
	edges := adapters.NewRoleHierarchyEdgeRepository(pool, pg.NewTransactor(pool))

	tn := seedTenant(t, tenants)
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))

	gp := newRole(t, tn.ID(), "GrandParent")
	p := newRole(t, tn.ID(), "Parent")
	c := newRole(t, tn.ID(), "Child")
	for _, r := range []*role.Role{gp, p, c} {
		if err := roles.Add(ctx, r); err != nil {
			t.Fatalf("seed %s: %v", r.Name(), err)
		}
	}
	// p → gp, c → p.
	if err := edges.Add(ctx, freshEdge(t, tn.ID(), p.ID(), gp.ID())); err != nil {
		t.Fatalf("p → gp: %v", err)
	}
	if err := edges.Add(ctx, freshEdge(t, tn.ID(), c.ID(), p.ID())); err != nil {
		t.Fatalf("c → p: %v", err)
	}

	ancs, err := edges.GetAncestorsByChild(ctx, tn.ID(), c.ID())
	if err != nil {
		t.Fatalf("GetAncestorsByChild: %v", err)
	}
	if len(ancs) != 2 {
		t.Fatalf("ancestors: got %d want 2", len(ancs))
	}
	// Depth-first: c's own edge (c→p) first, then p's edge (p→gp).
	if ancs[0].ChildRoleID() != c.ID() || ancs[0].ParentRoleID() != p.ID() {
		t.Errorf("ancestor[0]: got (%s→%s) want (%s→%s)",
			ancs[0].ChildRoleID(), ancs[0].ParentRoleID(), c.ID(), p.ID())
	}
	if ancs[1].ChildRoleID() != p.ID() || ancs[1].ParentRoleID() != gp.ID() {
		t.Errorf("ancestor[1]: got (%s→%s) want (%s→%s)",
			ancs[1].ChildRoleID(), ancs[1].ParentRoleID(), p.ID(), gp.ID())
	}
}

// SQL-contract: DB CHECK chk_edge_no_self_loop is the schema-level
// backstop when the aggregate's own ErrSelfReference is bypassed via
// UnmarshalFromDB (which doesn't re-validate). The check_violation must
// translate back to ErrSelfReference at the adapter layer.
func TestEdgeRepository_Add_RejectsSelfReference(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tenants := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))
	edges := adapters.NewRoleHierarchyEdgeRepository(pool, pg.NewTransactor(pool))

	tn := seedTenant(t, tenants)
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))

	r := newRole(t, tn.ID(), "RoleSelf")
	if err := roles.Add(ctx, r); err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Aggregate would reject this; bypass via direct construction is
	// blocked by ErrSelfReference. So instead we prove the DB CHECK
	// constraint is a backstop by constructing via UnmarshalFromDB
	// (which doesn't re-validate) and shipping straight to Add — the
	// adapter must translate the resulting check_violation to
	// ErrSelfReference.
	bad := rolehierarchy.UnmarshalFromDB(rolehierarchy.Snapshot{
		ID:           rolehierarchy.ID(ids.NewV7().String()),
		TenantID:     tn.ID(),
		ChildRoleID:  r.ID(),
		ParentRoleID: r.ID(),
	})
	addErr := edges.Add(ctx, bad)
	if !errors.Is(addErr, rolehierarchy.ErrSelfReference) {
		t.Fatalf("expected ErrSelfReference, got %v", addErr)
	}
}
