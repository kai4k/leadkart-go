//go:build integration

// arch-test:no-timeout-needed — one test over newE2EFixture, which bounds its own startup; per-request
// HTTP uses t.Context().

// arch-test:raw-sql-justified — plants STALE data (a managed name on a role and on an override) that the routes now refuse to write (G2m S1a); no typed helper can produce it.

// G2m S1 (ADR 0073 addendum): identity.roles.create/update/manage mean nothing outside the platform
// tenant, so nobody (LeadKart included) may put them on a company role or a company member's override,
// and a company can always take them off again.

package main

import (
	"encoding/json"
	"net/http"
	"slices"
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

	// G2n S1: a managed name does not land on a company member's REVOKED list either.
	for _, n := range []string{p.Roles.Create, p.Roles.Update, p.Roles.Manage} {
		for who, tok := range map[string]string{"owner": own, "operator": operator, "super user": super} {
			h := hdr
			if who == "owner" {
				h = nil
			}
			f.wantForbidden(t, who+" revokes "+n+" on a company member",
				f.doWithHeaders(t, http.MethodPatch, overridesPath, tok, ports.ReplaceUserPermissionOverridesRequest{Granted: []string{}, Revoked: []string{n}}, h),
				http.StatusForbidden, "grant_exceeds_authority")
		}
	}
	// An impersonation session confers nothing managed either.
	_, sess := f.openImpersonation(t, owner.TenantID)
	for _, req := range []ports.ReplaceUserPermissionOverridesRequest{
		{Granted: []string{p.Roles.Update}, Revoked: []string{}},
		{Granted: []string{}, Revoked: []string{p.Roles.Update}},
	} {
		if r := f.authedJSON(t, http.MethodPatch, overridesPath, sess.AccessToken, req); r.status != http.StatusForbidden {
			t.Errorf("an impersonation session confers a managed name %+v: %d %s, want 403", req, r.status, r.body)
		}
	}
	if st := readOverrides(t, f, own, member.membershipID); len(st.Granted) != 0 || len(st.Revoked) != 0 {
		t.Fatalf("the refused writes left state behind: %+v", st)
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
	// A second member with a STALE managed revoke (G2n S1).
	other := f.createMember(t, owner, "stale")
	otherPath := "/api/v1/users/" + other.membershipID + "/permission-overrides"
	if _, err := tx.Exec(t.Context(), `INSERT INTO identity.membership_permission_overrides (membership_id, permission_name, kind, tenant_id, updated_at)
		VALUES ($1, $2, 'revoked', $3, now())`, other.membershipID, p.Roles.Update, owner.TenantID); err != nil {
		t.Fatalf("plant revoke: %v", err)
	}
	if err := tx.Commit(t.Context()); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// Assigning a role that carries a managed name to a company member is refused (G2m M3).
	third := f.createMember(t, owner, "assignee")
	for who, tok := range map[string]string{"owner": own, "operator": operator, "super user": super, "impersonation": sess.AccessToken} {
		h := hdr
		if who == "owner" || who == "impersonation" {
			h = nil
		}
		r := f.doWithHeaders(t, http.MethodPost, "/api/v1/users/"+third.membershipID+"/roles", tok, ports.AssignUserRoleRequest{RoleID: role}, h)
		if r.status != http.StatusForbidden {
			t.Errorf("%s assigns a role carrying a managed name: %d %s, want 403", who, r.status, r.body)
		}
	}
	if ids := readUser(t, f, own, third.membershipID).RoleIDs; slices.Contains(ids, role) {
		t.Fatalf("the refused assign left the role on the member: %v", ids)
	}

	// S1b: the company owner removes both, and the state is read back each time.
	if r := f.doWithHeaders(t, http.MethodPatch, otherPath, own, ports.ReplaceUserPermissionOverridesRequest{Granted: []string{}, Revoked: []string{}}, nil); r.status != http.StatusNoContent {
		t.Fatalf("the owner clears a stale managed revoke: %d %s, want 204", r.status, r.body)
	}
	if st := readOverrides(t, f, own, other.membershipID); len(st.Granted) != 0 || len(st.Revoked) != 0 {
		t.Fatalf("the stale revoke is still there: %+v", st)
	}
	if r := f.doWithHeaders(t, http.MethodPatch, overridesPath, own, ports.ReplaceUserPermissionOverridesRequest{Granted: []string{}, Revoked: []string{}}, nil); r.status != http.StatusNoContent {
		t.Fatalf("the owner removes a managed-name override: %d %s, want 204", r.status, r.body)
	}
	if st := readOverrides(t, f, own, member.membershipID); len(st.Granted) != 0 || len(st.Revoked) != 0 {
		t.Fatalf("the managed override is still there: %+v", st)
	}
	if r := f.doWithHeaders(t, http.MethodDelete, "/api/v1/users/"+member.membershipID+"/roles/"+role, own, nil, nil); r.status != http.StatusNoContent {
		t.Fatalf("the owner revokes a role carrying a managed name: %d %s, want 204", r.status, r.body)
	}
	if ids := readUser(t, f, own, member.membershipID).RoleIDs; slices.Contains(ids, role) {
		t.Fatalf("the role is still on the member: %v", ids)
	}
}

func readUser(t *testing.T, f e2eFixture, token, membershipID string) ports.UserDto {
	t.Helper()
	r := f.do(t, http.MethodGet, "/api/v1/users/"+membershipID, token, nil)
	if r.status != http.StatusOK {
		t.Fatalf("read user: %d %s", r.status, r.body)
	}
	var u ports.UserDto
	if err := json.Unmarshal(r.body, &u); err != nil {
		t.Fatalf("decode user: %v", err)
	}
	return u
}

func readOverrides(t *testing.T, f e2eFixture, token, membershipID string) ports.UserPermissionOverridesDto {
	t.Helper()
	u := readUser(t, f, token, membershipID)
	if u.PermissionOverrides == nil {
		t.Fatalf("overrides absent for the owner read of %s", membershipID)
	}
	return *u.PermissionOverrides
}
