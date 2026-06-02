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
// SQL-CONTRACT COVERAGE for this file (ADR 0062 — adapter integration
// tests are SQL-contract-only; business-rule + state-machine coverage
// lives in membershiptest.FakeRepository unit tests):
//
//   - SQLSTATE 23505 from the partial unique index uq_memberships_person_
//     active (WHERE status='active' AND NOT is_deleted) → ErrAlreadyActive,
//     and the partial-index slot is RELEASED when the row flips out of
//     'active' (Deactivate frees the slot for a new Active in another
//     tenant).
//   - RLS policy enforcement — cross-tenant GetByID returns ErrNotFound.
//   - GetActiveForPerson runs under BYPASSRLS (platform scope, no
//     tenant GUC bound) so the login flow can resolve a person's active
//     tenant before the tenant ctx exists.
//   - Multi-table write in same tx: role_assignments + permission_overrides
//     + profile child-table rows survive Add → GetByID hydration.
//   - Composite FK fk_role_assignments_same_tenant rejects cross-tenant
//     role IDs (declarative cross-tenant safety per ADR 0058).
//
// arch-test:raw-sql-justified — TestMembershipRepository_UpdateByID_
//   DeactivateClearsActiveSlot intentionally bypasses the adapter
//   with a direct SELECT to assert the PHYSICAL row state
//   (status='inactive') after Deactivate, proving the UPDATE rewrote
//   the column that the partial-index predicate filters on. Per ADR
//   0062 §6 canonical SQL-contract shape (matches rolehierarchy +
//   role soft-delete sharpenings).

package adapters_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // pgx driver for database/sql owner-DSN RLS-bypass

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// testNow is the deterministic clock used by all test fixtures in this package.
var testNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

