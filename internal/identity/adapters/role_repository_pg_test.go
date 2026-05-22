//go:build integration

package adapters_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/common/pg"
)

// newRole is a tiny factory the role-repo tests share — gives a brand-new
// non-system-default sales-tier role with one permission grant baked in
// so the JSONB round-trip is non-trivial.
func newRole(t *testing.T, tenantID tenant.ID, name string) *role.Role {
	t.Helper()
	r, err := role.New(
		role.ID(ids.NewV7().String()),
		tenantID,
		name,
		false, // not a system default
		role.HierarchyLevelDefault,
		false, // not super-admin
	)
	if err != nil {
		t.Fatalf("role.New: %v", err)
	}
	view := permission.FromConstant(permission.IdentityPermissions.Roles.View)
	if err := r.GrantPermission(view); err != nil {
		t.Fatalf("GrantPermission: %v", err)
	}
	return r
}

func TestRoleRepository_Add_PersistsAndRoundTripsViaGetByID(t *testing.T) {
	pool := repoFixture(t)
	tenants := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))

	tn := seedTenant(t, tenants)
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))

	r := newRole(t, tn.ID(), "Sales")
	if err := roles.Add(ctx, r); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := roles.GetByID(ctx, r.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.ID() != r.ID() {
		t.Fatalf("id: got %q want %q", got.ID(), r.ID())
	}
	if got.Name() != "Sales" {
		t.Fatalf("name: got %q want Sales", got.Name())
	}
	if got.HierarchyLevel() != role.HierarchyLevelDefault {
		t.Fatalf("hierarchy: got %d want %d", got.HierarchyLevel(), role.HierarchyLevelDefault)
	}
	if len(got.Permissions()) != 1 {
		t.Fatalf("permissions: got %d want 1", len(got.Permissions()))
	}
	if got.Permissions()[0].Name() != permission.IdentityPermissions.Roles.View {
		t.Fatalf("permission name: got %q want %q",
			got.Permissions()[0].Name(), permission.IdentityPermissions.Roles.View)
	}
}

func TestRoleRepository_GetByID_ReturnsNotFound_WhenAbsent(t *testing.T) {
	pool := repoFixture(t)
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))

	// Bind a synthetic tenant — the row is absent so the value
	// doesn't filter anything; the GUC just has to be set so the
	// repo's tenant-scoped tx can open.
	ctx := tenancy.WithID(t.Context(), tenancy.ID(ids.NewV7().String()))
	_, err := roles.GetByID(ctx, role.ID(ids.NewV7().String()))
	if !errors.Is(err, role.ErrNotFound) {
		t.Fatalf("GetByID absent: got %v want ErrNotFound", err)
	}
}

func TestRoleRepository_Add_ReturnsErrNameTaken_OnDuplicateLiveName(t *testing.T) {
	pool := repoFixture(t)
	tenants := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))

	tn := seedTenant(t, tenants)
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))

	first := newRole(t, tn.ID(), "Sales")
	if err := roles.Add(ctx, first); err != nil {
		t.Fatalf("Add first: %v", err)
	}
	dup := newRole(t, tn.ID(), "Sales")
	err := roles.Add(ctx, dup)
	if !errors.Is(err, role.ErrNameTaken) {
		t.Fatalf("Add duplicate: got %v want ErrNameTaken", err)
	}
}

