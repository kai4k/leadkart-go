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
	"github.com/leadkart/leadkart-go/internal/platform/pg"
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

	_, err := roles.GetByID(t.Context(), role.ID(ids.NewV7().String()))
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
