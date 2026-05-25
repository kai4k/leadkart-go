// Package seedtest holds typed read-side helpers for the bootstrap
// integration test (cmd/bootstrap/seed_integration_test.go).
//
// Why this package exists: see audittest + messagingtest +
// identitytest — the broader rationale is "tests get the same
// typed-helper discipline as production (sqlc + adapters); no raw
// SQL in test files".
//
// The bootstrap test reads from a *sql.DB (rather than pgxpool)
// because cmd/bootstrap itself runs as the database owner via
// database/sql + the goose driver — these helpers mirror that
// shape so the test doesn't have to swap connection types just to
// verify state.
//
// The companion arch test TestArch_NoRawSQLInTests enforces it.
package seedtest

import (
	"context"
	"database/sql"
	"testing"
)

// GetTenantBySlug returns the tenant row identified by slug. Used
// by the bootstrap test to verify the seed wrote the canonical
// platform-tenant shape.
func GetTenantBySlug(t testing.TB, ctx context.Context, db *sql.DB, slug string) (id, gotSlug, status, legalName string) {
	t.Helper()
	const q = `
		SELECT id, slug, status, legal_name
		FROM   identity.tenants WHERE slug = $1
	`
	if err := db.QueryRowContext(ctx, q, slug).Scan(&id, &gotSlug, &status, &legalName); err != nil {
		t.Fatalf("seedtest.GetTenantBySlug(%s): %v", slug, err)
	}
	return id, gotSlug, status, legalName
}

// CountTenantsBySlug returns the number of identity.tenants rows
// matching the supplied slug. Used by the re-seed idempotency
// assertion ("re-run leaves row count at 1").
func CountTenantsBySlug(t testing.TB, ctx context.Context, db *sql.DB, slug string) int64 {
	t.Helper()
	var n int64
	const q = `SELECT count(*) FROM identity.tenants WHERE slug = $1`
	if err := db.QueryRowContext(ctx, q, slug).Scan(&n); err != nil {
		t.Fatalf("seedtest.CountTenantsBySlug(%s): %v", slug, err)
	}
	return n
}

// GetSeededSuperAdminPerson returns the identity.persons row for
// the seeded super-admin email. Used by the bootstrap test to
// verify Argon2 hash + first/last name persisted.
func GetSeededSuperAdminPerson(t testing.TB, ctx context.Context, db *sql.DB, email string) (id, gotEmail, passwordHash, firstName, lastName string) {
	t.Helper()
	const q = `
		SELECT id, email, password_hash, first_name, last_name
		FROM   identity.persons WHERE email = $1
	`
	if err := db.QueryRowContext(ctx, q, email).Scan(&id, &gotEmail, &passwordHash, &firstName, &lastName); err != nil {
		t.Fatalf("seedtest.GetSeededSuperAdminPerson(%s): %v", email, err)
	}
	return id, gotEmail, passwordHash, firstName, lastName
}

// GetSuperAdminRole returns the (id, name, is_super_admin,
// is_system_default) tuple for the super-admin role under the
// supplied tenant + role-name.
func GetSuperAdminRole(t testing.TB, ctx context.Context, db *sql.DB, tenantID, roleName string) (id, name string, isSuperAdmin, isSystemDefault bool) {
	t.Helper()
	const q = `
		SELECT id, name, is_super_admin, is_system_default
		FROM   identity.roles
		WHERE  tenant_id = $1 AND name = $2 AND NOT is_deleted
	`
	if err := db.QueryRowContext(ctx, q, tenantID, roleName).Scan(&id, &name, &isSuperAdmin, &isSystemDefault); err != nil {
		t.Fatalf("seedtest.GetSuperAdminRole(%s, %s): %v", tenantID, roleName, err)
	}
	return id, name, isSuperAdmin, isSystemDefault
}

// CountSuperAdminRoles returns the number of identity.roles rows
// flagged as super-admin (excluding soft-deleted). Used by the
// row-count assertion ("seed wrote exactly one super-admin role").
func CountSuperAdminRoles(t testing.TB, ctx context.Context, db *sql.DB) int64 {
	t.Helper()
	var n int64
	const q = `SELECT count(*) FROM identity.roles WHERE is_super_admin AND NOT is_deleted`
	if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		t.Fatalf("seedtest.CountSuperAdminRoles: %v", err)
	}
	return n
}

// GetMembershipForPerson returns the (id, status) for the membership
// row linking the supplied (personID, tenantID) pair.
func GetMembershipForPerson(t testing.TB, ctx context.Context, db *sql.DB, personID, tenantID string) (id, status string) {
	t.Helper()
	const q = `
		SELECT id, status FROM identity.tenant_memberships
		WHERE  person_id = $1 AND tenant_id = $2
	`
	if err := db.QueryRowContext(ctx, q, personID, tenantID).Scan(&id, &status); err != nil {
		t.Fatalf("seedtest.GetMembershipForPerson(%s, %s): %v", personID, tenantID, err)
	}
	return id, status
}

// CountRoleAssignmentsForMembershipAndRole returns the number of
// identity.role_assignments rows linking the supplied
// (membershipID, roleID). Used to confirm the role-assignment seed
// landed exactly once.
func CountRoleAssignmentsForMembershipAndRole(t testing.TB, ctx context.Context, db *sql.DB, membershipID, roleID string) int64 {
	t.Helper()
	var n int64
	const q = `
		SELECT count(*) FROM identity.role_assignments
		WHERE  membership_id = $1 AND role_id = $2
	`
	if err := db.QueryRowContext(ctx, q, membershipID, roleID).Scan(&n); err != nil {
		t.Fatalf("seedtest.CountRoleAssignmentsForMembershipAndRole(%s, %s): %v", membershipID, roleID, err)
	}
	return n
}

// CountAllPersons returns the total identity.persons row count.
func CountAllPersons(t testing.TB, ctx context.Context, db *sql.DB) int64 {
	t.Helper()
	var n int64
	const q = `SELECT count(*) FROM identity.persons`
	if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		t.Fatalf("seedtest.CountAllPersons: %v", err)
	}
	return n
}

// CountAllMemberships returns the total identity.tenant_memberships
// row count.
func CountAllMemberships(t testing.TB, ctx context.Context, db *sql.DB) int64 {
	t.Helper()
	var n int64
	const q = `SELECT count(*) FROM identity.tenant_memberships`
	if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		t.Fatalf("seedtest.CountAllMemberships: %v", err)
	}
	return n
}

// CountAllRoleAssignments returns the total identity.role_assignments
// row count.
func CountAllRoleAssignments(t testing.TB, ctx context.Context, db *sql.DB) int64 {
	t.Helper()
	var n int64
	const q = `SELECT count(*) FROM identity.role_assignments`
	if err := db.QueryRowContext(ctx, q).Scan(&n); err != nil {
		t.Fatalf("seedtest.CountAllRoleAssignments: %v", err)
	}
	return n
}