// TestRoleRepository_RLS_IsolatesCrossTenantReads is the canonical
// RLS proof — Tenant A inserts a role; Tenant B's connection scope
// MUST see zero rows. This test would PASS even on a buggy adapter
// because RLS fires at the Postgres layer, not the application — but
// it locks in the contract: bypassing the tenant ctx (forgetting to
// SET LOCAL app.tenant_id) does NOT leak rows because FORCE RLS +
// the non-superuser role together close the door.
func TestRoleRepository_RLS_IsolatesCrossTenantReads(t *testing.T) {
	pool := repoFixture(t)
	tenants := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))

	tnA := seedTenant(t, tenants)
	tnB := seedTenant(t, tenants)
	ctxA := tenancy.WithID(t.Context(), tenancy.ID(tnA.ID().String()))
	ctxB := tenancy.WithID(t.Context(), tenancy.ID(tnB.ID().String()))

	rA := newRole(t, tnA.ID(), "Sales")
	if err := roles.Add(ctxA, rA); err != nil {
		t.Fatalf("Add under A: %v", err)
	}
	// Tenant B's context cannot see Tenant A's role — RLS filters it out.
	_, err := roles.GetByID(ctxB, rA.ID())
	if !errors.Is(err, role.ErrNotFound) {
		t.Fatalf("RLS leak: tenant B saw tenant A's role: %v", err)
	}
}

func TestRoleRepository_ListByTenant_ScopedToCurrentTenantOnly(t *testing.T) {
	pool := repoFixture(t)
	tenants := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))

	tnA := seedTenant(t, tenants)
	tnB := seedTenant(t, tenants)
	ctxA := tenancy.WithID(t.Context(), tenancy.ID(tnA.ID().String()))
	ctxB := tenancy.WithID(t.Context(), tenancy.ID(tnB.ID().String()))

	if err := roles.Add(ctxA, newRole(t, tnA.ID(), "Sales")); err != nil {
		t.Fatalf("Add A1: %v", err)
	}
	if err := roles.Add(ctxA, newRole(t, tnA.ID(), "Manager")); err != nil {
		t.Fatalf("Add A2: %v", err)
	}
	if err := roles.Add(ctxB, newRole(t, tnB.ID(), "Operator")); err != nil {
		t.Fatalf("Add B1: %v", err)
	}

	listA, err := roles.ListByTenant(ctxA, tnA.ID())
	if err != nil {
		t.Fatalf("ListByTenant A: %v", err)
	}
	if len(listA) != 2 {
		t.Fatalf("List under A: got %d want 2", len(listA))
	}
	listB, err := roles.ListByTenant(ctxB, tnB.ID())
	if err != nil {
		t.Fatalf("ListByTenant B: %v", err)
	}
	if len(listB) != 1 {
		t.Fatalf("List under B: got %d want 1", len(listB))
	}
	if listB[0].Name() != "Operator" {
		t.Fatalf("List B name: got %q want Operator", listB[0].Name())
	}
}

func TestRoleRepository_GetByIDs_FiltersOutSoftDeletedAndCrossTenant(t *testing.T) {
	pool := repoFixture(t)
	tenants := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))

	tnA := seedTenant(t, tenants)
	tnB := seedTenant(t, tenants)
	ctxA := tenancy.WithID(t.Context(), tenancy.ID(tnA.ID().String()))
	ctxB := tenancy.WithID(t.Context(), tenancy.ID(tnB.ID().String()))

	rA1 := newRole(t, tnA.ID(), "Sales")
	rA2 := newRole(t, tnA.ID(), "Manager")
	rB := newRole(t, tnB.ID(), "Operator")
	if err := roles.Add(ctxA, rA1); err != nil {
		t.Fatalf("Add A1: %v", err)
	}
	if err := roles.Add(ctxA, rA2); err != nil {
		t.Fatalf("Add A2: %v", err)
	}
	if err := roles.Add(ctxB, rB); err != nil {
		t.Fatalf("Add B: %v", err)
	}

	// Tenant A asks for [rA1, rA2, rB] — RLS hides rB; expect 2 rows.
	got, err := roles.GetByIDs(ctxA, []role.ID{rA1.ID(), rA2.ID(), rB.ID()})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("GetByIDs (cross-tenant): got %d want 2 (B should be hidden)", len(got))
	}

	// Empty input returns empty result, not error.
	got, err = roles.GetByIDs(ctxA, nil)
	if err != nil {
		t.Fatalf("GetByIDs nil: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("GetByIDs nil: got %d want 0", len(got))
	}
}

// ----- Task 17 — UpdateByID (TDL Sep 2024 UpdateFn) ------------------------

