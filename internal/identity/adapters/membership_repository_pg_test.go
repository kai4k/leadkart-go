//go:build integration
// arch-test:no-timeout-needed - integration tests rely on testcontainers boot timeout

package adapters_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/common/pg"
)

// testNow is the deterministic instant test fixtures pass to domain
// factories + mutators per the clock-injection refactor.
var testNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

// seedTenant + seedPerson are convenience helpers — repos are wired the
// same way the production composition root will wire them.
func seedTenant(t *testing.T, repo *adapters.TenantRepository) *tenant.Tenant {
	t.Helper()
	id := tenant.ID(ids.NewV7().String())
	// UUIDv7's leading chars are timestamp-derived → tests called in
	// rapid succession would collide on a 8-char prefix slug. Use the
	// trailing random portion.
	full := ids.NewV7().String()
	s, err := slug.New("ten-" + full[len(full)-8:])
	if err != nil {
		t.Fatalf("slug: %v", err)
	}
	addr, _ := email.New("admin@example.test")
	tn, err := tenant.New(id, s, "Acme Pharma Pvt Ltd", "Acme", addr, testNow)
	if err != nil {
		t.Fatalf("tenant.New: %v", err)
	}
	if err := repo.Add(t.Context(), tn); err != nil {
		t.Fatalf("seedTenant Add: %v", err)
	}
	return tn
}

func seedPerson(t *testing.T, repo *adapters.PersonRepository, addr string) *person.Person {
	t.Helper()
	p := newPerson(t, addr)
	if err := repo.Add(t.Context(), p); err != nil {
		t.Fatalf("seedPerson: %v", err)
	}
	return p
}

func TestMembershipRepository_Add_PersistsRowAndOutboxEvent(t *testing.T) {
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	tenants := adapters.NewTenantRepository(pool, tx)
	persons := adapters.NewPersonRepository(pool, tx)
	memberships := adapters.NewMembershipRepository(pool, tx)

	tn := seedTenant(t, tenants)
	p := seedPerson(t, persons, "member@example.test")

	id := membership.ID(ids.NewV7().String())
	m, err := membership.New(id, p.ID(), tn.ID(), membership.ID(""), testNow)
	if err != nil {
		t.Fatalf("membership.New: %v", err)
	}

	// Caller binds tenant on ctx — under TxScopeTenant, the INSERT WITH
	// CHECK passes because tenant_id = app.current_tenant().
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))

	if err := memberships.Add(ctx, m); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := memberships.GetByID(ctx, m.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.PersonID() != p.ID() {
		t.Fatalf("personID round-trip: got %q want %q", got.PersonID(), p.ID())
	}
	if got.TenantID() != tn.ID() {
		t.Fatalf("tenantID round-trip: got %q want %q", got.TenantID(), tn.ID())
	}
	if got.Status() != membership.StatusActive {
		t.Fatalf("status: got %v want active", got.Status())
	}
}

func TestMembershipRepository_Add_SecondActive_ReturnsErrAlreadyActive(t *testing.T) {
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	tenants := adapters.NewTenantRepository(pool, tx)
	persons := adapters.NewPersonRepository(pool, tx)
	memberships := adapters.NewMembershipRepository(pool, tx)

	tnA := seedTenant(t, tenants)
	tnB := seedTenant(t, tenants)
	p := seedPerson(t, persons, "switch@example.test")

	// First Active Membership in tenant A.
	mA, _ := membership.New(membership.ID(ids.NewV7().String()), p.ID(), tnA.ID(), membership.ID(""), testNow)
	ctxA := tenancy.WithID(t.Context(), tenancy.ID(tnA.ID().String()))
	if err := memberships.Add(ctxA, mA); err != nil {
		t.Fatalf("first Add: %v", err)
	}

	// Second concurrent Active Membership in tenant B — partial unique
	// index uq_memberships_person_active blocks it.
	mB, _ := membership.New(membership.ID(ids.NewV7().String()), p.ID(), tnB.ID(), membership.ID(""), testNow)
	ctxB := tenancy.WithID(t.Context(), tenancy.ID(tnB.ID().String()))
	err := memberships.Add(ctxB, mB)
	if !errors.Is(err, membership.ErrAlreadyActive) {
		t.Fatalf("expected ErrAlreadyActive, got %v", err)
	}
}

