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
// lives in roletest.FakeRepository unit tests):
//
//   - JSONB permission-list round-trip via Postgres driver (Add → GetByID
//     hydrates Permissions intact).
//   - SQLSTATE 23505 → role.ErrNameTaken translation via the partial
//     unique index uq_roles_tenant_name WHERE NOT is_deleted.
//   - RLS policy enforcement — cross-tenant reads return ErrNotFound
//     even when the row exists; GetByIDs filters cross-tenant IDs.
//   - Soft-delete partial-index semantics — soft-deleted roles vanish
//     from GetByID under the live `WHERE NOT is_deleted` filter.
//   - pgx ROLLBACK on UpdateByID closure error (real tx rollback, not
//     fake in-memory revert).
//
// arch-test:raw-sql-justified — TestRoleRepository_UpdateByID_Delete_
//   PersistsSoftDeleteAndHidesFromGetByID intentionally bypasses the
//   adapter with a direct SELECT to assert the PHYSICAL row state
//   (`is_deleted=true`) after Delete, proving soft-vs-hard delete at
//   the SQL layer. Per ADR 0062 §6 canonical SQL-contract test shape
//   (matches the rolehierarchy soft-delete sharpening in commit
//   85bb585).

package adapters_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // pgx driver for database/sql owner-DSN RLS-bypass

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
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
		false, // not super-admin,
		testNow,
	)
	if err != nil {
		t.Fatalf("role.New: %v", err)
	}
	view := permission.FromConstant(permission.IdentityPermissions.Roles.View)
	if err := r.GrantPermission(view, testNow); err != nil {
		t.Fatalf("GrantPermission: %v", err)
	}
	return r
}

// SQL-contract: JSONB permission-list survives Marshal/Unmarshal through
// the pgx driver. Business-rule round-trip (id/name/hierarchy) covered
// by roletest.FakeRepository.
func TestRoleRepository_Add_PersistsPermissionsJSONBRoundTrip(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tenants := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))

	tn := seedTenant(t, tenants)
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))

	r := newRole(t, tn.ID(), "Sales")
	if err := roles.Add(ctx, r); err != nil {
		t.Fatalf("Add: %v", err)
	}

	got, err := roles.GetByID(ctx, tn.ID(), r.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if len(got.Permissions()) != 1 {
		t.Fatalf("permissions: got %d want 1", len(got.Permissions()))
	}
	if got.Permissions()[0].Name() != permission.IdentityPermissions.Roles.View {
		t.Fatalf("permission name: got %q want %q",
			got.Permissions()[0].Name(), permission.IdentityPermissions.Roles.View)
	}
}