func TestRoleRepository_UpdateByID_Rename_Persists(t *testing.T) {
	pool := repoFixture(t)
	tenants := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))

	tn := seedTenant(t, tenants)
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))
	r := newRole(t, tn.ID(), "Sales")
	if err := roles.Add(ctx, r); err != nil {
		t.Fatalf("Add: %v", err)
	}

	err := roles.UpdateByID(ctx, r.ID(), func(loaded *role.Role) (bool, error) {
		return true, loaded.Rename("Senior Sales")
	})
	if err != nil {
		t.Fatalf("UpdateByID: %v", err)
	}

	got, err := roles.GetByID(ctx, r.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name() != "Senior Sales" {
		t.Fatalf("name: got %q want Senior Sales", got.Name())
	}
}

func TestRoleRepository_UpdateByID_GrantPermission_Persists(t *testing.T) {
	pool := repoFixture(t)
	tenants := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))

	tn := seedTenant(t, tenants)
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))
	r := newRole(t, tn.ID(), "Sales")
	if err := roles.Add(ctx, r); err != nil {
		t.Fatalf("Add: %v", err)
	}

	rolesAssign := permission.FromConstant(permission.IdentityPermissions.Roles.Assign)
	err := roles.UpdateByID(ctx, r.ID(), func(loaded *role.Role) (bool, error) {
		return true, loaded.GrantPermission(rolesAssign)
	})
	if err != nil {
		t.Fatalf("UpdateByID grant: %v", err)
	}

	got, err := roles.GetByID(ctx, r.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if len(got.Permissions()) != 2 {
		t.Fatalf("permissions: got %d want 2 (Roles.View + Roles.Assign)", len(got.Permissions()))
	}
	// Order isn't load-bearing — assert set membership.
	hasView := false
	hasAssign := false
	for _, p := range got.Permissions() {
		switch p.Name() {
		case permission.IdentityPermissions.Roles.View:
			hasView = true
		case permission.IdentityPermissions.Roles.Assign:
			hasAssign = true
		}
	}
	if !hasView || !hasAssign {
		t.Fatalf("permissions set: hasView=%v hasAssign=%v", hasView, hasAssign)
	}
}

func TestRoleRepository_UpdateByID_Delete_PersistsSoftDeleteAndHidesFromGetByID(t *testing.T) {
	pool := repoFixture(t)
	tenants := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))

	tn := seedTenant(t, tenants)
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))
	r := newRole(t, tn.ID(), "Sales")
	if err := roles.Add(ctx, r); err != nil {
		t.Fatalf("Add: %v", err)
	}

	err := roles.UpdateByID(ctx, r.ID(), func(loaded *role.Role) (bool, error) {
		return true, loaded.Delete("admin@example.test")
	})
	if err != nil {
		t.Fatalf("UpdateByID delete: %v", err)
	}

	// Live read filters soft-deleted rows.
	_, err = roles.GetByID(ctx, r.ID())
	if !errors.Is(err, role.ErrNotFound) {
		t.Fatalf("GetByID after delete: got %v want ErrNotFound", err)
	}
}

func TestRoleRepository_UpdateByID_ReturnsNotFound_WhenAbsent(t *testing.T) {
	pool := repoFixture(t)
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))

	// Bind a synthetic tenant — the row is absent so the value
	// doesn't filter anything; the GUC just has to be set so the
	// repo's tenant-scoped tx can open.
	ctx := tenancy.WithID(t.Context(), tenancy.ID(ids.NewV7().String()))
	err := roles.UpdateByID(ctx, role.ID(ids.NewV7().String()),
		func(*role.Role) (bool, error) { return true, nil })
	if !errors.Is(err, role.ErrNotFound) {
		t.Fatalf("UpdateByID absent: got %v want ErrNotFound", err)
	}
}

