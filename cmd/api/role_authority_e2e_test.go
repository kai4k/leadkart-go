//go:build integration

// arch-test:no-timeout-needed — newE2EFixtureAt → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally + testcontainers wait strategy bounds
// container startup; per-request HTTP uses t.Context() (auto-canceled on test end).

// ADR 0069 A5: every role command decides through the authority policy under
// the tenant lock. One scenario per forbidden case, through the real mux; each
// refused call must also leave the role exactly as it was.

package main

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func (f e2eFixture) createRoleVia(t *testing.T, token string, body any) (httpResp, string) {
	t.Helper()
	r := f.authedJSON(t, http.MethodPost, "/api/v1/roles", token, body)
	var out ports.CreateRoleResponse
	if r.status == http.StatusCreated {
		if err := json.Unmarshal(r.body, &out); err != nil {
			t.Fatalf("decode create role: %v", err)
		}
	}
	return r, out.RoleID
}

func (f e2eFixture) roleDto(t *testing.T, token, id string) ports.RoleDto {
	t.Helper()
	r := f.authedJSON(t, http.MethodGet, "/api/v1/roles/"+id, token, nil)
	var dto ports.RoleDto
	if r.status != http.StatusOK || json.Unmarshal(r.body, &dto) != nil {
		t.Fatalf("get role %s: %d %s", id, r.status, r.body)
	}
	return dto
}

// grantOverrides gives a member route permissions by override (the owner may).
func (f e2eFixture) grantOverrides(t *testing.T, ownerTok, membershipID string, granted, revoked []string) {
	t.Helper()
	if revoked == nil {
		revoked = []string{}
	}
	if r := f.authedJSON(t, http.MethodPatch, "/api/v1/users/"+membershipID+"/permission-overrides", ownerTok,
		ports.ReplaceUserPermissionOverridesRequest{Granted: granted, Revoked: revoked}); r.status != http.StatusNoContent {
		t.Fatalf("overrides for %s: %d %s", membershipID, r.status, r.body)
	}
}