// SQL-contract: SQLSTATE 23505 from the partial unique index
// uq_roles_tenant_name WHERE NOT is_deleted is translated to
// role.ErrNameTaken by the adapter.
func TestRoleRepository_Add_ReturnsErrNameTaken_OnDuplicateLiveName(t *testing.T) {
	t.Parallel()
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

// SQL-contract: RLS policy enforcement — Tenant A inserts a role;
// Tenant B's connection scope MUST see zero rows. FORCE RLS + the
// non-superuser role together close the door even on direct GetByID.
// This is the canonical RLS proof for the file.
func TestRoleRepository_RLS_IsolatesCrossTenantReads(t *testing.T) {
	t.Parallel()
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
	_, err := roles.GetByID(ctxB, tnB.ID(), rA.ID())
	if !errors.Is(err, role.ErrNotFound) {
		t.Fatalf("RLS leak: tenant B saw tenant A's role: %v", err)
	}
}

// SQL-contract: GetByIDs (`WHERE id = ANY($1)`) applies RLS — IDs that
// belong to other tenants are silently dropped, not surfaced as error.
// Empty input returns empty slice without driver round-trip.
func TestRoleRepository_GetByIDs_FiltersCrossTenant(t *testing.T) {
	t.Parallel()
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
	got, err := roles.GetByIDs(ctxA, tnA.ID(), []role.ID{rA1.ID(), rA2.ID(), rB.ID()})
	if err != nil {
		t.Fatalf("GetByIDs: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("GetByIDs (cross-tenant): got %d want 2 (B should be hidden)", len(got))
	}
}

// SQL-contract: Delete is SOFT-delete (physical row survives, only
// `is_deleted` flips to true) AND the partial-index + read predicate
// `WHERE NOT is_deleted` hides it from the live read path. Two halves:
//
//   1. Physical row stays — direct SELECT bypassing the adapter
//      proves the UPDATE didn't DELETE.
//   2. The partial-index + WHERE NOT is_deleted predicate on the
//      read path means GetByID can't see it.
//
// The roletest.FakeRepository's "Delete → GetByID returns ErrNotFound"
// observable is identical; only the SQL test proves the PG-specific
// mechanism (soft vs. hard delete + partial-index filter) actually
// fires. Canonical post-rolehierarchy sharpening pattern per ADR 0062.
func TestRoleRepository_UpdateByID_Delete_PersistsSoftDeleteAndHidesFromGetByID(t *testing.T) {
	t.Parallel()
	pool := repoFixture(t)
	tenants := adapters.NewTenantRepository(pool, pg.NewTransactor(pool))
	roles := adapters.NewRoleRepository(pool, pg.NewTransactor(pool))

	tn := seedTenant(t, tenants)
	ctx := tenancy.WithID(t.Context(), tenancy.ID(tn.ID().String()))
	r := newRole(t, tn.ID(), "Sales")
	if err := roles.Add(ctx, r); err != nil {
		t.Fatalf("Add: %v", err)
	}

	err := roles.UpdateByID(ctx, tn.ID(), r.ID(), func(loaded *role.Role) (bool, error) {
		return true, loaded.Delete("admin@example.test", time.Now().UTC())
	})
	if err != nil {
		t.Fatalf("UpdateByID delete: %v", err)
	}

	// SQL-contract part 1: physical row survives the soft delete.
	// Direct SELECT via the OWNER DSN — bypasses RLS so the SELECT can
	// see the soft-deleted row regardless of tenant scope (the app
	// pool's RLS would filter it out by partial-index predicate).
	ownerDB, openErr := sql.Open("pgx", sharedPG.OwnerDSN())
	if openErr != nil {
		t.Fatalf("open owner DB: %v", openErr)
	}
	defer func() { _ = ownerDB.Close() }()
	var isDeleted bool
	if err := ownerDB.QueryRowContext(t.Context(),
		`SELECT is_deleted FROM identity.roles WHERE id = $1`,
		r.ID().String(),
	).Scan(&isDeleted); err != nil {
		t.Fatalf("direct SELECT for physical row: %v", err)
	}
	if !isDeleted {
		t.Fatal("physical row's is_deleted is false — Delete behaved as hard-delete, contract broken")
	}

	// SQL-contract part 2: partial index + `WHERE NOT is_deleted`
	// predicate hides the soft-deleted row from GetByID.
	_, err = roles.GetByID(ctx, tn.ID(), r.ID())
	if !errors.Is(err, role.ErrNotFound) {
		t.Fatalf("GetByID after delete: got %v want ErrNotFound (partial-index filter)", err)
	}
}

// SQL-contract: real pgx tx ROLLBACK on closure error — the in-memory
// fake can't prove that the underlying Postgres transaction actually
// rolled back. Asserts the post-rollback row is the pre-update state
// fetched from disk.
func TestRoleRepository_UpdateByID_Rollback_WhenUpdateFnErrors(t *testing.T) {
	t.Parallel()
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
	err := roles.UpdateByID(ctx, tn.ID(), r.ID(), func(loaded *role.Role) (bool, error) {
		_ = loaded.Rename("ShouldRollBack", testNow) // arch-test:ignore-err - test fixture setup
		return true, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("UpdateByID error: got %v want sentinel", err)
	}

	got, err := roles.GetByID(ctx, tn.ID(), r.ID())
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if got.Name() != "Sales" {
		t.Fatalf("name after rollback: got %q want Sales", got.Name())
	}
}

// Hierarchy integration tests moved to
// role_hierarchy_edges_pg_test.go per ADR 0058 (Wave 9.4) — the
// edge aggregate owns parent→child relationships now.
