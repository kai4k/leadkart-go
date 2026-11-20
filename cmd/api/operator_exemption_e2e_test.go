//go:build integration

// arch-test:no-timeout-needed — newE2EFixtureAt → startWiredPostgresForHTTP uses
// context.WithTimeout(90s) internally + testcontainers wait strategy bounds
// container startup; per-request HTTP uses t.Context() (auto-canceled on test end).

// ADR 0069 R3 (amended in hierarchy round 4): what a platform operator may and
// may not do inside a customer tenant under X-Tenant-Id. The exemption covers
// day-to-day repair (rank-free, ceiling = everything tenant.admin implies); who
// OWNS the tenant needs platform.tenants.ownership; and the owner guards, the
// super-admin rule and the platform tenant's own rank rules still bind.

package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/identitytest"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

const ownershipRequired = "ownership_authority_required"

func TestE2E_OperatorExemption_OwnershipNeedsItsOwnPermission(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	p := permission.IdentityPermissions
	b := f.registerAndLogin(t, "ownership")
	roleID := func(name string) string { return f.roleIDByName(t, b.AccessToken, name) }
	second := f.addSecondOwner(t, b, "second-owner")
	exec := f.createMember(t, b, "exec")
	hdr := scopeHdr(b.TenantID)

	usersOnly := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.TenantsView, p.Platform.UsersView, p.Platform.UsersManage}, false)
	rolesOnly := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.TenantsView, p.Platform.RolesView, p.Platform.RolesManage}, false)
	withOwnership := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.TenantsView, p.Platform.UsersView, p.Platform.UsersManage, p.Platform.RolesView, p.Platform.RolesManage, p.Platform.TenantsOwnership}, false)

	user := func(m createdMember, suffix string) string { return "/api/v1/users/" + m.membershipID + suffix }
	assign := func(tok string, m createdMember, roleName string) httpResp {
		return f.doWithHeaders(t, http.MethodPost, user(m, "/roles"), tok, ports.AssignUserRoleRequest{RoleID: roleID(roleName)}, hdr)
	}
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("assigning Company Owner", func(t *testing.T) {
		f.wantForbidden(t, "roles.manage only", assign(rolesOnly, exec, role.SystemRoles.Tenant.CompanyOwner), http.StatusForbidden, ownershipRequired)
		if r := assign(withOwnership, exec, role.SystemRoles.Tenant.CompanyOwner); r.status != http.StatusNoContent {
			t.Fatalf("with platform.tenants.ownership: %d %s, want 204", r.status, r.body)
		}
		// Revoking it is the same decision.
		f.wantForbidden(t, "revoking Company Owner with roles.manage only",
			f.doWithHeaders(t, http.MethodDelete, user(exec, "/roles/"+roleID(role.SystemRoles.Tenant.CompanyOwner)), rolesOnly, nil, hdr),
			http.StatusForbidden, ownershipRequired)
		if r := f.doWithHeaders(t, http.MethodDelete, user(exec, "/roles/"+roleID(role.SystemRoles.Tenant.CompanyOwner)), withOwnership, nil, hdr); r.status != http.StatusNoContent {
			t.Fatalf("revoke with platform.tenants.ownership: %d %s, want 204", r.status, r.body)
		}
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("a role that carries tenant.admin", func(t *testing.T) {
		f.wantForbidden(t, "assign Administrator, roles.manage only", assign(rolesOnly, exec, role.SystemRoles.Tenant.Administrator), http.StatusForbidden, ownershipRequired)
		if r := assign(withOwnership, exec, role.SystemRoles.Tenant.Administrator); r.status != http.StatusNoContent {
			t.Fatalf("assign Administrator with platform.tenants.ownership: %d %s, want 204", r.status, r.body)
		}
		if r := f.doWithHeaders(t, http.MethodDelete, user(exec, "/roles/"+roleID(role.SystemRoles.Tenant.Administrator)), withOwnership, nil, hdr); r.status != http.StatusNoContent {
			t.Fatalf("revoke Administrator with platform.tenants.ownership: %d %s, want 204", r.status, r.body)
		}
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("tenant.admin by override", func(t *testing.T) {
		grant := ports.ReplaceUserPermissionOverridesRequest{Granted: []string{p.Meta.TenantAdmin}, Revoked: []string{}}
		f.wantForbidden(t, "users.manage only confers tenant.admin",
			f.doWithHeaders(t, http.MethodPatch, user(exec, "/permission-overrides"), usersOnly, grant, hdr), http.StatusForbidden, ownershipRequired)
		if r := f.doWithHeaders(t, http.MethodPatch, user(exec, "/permission-overrides"), withOwnership, grant, hdr); r.status != http.StatusNoContent {
			t.Fatalf("confer tenant.admin with platform.tenants.ownership: %d %s, want 204", r.status, r.body)
		}
		f.wantForbidden(t, "users.manage only removes tenant.admin",
			f.doWithHeaders(t, http.MethodPatch, user(exec, "/permission-overrides"), usersOnly,
				ports.ReplaceUserPermissionOverridesRequest{Granted: []string{}, Revoked: []string{}}, hdr), http.StatusForbidden, ownershipRequired)
		if r := f.doWithHeaders(t, http.MethodPatch, user(exec, "/permission-overrides"), withOwnership,
			ports.ReplaceUserPermissionOverridesRequest{Granted: []string{}, Revoked: []string{}}, hdr); r.status != http.StatusNoContent {
			t.Fatalf("remove tenant.admin with platform.tenants.ownership: %d %s, want 204", r.status, r.body)
		}
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("deactivating an owner", func(t *testing.T) {
		f.wantForbidden(t, "users.manage only deactivates an owner",
			f.doWithHeaders(t, http.MethodPost, user(second, "/deactivate"), usersOnly, ports.DeactivateUserRequest{Reason: "ownership test"}, hdr),
			http.StatusForbidden, ownershipRequired)
		if r := f.doWithHeaders(t, http.MethodPost, user(second, "/deactivate"), withOwnership, ports.DeactivateUserRequest{Reason: "ownership test"}, hdr); r.status != http.StatusNoContent {
			t.Fatalf("deactivate an owner with platform.tenants.ownership: %d %s, want 204", r.status, r.body)
		}
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("everything else the exemption allows stays allowed", func(t *testing.T) {
		if r := assign(rolesOnly, exec, role.SystemRoles.Tenant.SalesExecutive); r.status != http.StatusNoContent {
			t.Errorf("assign a day-to-day role without ownership: %d %s, want 204", r.status, r.body)
		}
		if r := f.doWithHeaders(t, http.MethodPatch, user(exec, "/permission-overrides"), usersOnly,
			ports.ReplaceUserPermissionOverridesRequest{Granted: []string{p.Crm.Leads.Read}, Revoked: []string{}}, hdr); r.status != http.StatusNoContent {
			t.Errorf("change non-ownership overrides without ownership: %d %s, want 204", r.status, r.body)
		}
		if r := f.doWithHeaders(t, http.MethodPost, user(exec, "/deactivate"), usersOnly, ports.DeactivateUserRequest{Reason: "day-to-day repair"}, hdr); r.status != http.StatusNoContent {
			t.Errorf("deactivate a non-owner without ownership: %d %s, want 204", r.status, r.body)
		}
	})
}

// A super-admin role row, added the way an operator repair would: through the
// repositories, without the policy.
func (f e2eFixture) addSuperAdminMember(t *testing.T, owner registeredTenant) createdMember {
	t.Helper()
	m := f.createMember(t, owner, "super-admin")
	tx := pg.NewTransactor(f.Pool)
	r, err := role.New(role.ID(ids.NewV7().String()), tenant.ID(owner.TenantID), "Support Super", false, 1, true, time.Now())
	if err != nil {
		t.Fatalf("role.New: %v", err)
	}
	if err := adapters.NewRoleRepository(f.Pool, tx).Add(t.Context(), r); err != nil {
		t.Fatalf("add super-admin role: %v", err)
	}
	if err := adapters.NewMembershipRepository(f.Pool, tx).UpdateByID(t.Context(), tenant.ID(owner.TenantID), membership.ID(m.membershipID),
		func(mm *membership.Membership) (bool, error) { return true, mm.AssignRole(r.ID(), time.Now()) }); err != nil {
		t.Fatalf("assign super-admin role: %v", err)
	}
	return m
}

func TestE2E_OperatorExemption_StillBoundByOwnerGuardsAndSuperAdmin(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	p := permission.IdentityPermissions
	b := f.registerAndLogin(t, "exempt-bounds")
	hdr := scopeHdr(b.TenantID)
	// Even holding platform.tenants.ownership, the exemption is not a super user.
	op := mintOperatorJWTExact(t, f.Issuer, f.persons, "", []string{p.Platform.TenantsView, p.Platform.UsersView, p.Platform.UsersManage, p.Platform.RolesView, p.Platform.RolesManage, p.Platform.TenantsOwnership}, false)
	ownerRole := f.roleIDByName(t, b.AccessToken, role.SystemRoles.Tenant.CompanyOwner)

	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("cannot remove the last owner", func(t *testing.T) {
		f.wantForbidden(t, "deactivate the sole owner",
			f.doWithHeaders(t, http.MethodPost, "/api/v1/users/"+b.MembershipID+"/deactivate", op, ports.DeactivateUserRequest{Reason: "bounds test"}, hdr),
			http.StatusConflict, "last_owner_protected")
		f.wantForbidden(t, "revoke Company Owner from the sole owner",
			f.doWithHeaders(t, http.MethodDelete, "/api/v1/users/"+b.MembershipID+"/roles/"+ownerRole, op, nil, hdr),
			http.StatusConflict, "last_owner_protected")
		if u := f.getUser(t, b.AccessToken, b.MembershipID); u.Status != "active" {
			t.Errorf("the sole owner is %q after refused removals", u.Status)
		}
	})
	// arch-test:serial — subtests share one fixture and one tenant.
	t.Run("cannot act on a super-admin member", func(t *testing.T) {
		sa := f.addSuperAdminMember(t, b)
		exec := f.roleIDByName(t, b.AccessToken, role.SystemRoles.Tenant.SalesExecutive)
		f.wantForbidden(t, "assign a role to a super-admin member",
			f.doWithHeaders(t, http.MethodPost, "/api/v1/users/"+sa.membershipID+"/roles", op, ports.AssignUserRoleRequest{RoleID: exec}, hdr),
			http.StatusForbidden, "insufficient_rank")
		f.wantForbidden(t, "deactivate a super-admin member",
			f.doWithHeaders(t, http.MethodPost, "/api/v1/users/"+sa.membershipID+"/deactivate", op, ports.DeactivateUserRequest{Reason: "bounds test"}, hdr),
			http.StatusForbidden, "insufficient_rank")
	})
}

// Inside the platform tenant itself an operator is an ordinary member: rank
// decides, the exemption and the ownership gate do not apply.
func TestE2E_OperatorExemption_InsideThePlatformTenantFollowsRank(t *testing.T) {
	f := newE2EFixtureAt(t, time.Now)
	plat := f.registerPlatformTenant(t)
	p := permission.IdentityPermissions
	op := f.createMember(t, plat, "plat-op")
	target := f.createMember(t, plat, "plat-target")
	platOwnerRole := f.roleIDByName(t, plat.AccessToken, role.SystemRoles.Tenant.CompanyOwner)
	mint := f.mintMemberToken
	opTok := mint(t, plat, op, p.Platform.UsersManage, p.Platform.RolesManage, p.Platform.TenantsOwnership)

	// A role-less platform member holding every operator permission still has no rank.
	f.wantForbidden(t, "operator assigns Company Owner inside the platform tenant",
		f.authedJSON(t, http.MethodPost, "/api/v1/users/"+target.membershipID+"/roles", opTok, ports.AssignUserRoleRequest{RoleID: platOwnerRole}),
		http.StatusForbidden, "insufficient_rank")
	f.wantForbidden(t, "operator deactivates the platform owner",
		f.authedJSON(t, http.MethodPost, "/api/v1/users/"+plat.MembershipID+"/deactivate", opTok, ports.DeactivateUserRequest{Reason: "rank test"}),
		http.StatusForbidden, "insufficient_rank")
	if u := f.getUser(t, plat.AccessToken, plat.MembershipID); u.Status != "active" {
		t.Errorf("the platform owner is %q after a refused deactivation", u.Status)
	}
	if u := f.getUser(t, plat.AccessToken, target.membershipID); len(u.RoleIDs) != 0 {
		t.Errorf("the refused assignment gave the target roles %v", u.RoleIDs)
	}
}

// mintMemberToken issues a platform-tenant token for m carrying exactly perms
// (the member keeps its real, role-less membership).
func (f e2eFixture) mintMemberToken(t *testing.T, plat registeredTenant, m createdMember, perms ...string) string {
	t.Helper()
	tok, err := f.Issuer.Issue(jwt.IssueArgs{
		PersonID: m.personID, TenantID: plat.TenantID, TenantSlug: tenant.PlatformSlug,
		MembershipID: m.membershipID, SecurityStamp: identitytest.GetSecurityStamp(t, f.Pool, m.personID),
		IsPlatform: true, Permissions: perms,
	})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	return tok
}