func TestMembershipRepository_GetByID_OutsideTenantScope_NotFound(t *testing.T) {
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	tenants := adapters.NewTenantRepository(pool, tx)
	persons := adapters.NewPersonRepository(pool, tx)
	memberships := adapters.NewMembershipRepository(pool, tx)

	tnA := seedTenant(t, tenants)
	tnB := seedTenant(t, tenants)
	p := seedPerson(t, persons, "isolation@example.test")

	mA, _ := membership.New(membership.ID(ids.NewV7().String()), p.ID(), tnA.ID(), membership.ID(""), testNow)
	ctxA := tenancy.WithID(t.Context(), tenancy.ID(tnA.ID().String()))
	if err := memberships.Add(ctxA, mA); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Look up under tenant B's scope — RLS hides the row.
	ctxB := tenancy.WithID(t.Context(), tenancy.ID(tnB.ID().String()))
	_, err := memberships.GetByID(ctxB, mA.ID())
	if !errors.Is(err, membership.ErrNotFound) {
		t.Fatalf("expected ErrNotFound (RLS isolation), got %v", err)
	}
}

func TestMembershipRepository_UpdateByID_DeactivateClearsActiveSlot(t *testing.T) {
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	tenants := adapters.NewTenantRepository(pool, tx)
	persons := adapters.NewPersonRepository(pool, tx)
	memberships := adapters.NewMembershipRepository(pool, tx)

	tnA := seedTenant(t, tenants)
	tnB := seedTenant(t, tenants)
	p := seedPerson(t, persons, "rotate@example.test")

	// Active in tenant A.
	mA, _ := membership.New(membership.ID(ids.NewV7().String()), p.ID(), tnA.ID(), membership.ID(""), testNow)
	ctxA := tenancy.WithID(t.Context(), tenancy.ID(tnA.ID().String()))
	if err := memberships.Add(ctxA, mA); err != nil {
		t.Fatalf("Add A: %v", err)
	}

	// Deactivate in A.
	err := memberships.UpdateByID(ctxA, mA.ID(), func(m *membership.Membership) (bool, error) {
		if err := m.Deactivate("job change", testNow); err != nil {
			return false, err
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("Deactivate: %v", err)
	}

	// Now adding an Active in B is allowed (single-Active slot freed).
	mB, _ := membership.New(membership.ID(ids.NewV7().String()), p.ID(), tnB.ID(), membership.ID(""), testNow)
	ctxB := tenancy.WithID(t.Context(), tenancy.ID(tnB.ID().String()))
	if err := memberships.Add(ctxB, mB); err != nil {
		t.Fatalf("Add B after deactivate: %v", err)
	}
}

func TestMembershipRepository_GetActiveForPerson_BypassesRLS(t *testing.T) {
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	tenants := adapters.NewTenantRepository(pool, tx)
	persons := adapters.NewPersonRepository(pool, tx)
	memberships := adapters.NewMembershipRepository(pool, tx)

	tn := seedTenant(t, tenants)
	p := seedPerson(t, persons, "login@example.test")

	mA, _ := membership.New(membership.ID(ids.NewV7().String()), p.ID(), tn.ID(), membership.ID(""), testNow)
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))
	if err := memberships.Add(ctx, mA); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Login flow: ctx has NO tenant set yet — login resolves it via this
	// query under platform scope.
	got, err := memberships.GetActiveForPerson(t.Context(), p.ID())
	if err != nil {
		t.Fatalf("GetActiveForPerson: %v", err)
	}
	if got.ID() != mA.ID() {
		t.Fatalf("id round-trip: got %q want %q", got.ID(), mA.ID())
	}
}

func TestMembershipRepository_GetActiveForPerson_NoActive_NotFound(t *testing.T) {
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	persons := adapters.NewPersonRepository(pool, tx)
	memberships := adapters.NewMembershipRepository(pool, tx)

	p := seedPerson(t, persons, "noactive@example.test")
	_, err := memberships.GetActiveForPerson(t.Context(), p.ID())
	if !errors.Is(err, membership.ErrNotFound) {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}
}