func TestE2E_RoleCommands_PolicyDecides(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	owner := f.registerAndLogin(t, "roles")
	p := permission.IdentityPermissions
	roleID := func(name string) string { return f.roleIDByName(t, owner.AccessToken, name) }
	ownerRole := roleID(role.SystemRoles.Tenant.CompanyOwner)

	adminM := f.createMember(t, owner, "admin")
	f.assignRole(t, owner.AccessToken, adminM.membershipID, roleID(role.SystemRoles.Tenant.Administrator))
	mgrM := f.createMember(t, owner, "mgr")
	f.assignRole(t, owner.AccessToken, mgrM.membershipID, roleID(role.SystemRoles.Tenant.SalesManager))
	f.grantOverrides(t, owner.AccessToken, mgrM.membershipID, []string{p.Roles.Create, p.Roles.Update, p.Roles.Manage}, nil)
	weakM := f.createMember(t, owner, "weakadmin") // an Administrator who lost tenant.admin
	f.assignRole(t, owner.AccessToken, weakM.membershipID, roleID(role.SystemRoles.Tenant.Administrator))
	f.grantOverrides(t, owner.AccessToken, weakM.membershipID, []string{p.Roles.Update, p.Roles.Manage}, []string{p.Meta.TenantAdmin})
	adminTok := f.loginToken(t, adminM.email, adminM.password)
	mgrTok := f.loginToken(t, mgrM.email, mgrM.password)
	weakTok := f.loginToken(t, weakM.email, weakM.password)
	superTok := f.mintPlatformTokenWith(t, nil, true)

	rolePath := func(id, suffix string) string { return "/api/v1/roles/" + id + suffix }
	ownerBefore := f.roleDto(t, owner.AccessToken, ownerRole)
	assertOwnerRoleIntact := func(t *testing.T) {
		t.Helper()
		got := f.roleDto(t, owner.AccessToken, ownerRole)
		if !slices.Equal(got.Permissions, ownerBefore.Permissions) || got.Name != ownerBefore.Name || got.HierarchyLevel != ownerBefore.HierarchyLevel {
			t.Fatalf("the Company Owner role changed: %+v -> %+v", ownerBefore, got)
		}
	}

	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("system role: an Administrator cannot strip tenant.admin from Company Owner", func(t *testing.T) {
		r := f.authedJSON(t, http.MethodPost, rolePath(ownerRole, "/permissions/revoke"), adminTok, ports.RolePermissionRequest{Permission: p.Meta.TenantAdmin})
		f.wantForbidden(t, "admin revokes tenant.admin from Company Owner", r, http.StatusConflict, "role_system_default")
		assertOwnerRoleIntact(t)
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("system role: emptying it is refused, with {} and with []", func(t *testing.T) {
		r := f.authedJSON(t, http.MethodPut, rolePath(ownerRole, "/permissions"), adminTok, map[string]any{})
		f.wantForbidden(t, "PUT {} on Company Owner", r, http.StatusBadRequest, "invalid_body")
		r = f.authedJSON(t, http.MethodPut, rolePath(ownerRole, "/permissions"), adminTok, map[string]any{"permissions": nil})
		f.wantForbidden(t, "PUT null on Company Owner", r, http.StatusBadRequest, "invalid_body")
		r = f.authedJSON(t, http.MethodPut, rolePath(ownerRole, "/permissions"), adminTok, ports.ReplaceRolePermissionsRequest{Permissions: []string{}})
		f.wantForbidden(t, "PUT [] on Company Owner", r, http.StatusConflict, "role_system_default")
		assertOwnerRoleIntact(t)
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("system role: a super user may not edit, re-level, deactivate or re-parent it either", func(t *testing.T) {
		hdr := scopeHdr(owner.TenantID)
		for name, c := range map[string]struct {
			method, path string
			body         any
		}{
			"rename":     {http.MethodPatch, rolePath(ownerRole, ""), map[string]any{"name": "Renamed Owner"}},
			"re-level":   {http.MethodPatch, rolePath(ownerRole, ""), map[string]any{"hierarchy_level": 40}},
			"deactivate": {http.MethodPost, rolePath(ownerRole, "/deactivate"), nil},
			"grant":      {http.MethodPost, rolePath(ownerRole, "/permissions/grant"), ports.RolePermissionRequest{Permission: p.Crm.Leads.Read}},
			"revoke":     {http.MethodPost, rolePath(ownerRole, "/permissions/revoke"), ports.RolePermissionRequest{Permission: p.Meta.TenantAdmin}},
			"replace":    {http.MethodPut, rolePath(ownerRole, "/permissions"), ports.ReplaceRolePermissionsRequest{Permissions: []string{}}},
			"set parent": {http.MethodPatch, rolePath(ownerRole, "/parent"), map[string]any{"parent_role_id": nil}},
		} {
			r := f.doWithHeaders(t, c.method, c.path, superTok, c.body, hdr)
			f.wantForbidden(t, "super user "+name, r, http.StatusConflict, "role_system_default")
		}
		assertOwnerRoleIntact(t)
	})

	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("create: level is required, in [1, 99], strictly below the creator", func(t *testing.T) {
		r, _ := f.createRoleVia(t, mgrTok, map[string]any{"name": "No Level Role"})
		f.wantForbidden(t, "create without hierarchy_level", r, http.StatusBadRequest, "invalid_body")
		r, _ = f.createRoleVia(t, mgrTok, map[string]any{"name": "Zero Level", "hierarchy_level": 0})
		f.wantForbidden(t, "create at level 0", r, http.StatusUnprocessableEntity, "role_invalid")
		r, _ = f.createRoleVia(t, mgrTok, map[string]any{"name": "Level 100", "hierarchy_level": 100})
		f.wantForbidden(t, "create at level 100", r, http.StatusUnprocessableEntity, "role_invalid")
		r, _ = f.createRoleVia(t, mgrTok, ports.CreateRoleRequest{Name: "Peer Manager", HierarchyLevel: role.HierarchyLevelManager})
		f.wantForbidden(t, "manager creates at their own level", r, http.StatusForbidden, "insufficient_rank")
		r, _ = f.createRoleVia(t, mgrTok, ports.CreateRoleRequest{Name: "Boss Role", HierarchyLevel: role.HierarchyLevelAdmin})
		f.wantForbidden(t, "manager creates above themselves", r, http.StatusForbidden, "insufficient_rank")
		r, id := f.createRoleVia(t, mgrTok, ports.CreateRoleRequest{Name: "Field Rep", HierarchyLevel: 60})
		if r.status != http.StatusCreated || id == "" {
			t.Fatalf("manager creates a role below themselves: %d %s", r.status, r.body)
		}
	})

	custom := func(t *testing.T, name string, level int, perms ...string) string {
		t.Helper()
		r, id := f.createRoleVia(t, adminTok, ports.CreateRoleRequest{Name: name, HierarchyLevel: level})
		if r.status != http.StatusCreated {
			t.Fatalf("create %s: %d %s", name, r.status, r.body)
		}
		if len(perms) > 0 {
			if r := f.authedJSON(t, http.MethodPut, rolePath(id, "/permissions"), adminTok, ports.ReplaceRolePermissionsRequest{Permissions: perms}); r.status != http.StatusNoContent {
				t.Fatalf("seed permissions of %s: %d %s", name, r.status, r.body)
			}
		}
		return id
	}

	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("update: re-levelling above yourself is refused, and so is level 0", func(t *testing.T) {
		id := custom(t, "Relevel Me", 60)
		r := f.authedJSON(t, http.MethodPatch, rolePath(id, ""), mgrTok, map[string]any{"hierarchy_level": role.HierarchyLevelAdmin})
		f.wantForbidden(t, "manager re-levels above themselves", r, http.StatusForbidden, "insufficient_rank")
		r = f.authedJSON(t, http.MethodPatch, rolePath(id, ""), mgrTok, map[string]any{"hierarchy_level": 0})
		f.wantForbidden(t, "manager re-levels to 0", r, http.StatusUnprocessableEntity, "role_invalid")
		if got := f.roleDto(t, owner.AccessToken, id).HierarchyLevel; got != 60 {
			t.Errorf("level = %d after refused re-levels, want 60", got)
		}
		r = f.authedJSON(t, http.MethodPatch, rolePath(id, ""), mgrTok, map[string]any{"hierarchy_level": 70, "name": "Relevelled"})
		if r.status != http.StatusNoContent {
			t.Errorf("manager re-levels below themselves: %d %s", r.status, r.body)
		}
		dup := custom(t, "Duplicate Source", 60)
		r = f.authedJSON(t, http.MethodPatch, rolePath(dup, ""), mgrTok, map[string]any{"name": "Relevelled"})
		f.wantForbidden(t, "rename onto a taken name", r, http.StatusConflict, "role_name_taken")
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("permissions: a manager confers and removes only what they hold", func(t *testing.T) {
		id := custom(t, "Perm Target", 60, p.Crm.Leads.Read, p.Dispatch.ConsignmentNotes.Manage)
		r := f.authedJSON(t, http.MethodPost, rolePath(id, "/permissions/grant"), mgrTok, ports.RolePermissionRequest{Permission: p.Dispatch.ConsignmentNotes.Read})
		f.wantForbidden(t, "grant unheld", r, http.StatusForbidden, "grant_exceeds_authority")
		r = f.authedJSON(t, http.MethodPost, rolePath(id, "/permissions/revoke"), mgrTok, ports.RolePermissionRequest{Permission: p.Dispatch.ConsignmentNotes.Manage})
		f.wantForbidden(t, "revoke unheld", r, http.StatusForbidden, "grant_exceeds_authority")
		r = f.authedJSON(t, http.MethodPut, rolePath(id, "/permissions"), mgrTok, ports.ReplaceRolePermissionsRequest{Permissions: []string{}})
		f.wantForbidden(t, "empty a role that carries an unheld permission", r, http.StatusForbidden, "grant_exceeds_authority")
		r = f.authedJSON(t, http.MethodPut, rolePath(id, "/permissions"), mgrTok, ports.ReplaceRolePermissionsRequest{Permissions: []string{p.Tenants.Suspend}})
		f.wantForbidden(t, "confer a platform-reserved name", r, http.StatusForbidden, "grant_exceeds_authority")
		if got := f.roleDto(t, owner.AccessToken, id).Permissions; len(got) != 2 {
			t.Errorf("a refused call changed the role: %v", got)
		}
		r = f.authedJSON(t, http.MethodPost, rolePath(id, "/permissions/grant"), mgrTok, ports.RolePermissionRequest{Permission: p.Crm.Leads.ReadAll})
		if r.status != http.StatusNoContent {
			t.Errorf("grant held: %d %s", r.status, r.body)
		}
		r = f.authedJSON(t, http.MethodPost, rolePath(id, "/permissions/grant"), mgrTok, ports.RolePermissionRequest{Permission: "identity.totally.fake"})
		f.wantForbidden(t, "grant unknown", r, http.StatusUnprocessableEntity, "permission_unknown")
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("deactivate: an Administrator without tenant.admin cannot deactivate a role carrying it", func(t *testing.T) {
		id := custom(t, "Carries Admin", 60)
		// Only an owner may confer tenant.admin.
		if r := f.authedJSON(t, http.MethodPost, rolePath(id, "/permissions/grant"), owner.AccessToken, ports.RolePermissionRequest{Permission: p.Meta.TenantAdmin}); r.status != http.StatusNoContent {
			t.Fatalf("owner grants tenant.admin to a custom role: %d %s", r.status, r.body)
		}
		r := f.authedJSON(t, http.MethodPost, rolePath(id, "/deactivate"), weakTok, nil)
		f.wantForbidden(t, "weak admin deactivates a tenant.admin role", r, http.StatusForbidden, "grant_exceeds_authority")
		// tenant.admin is owner-only in both directions, even for an Administrator who holds it.
		r = f.authedJSON(t, http.MethodPost, rolePath(id, "/permissions/revoke"), adminTok, ports.RolePermissionRequest{Permission: p.Meta.TenantAdmin})
		f.wantForbidden(t, "admin revokes tenant.admin from a custom role", r, http.StatusForbidden, "grant_exceeds_authority")
		r = f.authedJSON(t, http.MethodPost, rolePath(id, "/deactivate"), adminTok, nil)
		f.wantForbidden(t, "admin holding tenant.admin deactivates the role carrying it", r, http.StatusForbidden, "grant_exceeds_authority")
		if r := f.authedJSON(t, http.MethodPost, rolePath(id, "/deactivate"), owner.AccessToken, nil); r.status != http.StatusOK {
			t.Errorf("owner deactivates the role carrying tenant.admin: %d %s", r.status, r.body)
		}
		// Reactivating gives tenant.admin back to every holder: the same authority is asked.
		r = f.authedJSON(t, http.MethodPost, rolePath(id, "/reactivate"), adminTok, nil)
		f.wantForbidden(t, "admin holding tenant.admin reactivates the role carrying it", r, http.StatusForbidden, "grant_exceeds_authority")
		return
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("deactivate: a role with an active holder is 409 role_in_use; an inactive role confers nothing (ADR 0073)", func(t *testing.T) {
		id := custom(t, "Held Role", 60)
		// The role carries a permission the holder can only get through it.
		if r := f.authedJSON(t, http.MethodPost, rolePath(id, "/permissions/grant"), owner.AccessToken, ports.RolePermissionRequest{Permission: p.Tasks.WorkItems.Read}); r.status != http.StatusNoContent {
			t.Fatalf("grant the role a permission: %d %s", r.status, r.body)
		}
		holder := f.createMember(t, owner, "holder")
		f.assignRole(t, owner.AccessToken, holder.membershipID, id)
		r := f.authedJSON(t, http.MethodPost, rolePath(id, "/deactivate"), adminTok, nil)
		f.wantForbidden(t, "deactivate a held role", r, http.StatusConflict, "role_in_use")
		if e := decodeError(t, r.body); e.Errors["active_holders"][0] != "1" {
			t.Errorf("holder count = %v, want 1", e.Errors)
		}
		if got := f.roleDto(t, owner.AccessToken, id); !got.IsActive {
			t.Fatal("a refused deactivation made the role inactive")
		}
		if r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+holder.membershipID+"/deactivate", owner.AccessToken, ports.DeactivateUserRequest{Reason: "role in use test"}); r.status != http.StatusNoContent {
			t.Fatalf("deactivate holder: %d %s", r.status, r.body)
		}
		// An INACTIVE holder still holds the role: it is refused too (owner ruling 2026-11-25), and the
		// refusal says how many hold it and who.
		r = f.authedJSON(t, http.MethodPost, rolePath(id, "/deactivate"), adminTok, nil)
		f.wantForbidden(t, "deactivate a role held by an inactive member", r, http.StatusConflict, "role_in_use")
		var inUse struct {
			Holders     *int     `json:"holders"`
			HolderNames []string `json:"holder_names"`
		}
		if err := json.Unmarshal(r.body, &inUse); err != nil || inUse.Holders == nil || *inUse.Holders != 1 || len(inUse.HolderNames) != 1 || !strings.Contains(inUse.HolderNames[0], "holder") {
			t.Fatalf("role_in_use must carry holders=1 and the holder name: %s (%v)", r.body, err)
		}
		if e := decodeError(t, r.body); e.Errors["active_holders"][0] != "0" {
			t.Errorf("active holder count = %v, want 0", e.Errors)
		}
		// Taken off the last holder, the role goes inactive, answers the updated role, and repeating is a no-op.
		if r := f.authedJSON(t, http.MethodDelete, "/api/v1/users/"+holder.membershipID+"/roles/"+id, owner.AccessToken, nil); r.status != http.StatusNoContent {
			t.Fatalf("revoke the role from the inactive holder: %d %s", r.status, r.body)
		}
		for range 2 {
			r := f.authedJSON(t, http.MethodPost, rolePath(id, "/deactivate"), adminTok, nil)
			if r.status != http.StatusOK {
				t.Fatalf("deactivate with only an inactive holder: %d %s", r.status, r.body)
			}
			var dto ports.RoleDto
			if err := json.Unmarshal(r.body, &dto); err != nil || dto.ID != id || dto.IsActive {
				t.Fatalf("deactivate answers the role, now inactive: %s (%v)", r.body, err)
			}
		}
		// Nothing is deleted: the role is still readable.
		if got := f.roleDto(t, owner.AccessToken, id); got.IsActive {
			t.Errorf("the role reads as active after deactivation: %+v", got)
		}
		// An inactive role takes no new holder and cannot be edited.
		other := f.createMember(t, owner, "other")
		if r := f.authedJSON(t, http.MethodPost, "/api/v1/users/"+other.membershipID+"/roles", adminTok, ports.AssignUserRoleRequest{RoleID: id}); r.status != http.StatusConflict || decodeError(t, r.body).Error != "role_inactive" {
			t.Errorf("assign an inactive role: %d %s, want 409 role_inactive", r.status, r.body)
		}
		if r := f.authedJSON(t, http.MethodPatch, rolePath(id, ""), adminTok, ports.UpdateRoleRequest{Name: "Edited while inactive"}); r.status != http.StatusConflict || decodeError(t, r.body).Error != "role_inactive" {
			t.Errorf("edit an inactive role: %d %s, want 409 role_inactive", r.status, r.body)
		}
		// A role that nobody holds can come back, and an active role takes holders again.
		if r := f.authedJSON(t, http.MethodPost, rolePath(id, "/reactivate"), adminTok, nil); r.status != http.StatusOK {
			t.Fatalf("reactivate the role: %d %s", r.status, r.body)
		}
		f.assignRole(t, owner.AccessToken, holder.membershipID, id)
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("parent: must be strictly more senior; the key is required; null clears", func(t *testing.T) {
		child := custom(t, "Child Role", 60)
		peer := custom(t, "Peer Role", 60)
		senior := custom(t, "Senior Role", 40)
		r := f.authedJSON(t, http.MethodPatch, rolePath(child, "/parent"), adminTok, map[string]any{"parent_role_id": peer})
		f.wantForbidden(t, "equal-level parent", r, http.StatusUnprocessableEntity, "role_parent_not_eligible")
		r = f.authedJSON(t, http.MethodPatch, rolePath(child, "/parent"), adminTok, map[string]any{"parent_role_id": "01a1f625-2aff-792d-ba5e-4ecb47aeda99"})
		f.wantForbidden(t, "unknown parent", r, http.StatusUnprocessableEntity, "role_parent_not_eligible")
		r = f.authedJSON(t, http.MethodPatch, rolePath(child, "/parent"), adminTok, map[string]any{"parent_role_id": senior})
		if r.status != http.StatusNoContent {
			t.Fatalf("senior parent: %d %s", r.status, r.body)
		}
		r = f.authedJSON(t, http.MethodPatch, rolePath(child, "/parent"), adminTok, map[string]any{})
		f.wantForbidden(t, "PATCH parent {}", r, http.StatusBadRequest, "invalid_body")
		if got := f.roleDto(t, owner.AccessToken, child).ParentRoleID; got != senior {
			t.Errorf("parent = %q after a refused {}, want %s", got, senior)
		}
		r = f.authedJSON(t, http.MethodPatch, rolePath(child, "/parent"), adminTok, map[string]any{"parent_role_id": nil})
		if r.status != http.StatusNoContent {
			t.Fatalf("explicit null clears: %d %s", r.status, r.body)
		}
		if got := f.roleDto(t, owner.AccessToken, child).ParentRoleID; got != "" {
			t.Errorf("parent = %q after null, want none", got)
		}
		// create under a parent that is not strictly more senior than the new role
		rr, _ := f.createRoleVia(t, adminTok, ports.CreateRoleRequest{Name: "Under Peer", HierarchyLevel: 60, ParentRoleID: peer})
		f.wantForbidden(t, "create under an equal-level parent", rr, http.StatusUnprocessableEntity, "role_parent_not_eligible")
	})
}
