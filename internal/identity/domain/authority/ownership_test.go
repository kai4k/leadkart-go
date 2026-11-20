package authority_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
)

// Hierarchy round 4 (ADR 0069 R3 amendment): the cross-tenant operator exemption
// covers day-to-day repair, not who OWNS a customer tenant. Changing ownership
// from outside needs a super user or platform.tenants.ownership: assigning or
// revoking the Company Owner role, conferring or removing tenant.admin (by
// role or by override), and deactivating an owner. Everything else the
// exemption allows stays allowed.

type ownershipWorld struct {
	ownerR, adminR, execR, leadsR          *role.Role
	owner1, owner2, admin, exec, tenantAdm authority.Member
	opManage, opRoles, opOwn, super        authority.Actor
	opPlatformNoOwnership                  authority.Actor
	opNoExemption, tenantOwner             authority.Actor
}

func newOwnershipWorld(t *testing.T) ownershipWorld {
	t.Helper()
	p := permission.IdentityPermissions
	w := ownershipWorld{}
	w.ownerR = mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	w.adminR = mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false, p.Meta.TenantAdmin)
	w.execR = mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false, p.Crm.Leads.Read)
	w.leadsR = mkRole(6, tA, "Lead Router", false, 40, false, p.Crm.Leads.Assign)
	w.owner1 = mkMember(t, 11, tA, true, "", w.ownerR)
	w.owner2 = mkMember(t, 12, tA, true, "", w.ownerR)
	w.admin = mkMember(t, 13, tA, true, "", w.adminR)
	w.exec = mkMember(t, 14, tA, true, "", w.execR)
	w.tenantAdm = memberWithTenantAdminOverride(t, 15)
	op := func(n int, perms ...string) authority.Actor {
		return mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(n)), TenantID: tP, Rank: rk(authority.RankNone), IsPlatformOperator: true, EffectivePermissions: perms})
	}
	w.opManage = op(91, p.Platform.UsersManage)
	w.opRoles = op(92, p.Platform.RolesManage)
	w.opOwn = op(93, p.Platform.UsersManage, p.Platform.RolesManage, p.Platform.TenantsOwnership)
	w.opNoExemption = op(94, p.Platform.TenantsView)
	w.super = mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(90)), TenantID: tP, Rank: rk(authority.RankOwner), IsSuperUser: true})
	// A super user that also carries the operator flags is still just a super user.
	w.opPlatformNoOwnership = mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(95)), TenantID: tP, Rank: rk(authority.RankOwner), IsSuperUser: true,
		IsPlatformOperator: true, EffectivePermissions: []string{p.Platform.UsersManage}})
	w.tenantOwner = mkActor(t, authority.ActorInput{MembershipID: w.owner1.ID(), TenantID: tA, Rank: rk(authority.RankOwner), EffectivePermissions: adminEffective()})
	return w
}

func TestOwnership_ExemptOperatorNeedsTheOwnershipPermission(t *testing.T) {
	t.Parallel()
	w := newOwnershipWorld(t)
	p := permission.IdentityPermissions
	grantAdmin := authority.Overrides{Granted: []authority.Override{{Name: p.Meta.TenantAdmin}}, Revoked: []string{}}
	type action struct {
		name string
		do   func(a authority.Actor) error
	}
	gated := []action{
		{"assign Company Owner", func(a authority.Actor) error { return a.MayAssignRole(w.exec, w.ownerR) }},
		{"revoke Company Owner (another owner remains)", func(a authority.Actor) error {
			return a.MayRevokeRole(w.owner1, w.ownerR, []authority.Member{w.owner1, w.owner2})
		}},
		{"assign Administrator (carries tenant.admin)", func(a authority.Actor) error { return a.MayAssignRole(w.exec, w.adminR) }},
		{"revoke Administrator (carries tenant.admin)", func(a authority.Actor) error { return a.MayRevokeRole(w.admin, w.adminR, nil) }},
		{"confer tenant.admin by override", func(a authority.Actor) error { return a.MayChangeOverrides(w.exec, grantAdmin) }},
		{"remove tenant.admin by override", func(a authority.Actor) error {
			return a.MayChangeOverrides(w.tenantAdm, authority.Overrides{Granted: []authority.Override{}, Revoked: []string{}})
		}},
		{"deactivate an owner (another owner remains)", func(a authority.Actor) error {
			return a.MayDeactivate(w.owner1, []authority.Member{w.owner1, w.owner2})
		}},
		{"grant tenant.admin to a role", func(a authority.Actor) error {
			return a.MayGrantRolePermission(w.leadsR, p.Meta.TenantAdmin, authority.RoleHolders{})
		}},
		{"replace a role's permissions with tenant.admin", func(a authority.Actor) error {
			return a.MayReplaceRolePermissions(w.leadsR, []string{p.Meta.TenantAdmin}, authority.RoleHolders{})
		}},
		{"revoke tenant.admin from the Administrator role", func(a authority.Actor) error {
			return a.MayRevokeRolePermission(mkRole(7, tA, "Custom Admin", false, 30, false, p.Meta.TenantAdmin), p.Meta.TenantAdmin, authority.RoleHolders{})
		}},
		{"create a role carrying tenant.admin", func(a authority.Actor) error { return a.MayCreateRole(tA, 30, []string{p.Meta.TenantAdmin}, nil) }},
	}
	for _, c := range gated {
		for who, a := range map[string]authority.Actor{"users.manage only": w.opManage, "roles.manage only": w.opRoles} {
			if err := c.do(a); !errors.Is(err, authority.ErrOwnershipAuthorityRequired) {
				t.Errorf("%s, operator with %s: %v, want ErrOwnershipAuthorityRequired", c.name, who, err)
			}
		}
		if err := c.do(w.opOwn); err != nil {
			t.Errorf("%s, operator with platform.tenants.ownership: %v, want allowed", c.name, err)
		}
		if err := c.do(w.super); err != nil {
			t.Errorf("%s, super user: %v, want allowed", c.name, err)
		}
		if err := c.do(w.opPlatformNoOwnership); err != nil {
			t.Errorf("%s, super user carrying operator flags: %v, want allowed (a super user is not an exempt operator)", c.name, err)
		}
	}
}