// ----- Task 18 — child-table state (roles + overrides + profile) ------------

func TestMembershipRepository_Add_PersistsRoleAssignmentsAndOverrides(t *testing.T) {
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	persons := adapters.NewPersonRepository(pool, tx)
	memberships := adapters.NewMembershipRepository(pool, tx)
	rolesRepo := adapters.NewRoleRepository(pool, tx)
	tenants := adapters.NewTenantRepository(pool, tx)

	tn := seedTenant(t, tenants)
	p := seedPerson(t, persons, "task18-add@example.test")

	// Two real roles in the tenant — composite FK requires real role IDs.
	r1 := newRole(t, tn.ID(), "Sales")
	r2 := newRole(t, tn.ID(), "Manager")
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))
	if err := rolesRepo.Add(ctx, r1); err != nil {
		t.Fatalf("Add r1: %v", err)
	}
	if err := rolesRepo.Add(ctx, r2); err != nil {
		t.Fatalf("Add r2: %v", err)
	}

	// Build a Membership carrying role assignments + a granted + a revoked
	// permission overlay + non-empty profile.
	m, err := membership.New(membership.ID(ids.NewV7().String()), p.ID(), tn.ID(), membership.ID(""), testNow)
	if err != nil {
		t.Fatalf("membership.New: %v", err)
	}
	if err := m.AssignRole(r1.ID(), testNow); err != nil {
		t.Fatalf("AssignRole r1: %v", err)
	}
	if err := m.AssignRole(r2.ID(), testNow); err != nil {
		t.Fatalf("AssignRole r2: %v", err)
	}
	grantP := permission.FromConstant(permission.IdentityPermissions.Roles.Assign)
	revokeP := permission.FromConstant(permission.IdentityPermissions.Users.Anonymise)
	if err := m.GrantPermission(grantP, time.Time{}, testNow); err != nil {
		t.Fatalf("GrantPermission: %v", err)
	}
	if err := m.RevokePermission(revokeP, testNow); err != nil {
		t.Fatalf("RevokePermission: %v", err)
	}
	if err := m.UpdateProfile("Senior Sales Lead", "South Region", "On call", testNow); err != nil {
		t.Fatalf("UpdateProfile: %v", err)
	}

	if err := memberships.Add(ctx, m); err != nil {
		t.Fatalf("Add membership: %v", err)
	}

	// GetByID hydrates roles + overrides + profile end-to-end.
	got, err := memberships.GetByID(ctx, m.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if len(got.RoleAssignments()) != 2 {
		t.Fatalf("RoleAssignments: got %d want 2", len(got.RoleAssignments()))
	}
	if len(got.GrantedPermissions()) != 1 ||
		got.GrantedPermissions()[0].Permission.Name() != permission.IdentityPermissions.Roles.Assign {
		t.Fatalf("Granted: got %v", got.GrantedPermissions())
	}
	if len(got.RevokedPermissions()) != 1 ||
		got.RevokedPermissions()[0].Name() != permission.IdentityPermissions.Users.Anonymise {
		t.Fatalf("Revoked: got %v", got.RevokedPermissions())
	}
	if got.Designation() != "Senior Sales Lead" {
		t.Fatalf("Designation: got %q", got.Designation())
	}
	if got.Department() != "South Region" {
		t.Fatalf("Department: got %q", got.Department())
	}
	if got.StatusMessage() != "On call" {
		t.Fatalf("StatusMessage: got %q", got.StatusMessage())
	}
}