func TestRoleRepository_UpdateByID_NoOp_WhenUpdateFnReturnsFalse(t *testing.T) {
	pool := repoFixture(t)
	tenants := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))

	tn := seedTenant(t, tenants)
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))
	r := newRole(t, tn.ID(), "Sales")
	if err := roles.Add(ctx, r); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Mutate the loaded aggregate but instruct the repo NOT to persist.
	err := roles.UpdateByID(ctx, r.ID(), func(loaded *role.Role) (bool, error) {
		_ = loaded.Rename("ShouldNotStick")
		return false, nil
	})
	if err != nil {
		t.Fatalf("UpdateByID no-op: %v", err)
	}

	got, err := roles.GetByID(ctx, r.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name() != "Sales" {
		t.Fatalf("name after no-op: got %q want Sales", got.Name())
	}
}

func TestRoleRepository_UpdateByID_Rollback_WhenUpdateFnErrors(t *testing.T) {
	pool := repoFixture(t)
	tenants := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))

	tn := seedTenant(t, tenants)
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))
	r := newRole(t, tn.ID(), "Sales")
	if err := roles.Add(ctx, r); err != nil {
		t.Fatalf("Add: %v", err)
	}

	sentinel := errors.New("update intentionally failed")
	err := roles.UpdateByID(ctx, r.ID(), func(loaded *role.Role) (bool, error) {
		_ = loaded.Rename("ShouldRollBack")
		return true, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("UpdateByID error: got %v want sentinel", err)
	}

	got, err := roles.GetByID(ctx, r.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name() != "Sales" {
		t.Fatalf("name after rollback: got %q want Sales", got.Name())
	}
}

// ----- ADR 0054 hierarchy --------------------------------------------------

// TestRoleRepository_Hierarchy_TriggerRejectsCycle proves the DB-level
// cycle gate fires when a parent_role_id link would close a loop:
// create A → B (B parents to A), then attempt A → B (A parents to B) =
// cycle. ADR 0054 — defense-in-depth strict gate even when the domain
// guard is bypassed.
func TestRoleRepository_Hierarchy_TriggerRejectsCycle(t *testing.T) {
	pool := repoFixture(t)
	tenants := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))

	tn := seedTenant(t, tenants)
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))

	a := newRole(t, tn.ID(), "A")
	b := newRole(t, tn.ID(), "B")
	if err := roles.Add(ctx, a); err != nil {
		t.Fatalf("Add A: %v", err)
	}
	if err := roles.Add(ctx, b); err != nil {
		t.Fatalf("Add B: %v", err)
	}

	// B parents to A (legal).
	if err := roles.UpdateByID(ctx, b.ID(), func(loaded *role.Role) (bool, error) {
		return true, loaded.ChangeParent(a.ID(), func(role.ID) ([]role.ID, error) { return nil, nil })
	}); err != nil {
		t.Fatalf("B → A: %v", err)
	}

	// A parents to B would close the loop A → B → A. The domain guard
	// catches it first via ancestor lookup, but to prove the DB trigger
	// is the strict gate we drive ChangeParent with an EMPTY ancestor
	// lookup — that bypasses the domain check, leaving only the trigger.
	err := roles.UpdateByID(ctx, a.ID(), func(loaded *role.Role) (bool, error) {
		return true, loaded.ChangeParent(b.ID(), func(role.ID) ([]role.ID, error) { return nil, nil })
	})
	if !errors.Is(err, role.ErrHierarchyCycle) {
		t.Fatalf("expected ErrHierarchyCycle from DB trigger, got %v", err)
	}
}

