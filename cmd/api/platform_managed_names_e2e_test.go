//go:build integration

// arch-test:no-timeout-needed — one test over newE2EFixture, which bounds its own startup; per-request
// HTTP uses t.Context().

// arch-test:raw-sql-justified — plants STALE data (a managed name on a role and on an override) that the routes now refuse to write (G2m S1a); no typed helper can produce it.

// G2m S1 (ADR 0073 addendum): identity.roles.create/update/manage mean nothing outside the platform
// tenant, so nobody (LeadKart included) may put them on a company role or a company member's override,
// and a company can always take them off again.

package main

import (
	"net/http"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func TestE2E_PlatformManagedNames_NeverOnACompany_AlwaysRemovable(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "managednames")
	own := owner.AccessToken
	p := permission.IdentityPermissions
	hdr := scopeHdr(owner.TenantID)

	role := f.customRole(t, own, "Inert Names", 60)
	member := f.createMember(t, owner, "holder")
	f.assignRole(t, own, member.membershipID, role)
	super := f.mintPlatformTokenWith(t, nil, true)
	operator := f.mintPlatformTokenWith(t, []string{p.Platform.RolesManage}, false)
	rolePath := "/api/v1/roles/" + role
	overridesPath := "/api/v1/users/" + member.membershipID + "/permission-overrides"

	// S1a: LeadKart cannot confer the names on a company role or override.
	for _, n := range []string{p.Roles.Create, p.Roles.Update, p.Roles.Manage} {
		for who, tok := range map[string]string{"operator": operator, "super user": super} {
			f.wantForbidden(t, who+" grants "+n+" to a company role",
				f.doWithHeaders(t, http.MethodPost, rolePath+"/permissions/grant", tok, ports.RolePermissionRequest{Permission: n}, hdr),
				http.StatusForbidden, "grant_exceeds_authority")
			f.wantForbidden(t, who+" replaces a company role's permissions with "+n,
				f.doWithHeaders(t, http.MethodPut, rolePath+"/permissions", tok, ports.ReplaceRolePermissionsRequest{Permissions: []string{n}}, hdr),
				http.StatusForbidden, "grant_exceeds_authority")
			f.wantForbidden(t, who+" overrides "+n+" onto a company member",
				f.doWithHeaders(t, http.MethodPatch, overridesPath, tok, ports.ReplaceUserPermissionOverridesRequest{Granted: []string{n}, Revoked: []string{}}, hdr),
				http.StatusForbidden, "grant_exceeds_authority")
		}
	}

	// Plant the state an earlier build allowed: the role carries a name and the member holds one as an override.
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(t.Context()) }()
	if _, err := tx.Exec(t.Context(), `SELECT set_config('app.is_platform', 'true', true)`); err != nil {
		t.Fatalf("platform scope: %v", err)
	}
	if _, err := tx.Exec(t.Context(), `UPDATE identity.roles SET permissions = permissions || to_jsonb($1::text) WHERE id = $2 AND tenant_id = $3`,
		p.Roles.Update, role, owner.TenantID); err != nil {
		t.Fatalf("plant role permission: %v", err)
	}
	if _, err := tx.Exec(t.Context(), `INSERT INTO identity.membership_permission_overrides (membership_id, permission_name, kind, tenant_id, updated_at)
		VALUES ($1, $2, 'granted', $3, now())`, member.membershipID, p.Roles.Manage, owner.TenantID); err != nil {
		t.Fatalf("plant override: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// S1b: the company owner removes both.
	if r := f.doWithHeaders(t, http.MethodPatch, overridesPath, own, ports.ReplaceUserPermissionOverridesRequest{Granted: []string{}, Revoked: []string{}}, nil); r.status != http.StatusNoContent {
		t.Fatalf("the owner removes a managed-name override: %d %s, want 204", r.status, r.body)
	}
	if r := f.doWithHeaders(t, http.MethodDelete, "/api/v1/users/"+member.membershipID+"/roles/"+role, own, nil, nil); r.status != http.StatusNoContent {
		t.Fatalf("the owner revokes a role carrying a managed name: %d %s, want 204", r.status, r.body)
	}
}