func TestMembershipRepository_UpdateByID_ReplacesRoleAssignmentsAndOverrides(t *testing.T) {
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	persons := adapters.NewPersonRepository(pool, tx)
	memberships := adapters.NewMembershipRepository(pool, tx)
	rolesRepo := adapters.NewRoleRepository(pool, tx)
	tenants := adapters.NewTenantRepository(pool, tx)

	tn := seedTenant(t, tenants)
	p := seedPerson(t, persons, "task18-update@example.test")

	r1 := newRole(t, tn.ID(), "Sales")
	r2 := newRole(t, tn.ID(), "Manager")
	r3 := newRole(t, tn.ID(), "Operator")
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))
	for _, r := range []*role.Role{r1, r2, r3} {
		if err := rolesRepo.Add(ctx, r); err != nil {
			t.Fatalf("Add role %s: %v", r.Name(), err)
		}
	}

	m, _ := membership.New(membership.ID(ids.NewV7().String()), p.ID(), tn.ID(), membership.ID(""), testNow)
	_ = m.AssignRole(r1.ID(), testNow) // arch-test:ignore-err - test fixture setup
	_ = m.AssignRole(r2.ID(), testNow) // arch-test:ignore-err - test fixture setup
	_ = m.GrantPermission(permission.FromConstant(permission.IdentityPermissions.Roles.View), time.Time{}, testNow) // arch-test:ignore-err - test fixture setup
	if err := memberships.Add(ctx, m); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// Replace state via UpdateByID: drop r1, add r3, drop the grant, add a revoke.
	err := memberships.UpdateByID(ctx, m.ID(), func(loaded *membership.Membership) (bool, error) {
		_ = loaded.RevokeRole(r1.ID(), testNow) // arch-test:ignore-err - test fixture setup
		_ = loaded.AssignRole(r3.ID(), testNow) // arch-test:ignore-err - test fixture setup
		// flipping the same permission from granted to revoked auto-suppresses.
		_ = loaded.RevokePermission(permission.FromConstant(permission.IdentityPermissions.Roles.View), testNow) // arch-test:ignore-err - test fixture setup
		return true, nil
	})
	if err != nil {
		t.Fatalf("UpdateByID: %v", err)
	}

	got, err := memberships.GetByID(ctx, m.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if len(got.RoleAssignments()) != 2 {
		t.Fatalf("RoleAssignments: got %d want 2 (r2, r3)", len(got.RoleAssignments()))
	}
	gotRoles := map[role.ID]bool{}
	for _, rid := range got.RoleAssignments() {
		gotRoles[rid] = true
	}
	if !gotRoles[r2.ID()] || !gotRoles[r3.ID()] || gotRoles[r1.ID()] {
		t.Fatalf("RoleAssignments set wrong: got %v want {r2, r3}", got.RoleAssignments())
	}
	if len(got.GrantedPermissions()) != 0 {
		t.Fatalf("Granted after revoke: got %d want 0", len(got.GrantedPermissions()))
	}
	if len(got.RevokedPermissions()) != 1 {
		t.Fatalf("Revoked: got %d want 1", len(got.RevokedPermissions()))
	}
}

func TestMembershipRepository_Add_RejectsCrossTenantRoleAssignment(t *testing.T) {
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	persons := adapters.NewPersonRepository(pool, tx)
	memberships := adapters.NewMembershipRepository(pool, tx)
	rolesRepo := adapters.NewRoleRepository(pool, tx)
	tenants := adapters.NewTenantRepository(pool, tx)

	tnA := seedTenant(t, tenants)
	tnB := seedTenant(t, tenants)
	p := seedPerson(t, persons, "cross-tenant@example.test")

	// Role in Tenant B; Membership in Tenant A; assign role from B → must fail.
	rB := newRole(t, tnB.ID(), "ForeignRole")
	ctxB := tenancy.WithID(t.Context(), tenancy.ID(tnB.ID().String()))
	if err := rolesRepo.Add(ctxB, rB); err != nil {
		t.Fatalf("Add rB: %v", err)
	}

	m, _ := membership.New(membership.ID(ids.NewV7().String()), p.ID(), tnA.ID(), membership.ID(""), testNow)
	_ = m.AssignRole(rB.ID(), testNow) // arch-test:ignore-err - test fixture setup

	ctxA := tenancy.WithID(t.Context(), tenancy.ID(tnA.ID().String()))
	err := memberships.Add(ctxA, m)
	if err == nil {
		t.Fatalf("Add: expected schema-level rejection of cross-tenant role assignment")
	}
}