// TestRoleRepository_Hierarchy_TriggerRejectsCrossTenant proves the
// DB-level cross-tenant gate fires when parent_role_id references a
// role in a different tenant. ADR 0054.
func TestRoleRepository_Hierarchy_TriggerRejectsCrossTenant(t *testing.T) {
	pool := repoFixture(t)
	tenants := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))

	tnA := seedTenant(t, tenants)
	tnB := seedTenant(t, tenants)
	ctxA := tenancy.WithID(t.Context(), tenancy.ID(tnA.ID().String()))
	ctxB := tenancy.WithID(t.Context(), tenancy.ID(tnB.ID().String()))

	rA := newRole(t, tnA.ID(), "A")
	rB := newRole(t, tnB.ID(), "B")
	if err := roles.Add(ctxA, rA); err != nil {
		t.Fatalf("Add A: %v", err)
	}
	if err := roles.Add(ctxB, rB); err != nil {
		t.Fatalf("Add B: %v", err)
	}

	// rB attempts to parent to rA — cross-tenant. The domain guard
	// doesn't see rA under tenant B's RLS scope, so ancestor lookup
	// returns nothing — the cycle check passes, but the trigger
	// rejects on tenant mismatch. (Platform-scoped writer would hit
	// the same path.)
	err := roles.UpdateByID(ctxB, rB.ID(), func(loaded *role.Role) (bool, error) {
		return true, loaded.ChangeParent(rA.ID(), func(role.ID) ([]role.ID, error) { return nil, nil })
	})
	if !errors.Is(err, role.ErrHierarchyCrossTenant) {
		t.Fatalf("expected ErrHierarchyCrossTenant from DB trigger, got %v", err)
	}
}

// TestRoleRepository_GetAncestors_WalksUpward proves the recursive CTE
// returns the full ancestor chain in depth order.
func TestRoleRepository_GetAncestors_WalksUpward(t *testing.T) {
	pool := repoFixture(t)
	tenants := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))

	tn := seedTenant(t, tenants)
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))

	gp := newRole(t, tn.ID(), "GP")
	p := newRole(t, tn.ID(), "P")
	c := newRole(t, tn.ID(), "C")
	for _, r := range []*role.Role{gp, p, c} {
		if err := roles.Add(ctx, r); err != nil {
			t.Fatalf("Add %s: %v", r.Name(), err)
		}
	}
	// Link p → gp, c → p.
	link := func(child *role.Role, parent role.ID) {
		if err := roles.UpdateByID(ctx, child.ID(), func(loaded *role.Role) (bool, error) {
			return true, loaded.ChangeParent(parent, func(role.ID) ([]role.ID, error) { return nil, nil })
		}); err != nil {
			t.Fatalf("link %s: %v", child.Name(), err)
		}
	}
	link(p, gp.ID())
	link(c, p.ID())

	ancs, err := roles.GetAncestors(ctx, c.ID())
	if err != nil {
		t.Fatalf("GetAncestors: %v", err)
	}
	if len(ancs) != 2 {
		t.Fatalf("ancestors: got %d want 2", len(ancs))
	}
	// First entry = parent (P), second = grandparent (GP).
	if ancs[0].ID() != p.ID() || ancs[1].ID() != gp.ID() {
		t.Fatalf("ancestor order: got [%s, %s] want [%s, %s]",
			ancs[0].Name(), ancs[1].Name(), p.Name(), gp.Name())
	}
}

func TestRoleRepository_UpdateByID_RLS_RefusesCrossTenantUpdate(t *testing.T) {
	pool := repoFixture(t)
	tenants := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))

	tnA := seedTenant(t, tenants)
	tnB := seedTenant(t, tenants)
	ctxA := tenancy.WithID(t.Context(), tenancy.ID(tnA.ID().String()))
	ctxB := tenancy.WithID(t.Context(), tenancy.ID(tnB.ID().String()))

	rA := newRole(t, tnA.ID(), "Sales")
	if err := roles.Add(ctxA, rA); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Tenant B's context cannot load Tenant A's role — RLS hides it,
	// so UpdateByID surfaces ErrNotFound (the load step fails before
	// updateFn ever runs).
	called := false
	err := roles.UpdateByID(ctxB, rA.ID(), func(*role.Role) (bool, error) {
		called = true
		return true, nil
	})
	if !errors.Is(err, role.ErrNotFound) {
		t.Fatalf("UpdateByID cross-tenant: got %v want ErrNotFound", err)
	}
	if called {
		t.Fatalf("updateFn ran under wrong tenant context — RLS leak")
	}
}