func TestOwnership_EverythingElseTheExemptionAllowsStays(t *testing.T) {
	t.Parallel()
	w := newOwnershipWorld(t)
	p := permission.IdentityPermissions
	for name, err := range map[string]error{
		"assign a day-to-day role": w.opManage.MayAssignRole(w.exec, w.leadsR),
		"revoke a day-to-day role": w.opRoles.MayRevokeRole(w.exec, w.execR, nil),
		"change non-ownership overrides": w.opManage.MayChangeOverrides(w.exec, authority.Overrides{
			Granted: []authority.Override{{Name: p.Crm.Leads.Read}}, Revoked: []string{}}),
		"deactivate a non-owner":               w.opManage.MayDeactivate(w.exec, nil),
		"deactivate an Administrator":          w.opManage.MayDeactivate(w.admin, nil),
		"reactivate-class act-on an owner":     w.opManage.MayActOn(w.owner1),
		"create a role without tenant.admin":   w.opRoles.MayCreateRole(tA, 30, []string{p.Crm.Leads.Read}, nil),
		"grant a day-to-day role permission":   w.opRoles.MayGrantRolePermission(w.leadsR, p.Crm.Leads.Read, authority.RoleHolders{}),
		"clear a manager":                      w.opManage.MayClearManager(w.exec, nil),
		"delete a role without tenant.admin":   w.opRoles.MayDeleteRole(w.leadsR, authority.RoleHolders{}),
		"approve-class: act on the sole owner": w.opManage.MayActOn(w.owner1),
	} {
		if err != nil {
			t.Errorf("%s: %v, want allowed (the exemption is unchanged outside ownership)", name, err)
		}
	}
}

func TestOwnership_OnlyTheExemptOperatorIsGated(t *testing.T) {
	t.Parallel()
	w := newOwnershipWorld(t)
	// A tenant's own owner confers tenant.admin and assigns Company Owner as before.
	if err := w.tenantOwner.MayAssignRole(w.exec, w.ownerR); err != nil {
		t.Errorf("own-tenant owner assigns Company Owner: %v", err)
	}
	if err := w.tenantOwner.MayChangeOverrides(w.exec, authority.Overrides{Granted: []authority.Override{{Name: permission.IdentityPermissions.Meta.TenantAdmin}}, Revoked: []string{}}); err != nil {
		t.Errorf("own-tenant owner confers tenant.admin: %v", err)
	}
	// An operator inside its OWN tenant is not exempt: rank applies, not the ownership gate.
	own := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(96)), TenantID: tA, Rank: rk(authority.RankNone), IsPlatformOperator: true,
		EffectivePermissions: []string{permission.IdentityPermissions.Platform.UsersManage}})
	err := own.MayAssignRole(w.exec, w.ownerR)
	if errors.Is(err, authority.ErrOwnershipAuthorityRequired) || !errors.Is(err, authority.ErrInsufficientRank) {
		t.Errorf("operator in its own tenant: %v, want ErrInsufficientRank (rank, not the ownership gate)", err)
	}
	// A cross-tenant caller with no exemption is refused by the existing rules, not by this gate.
	if err := w.opNoExemption.MayAssignRole(w.exec, w.ownerR); errors.Is(err, authority.ErrOwnershipAuthorityRequired) || err == nil {
		t.Errorf("operator without an exemption permission: %v, want the existing refusal", err)
	}
}

// memberWithTenantAdminOverride is a roleless member of tA holding tenant.admin
// by a permanent override.
func memberWithTenantAdminOverride(t *testing.T, n int) authority.Member {
	t.Helper()
	m := membership.UnmarshalFromDB(membership.Snapshot{ID: membership.ID(uid(n)), PersonID: person.ID(uid(n + 500)), TenantID: tA,
		Status: membership.StatusActive, JoinedAt: t0,
		GrantedPermissions: []membership.GrantedOverride{{Permission: permission.FromConstant(permission.IdentityPermissions.Meta.TenantAdmin)}}})
	out, err := authority.MemberOf(m, nil, t0, activePerson())
	if err != nil {
		t.Fatalf("MemberOf: %v", err)
	}
	return out
}

// The Company Owner role is gated by its identity, not only by the tenant.admin
// it normally carries: a Company Owner role row that lost its permissions is
// still the ownership role.
func TestOwnership_CompanyOwnerRoleIsGatedByIdentity(t *testing.T) {
	t.Parallel()
	w := newOwnershipWorld(t)
	bareOwnerR := mkRole(8, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false)
	if err := w.opManage.MayAssignRole(w.exec, bareOwnerR); !errors.Is(err, authority.ErrOwnershipAuthorityRequired) {
		t.Errorf("assign a permissionless Company Owner role: %v, want ErrOwnershipAuthorityRequired", err)
	}
	if err := w.opOwn.MayAssignRole(w.exec, bareOwnerR); err != nil {
		t.Errorf("with platform.tenants.ownership: %v, want allowed", err)
	}
}
