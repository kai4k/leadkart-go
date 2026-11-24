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

package adapters_test

import (
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
)

// ADR 0073 (review G2j M2): the two SQL reads that decide who holds a role's permission and whether
// a tenant has a super-admin filter on is_active. An inactive role confers nothing, so a member
// holding only an inactive role is neither a permission holder nor a super-admin. Mutants: the
// `r.is_active` predicate in permission_holders.sql / memberships.sql replaced by true.
func TestInactiveRole_ConfersNothingInTheHolderAndSuperAdminReads(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	tenants := adapters.NewTenantRepository(pool, tx)
	persons := adapters.NewPersonRepository(pool, tx)
	roles := adapters.NewRoleRepository(pool, tx)
	memberships := adapters.NewMembershipRepository(pool, tx)
	holders := adapters.NewPermissionHolderReader(pool, tx)

	tn := seedTenant(t, tenants)
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))
	perm := permission.IdentityPermissions.Users.View

	// A custom super-admin role (the constructor allows it; system roles cannot be deactivated)
	// carrying one permission, held by an active member.
	r, err := role.NewWithPermissions(role.ID(ids.NewV7().String()), tn.ID(), "Holder Role", false, 40, true,
		[]*permission.Permission{permission.FromConstant(perm)}, testNow)
	if err != nil {
		t.Fatalf("role: %v", err)
	}
	if err := roles.Add(ctx, r); err != nil {
		t.Fatalf("Add role: %v", err)
	}
	p := seedPerson(t, persons, "inactive-role-holder@example.test")
	m, _ := membership.New(membership.ID(ids.NewV7().String()), p.ID(), tn.ID(), membership.ID(""), testNow)
	if err := m.AssignRole(r.ID(), testNow); err != nil {
		t.Fatalf("assign: %v", err)
	}
	if err := memberships.Add(ctx, m); err != nil {
		t.Fatalf("Add member: %v", err)
	}

	count := func() (holderCount int, superAdmin bool) {
		ids, err := holders.ActiveMembershipsHoldingAny(t.Context(), tn.ID(), []string{perm})
		if err != nil {
			t.Fatalf("ActiveMembershipsHoldingAny: %v", err)
		}
		sa, err := memberships.HasActiveSuperAdmin(t.Context(), tn.ID())
		if err != nil {
			t.Fatalf("HasActiveSuperAdmin: %v", err)
		}
		return len(ids), sa
	}
	if n, sa := count(); n != 1 || !sa {
		t.Fatalf("control, active role: %d holders, super-admin %v; want 1 and true", n, sa)
	}

	if err := roles.UpdateByID(ctx, tn.ID(), r.ID(), func(loaded *role.Role) (bool, error) {
		return true, loaded.Deactivate("admin", time.Now().UTC())
	}); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	if n, sa := count(); n != 0 || sa {
		t.Fatalf("inactive role: %d holders, super-admin %v; want 0 and false (it confers nothing)", n, sa)
	}
}