// seedTenant and seedPerson are shared test helpers wired the same way
// the production composition root wires them.
func seedTenant(t *testing.T, repo *adapters.TenantRepository) *tenant.Tenant {
	t.Helper()
	id := tenant.ID(ids.NewV7().String())
	// UUIDv7 leading chars are timestamp-derived; use the trailing random
	// portion to avoid slug collisions on rapid test starts.
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

// SQL-contract: SQLSTATE 23505 from uq_memberships_person_active blocks
// a second Active row for the same Person across any tenant.
func TestMembershipRepository_Add_SecondActive_ReturnsErrAlreadyActive(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	tenants := adapters.NewTenantRepository(pool, tx)
	persons := adapters.NewPersonRepository(pool, tx)
	memberships := adapters.NewMembershipRepository(pool, tx)

	tnA := seedTenant(t, tenants)
	tnB := seedTenant(t, tenants)
	p := seedPerson(t, persons, "switch@example.test")

	// Active in tenant A.
	mA, _ := membership.New(membership.ID(ids.NewV7().String()), p.ID(), tnA.ID(), membership.ID(""), testNow)
	ctxA := tenancy.WithID(t.Context(), tenancy.ID(tnA.ID().String()))
	if err := memberships.Add(ctxA, mA); err != nil {
		t.Fatalf("first Add: %v", err)
	}

	// Second Active in tenant B — partial unique index blocks it.
	mB, _ := membership.New(membership.ID(ids.NewV7().String()), p.ID(), tnB.ID(), membership.ID(""), testNow)
	ctxB := tenancy.WithID(t.Context(), tenancy.ID(tnB.ID().String()))
	err := memberships.Add(ctxB, mB)
	if !errors.Is(err, membership.ErrAlreadyActive) {
		t.Fatalf("expected ErrAlreadyActive, got %v", err)
	}
}

// SQL-contract: RLS isolation — cross-tenant GetByID returns ErrNotFound.
func TestMembershipRepository_GetByID_OutsideTenantScope_NotFound(t *testing.T) {
	t.Parallel()
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

	// Tenant B's scope — RLS hides the row.
	ctxB := tenancy.WithID(t.Context(), tenancy.ID(tnB.ID().String()))
	_, err := memberships.GetByID(ctxB, tnB.ID(), mA.ID())
	if !errors.Is(err, membership.ErrNotFound) {
		t.Fatalf("expected ErrNotFound (RLS isolation), got %v", err)
	}
}

// SQL-contract: uq_memberships_person_active releases its slot when
// status flips out of 'active'. Two halves:
//
//  1. Physical row state via adapter-bypass SELECT: UPDATE rewrote
//     status column (not a soft-delete), freeing the partial-index slot.
//  2. Slot release admits a second Active for the same Person.
func TestMembershipRepository_UpdateByID_DeactivateClearsActiveSlot(t *testing.T) {
	t.Parallel()
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
	err := memberships.UpdateByID(ctxA, tnA.ID(), mA.ID(), func(m *membership.Membership) (bool, error) {
		if err := m.Deactivate("job change", testNow); err != nil {
			return false, err
		}
		return true, nil
	})
	if err != nil {
		t.Fatalf("Deactivate: %v", err)
	}

	// SQL-contract part 1: direct SELECT via OWNER DSN proves the UPDATE
	// wrote status='inactive' (freeing the partial-index slot).
	ownerDB, openErr := sql.Open("pgx", sharedPG.OwnerDSN())
	if openErr != nil {
		t.Fatalf("open owner DB: %v", openErr)
	}
	defer func() { _ = ownerDB.Close() }()
	var status string
	if err := ownerDB.QueryRowContext(t.Context(),
		`SELECT status FROM identity.tenant_memberships WHERE id = $1`,
		mA.ID().String(),
	).Scan(&status); err != nil {
		t.Fatalf("direct SELECT for physical row: %v", err)
	}
	if status != "inactive" {
		t.Fatalf("physical row status: got %q want %q — Deactivate didn't update the status column", status, "inactive")
	}

	// SQL-contract part 2: slot release admits a new Active in another tenant.
	mB, _ := membership.New(membership.ID(ids.NewV7().String()), p.ID(), tnB.ID(), membership.ID(""), testNow)
	ctxB := tenancy.WithID(t.Context(), tenancy.ID(tnB.ID().String()))
	if err := memberships.Add(ctxB, mB); err != nil {
		t.Fatalf("Add B after deactivate (partial-index slot release): %v", err)
	}
}

// SQL-contract: GetActiveForPerson runs without a tenant GUC bound;
// the adapter opens under BYPASSRLS so it can scan across tenants.
func TestMembershipRepository_GetActiveForPerson_BypassesRLS(t *testing.T) {
	t.Parallel()
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

	// Login flow: ctx has no tenant — GetActiveForPerson resolves it.
	got, err := memberships.GetActiveForPerson(t.Context(), p.ID())
	if err != nil {
		t.Fatalf("GetActiveForPerson: %v", err)
	}
	if got.ID() != mA.ID() {
		t.Fatalf("id round-trip: got %q want %q", got.ID(), mA.ID())
	}
}

// SQL-contract: Add writes role_assignments, permission_overrides, and
// profile in the same transaction as the membership row; GetByID hydrates
// all child tables.
func TestMembershipRepository_Add_PersistsChildTablesInSameTx(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	persons := adapters.NewPersonRepository(pool, tx)
	memberships := adapters.NewMembershipRepository(pool, tx)
	rolesRepo := adapters.NewRoleRepository(pool, tx)
	tenants := adapters.NewTenantRepository(pool, tx)

	tn := seedTenant(t, tenants)
	p := seedPerson(t, persons, "task18-add@example.test")

	// Composite FK requires real role IDs in the same tenant.
	r1 := newRole(t, tn.ID(), "Sales")
	r2 := newRole(t, tn.ID(), "Manager")
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))
	if err := rolesRepo.Add(ctx, r1); err != nil {
		t.Fatalf("Add r1: %v", err)
	}
	if err := rolesRepo.Add(ctx, r2); err != nil {
		t.Fatalf("Add r2: %v", err)
	}

	// Membership with role assignments, a granted override, a revoked override,
	// and non-empty profile fields.
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

	// GetByID proves the SQL fan-out write+read round-trip is intact.
	// State-machine semantics are covered by membershiptest.FakeRepository.
	got, err := memberships.GetByID(ctx, tn.ID(), m.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if len(got.RoleAssignments()) != 2 {
		t.Fatalf("RoleAssignments hydrated: got %d want 2", len(got.RoleAssignments()))
	}
	if len(got.GrantedPermissions()) != 1 {
		t.Fatalf("GrantedPermissions hydrated: got %d want 1", len(got.GrantedPermissions()))
	}
	if len(got.RevokedPermissions()) != 1 {
		t.Fatalf("RevokedPermissions hydrated: got %d want 1", len(got.RevokedPermissions()))
	}
	if got.Designation() == "" || got.Department() == "" || got.StatusMessage() == "" {
		t.Fatalf("profile columns not hydrated: %q %q %q",
			got.Designation(), got.Department(), got.StatusMessage())
	}
}

// SQL-contract: composite FK fk_role_assignments_same_tenant rejects a
// role_id from another tenant (declarative cross-tenant safety, ADR 0058).
func TestMembershipRepository_Add_RejectsCrossTenantRoleAssignment(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tx := pg.NewTransactor(pool)
	persons := adapters.NewPersonRepository(pool, tx)
	memberships := adapters.NewMembershipRepository(pool, tx)
	rolesRepo := adapters.NewRoleRepository(pool, tx)
	tenants := adapters.NewTenantRepository(pool, tx)

	tnA := seedTenant(t, tenants)
	tnB := seedTenant(t, tenants)
	p := seedPerson(t, persons, "cross-tenant@example.test")

	// Role from Tenant B assigned to Membership in Tenant A — must fail.
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
