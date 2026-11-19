package authority_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
)

type decision struct {
	name string
	got  error
	want error
}

// mismatches lists every case whose error is not the wanted sentinel (nil for "allowed").
func mismatches(cases []decision) []string {
	var out []string
	for _, c := range cases {
		if c.want == nil && c.got != nil || c.want != nil && !errors.Is(c.got, c.want) {
			out = append(out, fmt.Sprintf("%s: got %v, want %v", c.name, c.got, c.want))
		}
	}
	return out
}

func TestGuardLastOwner_CountsOnlyActiveOwnersOfTheSameTenant(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	ownerRB := mkRole(2, tB, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	rogueR := mkRole(3, tA, "Rogue", false, 0, false)
	adminR := mkRole(4, tA, role.SystemRoles.Tenant.Administrator, false, 10, false, p.Meta.TenantAdmin)
	owner1 := mkMember(t, 11, tA, true, "", ownerR)
	owner2 := mkMember(t, 12, tA, true, "", ownerR)
	inactiveOwner := mkMember(t, 13, tA, false, "", ownerR)
	otherTenantOwner := mkMember(t, 14, tB, true, "", ownerRB)
	rogue := mkMember(t, 15, tA, true, "", rogueR)
	admin := mkMember(t, 16, tA, true, "", adminR)
	var zero authority.Member

	for _, m := range mismatches([]decision{
		{"nil list", authority.GuardLastOwner(owner1, nil), authority.ErrLastOwnerProtected},
		{"only the target", authority.GuardLastOwner(owner1, []authority.Member{owner1}), authority.ErrLastOwnerProtected},
		{"another active owner", authority.GuardLastOwner(owner1, []authority.Member{owner1, owner2}), nil},
		{"only an inactive owner besides", authority.GuardLastOwner(owner1, []authority.Member{owner1, inactiveOwner}), authority.ErrLastOwnerProtected},
		{"only another tenant's owner besides", authority.GuardLastOwner(owner1, []authority.Member{owner1, otherTenantOwner}), authority.ErrLastOwnerProtected},
		{"a level-0 custom holder is not an owner", authority.GuardLastOwner(owner1, []authority.Member{owner1, rogue}), authority.ErrLastOwnerProtected},
		{"a non-owner is not an owner", authority.GuardLastOwner(owner1, []authority.Member{owner1, admin}), authority.ErrLastOwnerProtected},
		{"zero members in the list are skipped", authority.GuardLastOwner(owner1, []authority.Member{zero, owner1}), authority.ErrLastOwnerProtected},
		{"an inactive sole owner is still the last owner", authority.GuardLastOwner(inactiveOwner, nil), authority.ErrLastOwnerProtected},
		{"an inactive owner beside an active one may go", authority.GuardLastOwner(inactiveOwner, []authority.Member{inactiveOwner, owner1}), nil},
		{"an inactive owner beside only another inactive one is protected", authority.GuardLastOwner(inactiveOwner, []authority.Member{inactiveOwner, mkMember(t, 17, tA, false, "", ownerR)}), authority.ErrLastOwnerProtected},
		{"non-owner target", authority.GuardLastOwner(admin, nil), nil},
		{"zero target", authority.GuardLastOwner(zero, []authority.Member{owner1, owner2}), authority.ErrNoAuthority},
	}) {
		t.Error(m)
	}
}

func TestMayChangeOverrides_ActsCeilingAndOwnerGuard(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	adminR := mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false, p.Meta.TenantAdmin)
	mgrR := mkRole(3, tA, role.SystemRoles.Tenant.SalesManager, false, 20, false, p.Crm.Leads.Read)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	owner1 := mkMember(t, 11, tA, true, "", ownerR)
	owner2 := mkMember(t, 12, tA, true, "", ownerR)
	admin := mkMember(t, 13, tA, true, "", adminR)
	mgr := mkMember(t, 15, tA, true, "", mgrR)
	user := mkMember(t, 18, tA, true, "", userR)
	aOwner := mkActor(t, authority.ActorInput{MembershipID: owner1.ID(), TenantID: tA, Rank: rk(authority.RankOwner), EffectivePermissions: adminEffective()})
	aAdmin := mkActor(t, authority.ActorInput{MembershipID: admin.ID(), TenantID: tA, Rank: rk(authority.RankAdmin), EffectivePermissions: adminEffective()})
	aMgr := mkActor(t, authority.ActorInput{MembershipID: mgr.ID(), TenantID: tA, Rank: rk(authority.RankManager), EffectivePermissions: []string{p.Crm.Leads.Read}})
	aSuper := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(90)), TenantID: tP, IsSuperUser: true})
	ta := p.Meta.TenantAdmin

	for _, m := range mismatches([]decision{
		{"admin cannot touch an owner at all", aAdmin.MayChangeOverrides(owner1, authority.Overrides{Granted: grants(p.Crm.Leads.Read)}), authority.ErrInsufficientRank},
		{"manager grants a permission it does not hold", aMgr.MayChangeOverrides(user, authority.Overrides{Granted: grants(p.Crm.Leads.Assign)}), authority.ErrGrantExceedsAuthority},
		{"manager grants a held permission", aMgr.MayChangeOverrides(user, authority.Overrides{Granted: grants(p.Crm.Leads.Read)}), nil},
		{"admin grants tenant.admin", aAdmin.MayChangeOverrides(user, authority.Overrides{Granted: grants(ta)}), authority.ErrGrantExceedsAuthority},
		{"owner grants tenant.admin to a user", aOwner.MayChangeOverrides(user, authority.Overrides{Granted: grants(ta)}), nil},
		{"owner revokes tenant.admin from another owner", aOwner.MayChangeOverrides(owner2, authority.Overrides{Granted: grants(ta), Revoked: []string{ta}}), authority.ErrOwnerProtected},
		{"owner changes another owner's other overrides", aOwner.MayChangeOverrides(owner2, authority.Overrides{Granted: grants(p.Crm.Leads.Read), Revoked: []string{p.Crm.Leads.Read}}), nil},
		{"owner revokes tenant.admin override from an admin", aOwner.MayChangeOverrides(admin, authority.Overrides{Granted: grants(ta), Revoked: []string{ta}}), nil},
		{"the owner guard binds a super user too", aSuper.MayChangeOverrides(owner2, authority.Overrides{Granted: grants(ta), Revoked: []string{ta}}), authority.ErrOwnerProtected},
		{"empty change on a valid target", aAdmin.MayChangeOverrides(user, authority.Overrides{}), nil},
		{"unknown name in the delta", aAdmin.MayChangeOverrides(user, authority.Overrides{Granted: grants("nope")}), authority.ErrGrantExceedsAuthority},
		{"self", aAdmin.MayChangeOverrides(admin, authority.Overrides{Granted: grants(p.Crm.Leads.Read)}), authority.ErrSelfTarget},
	}) {
		t.Error(m)
	}
}

func TestMayDeactivate_ActsAndGuardsLastOwner(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	adminR := mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false, p.Meta.TenantAdmin)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	owner1 := mkMember(t, 11, tA, true, "", ownerR)
	owner2 := mkMember(t, 12, tA, true, "", ownerR)
	admin := mkMember(t, 13, tA, true, "", adminR)
	user := mkMember(t, 18, tA, true, "", userR)
	aOwner := mkActor(t, authority.ActorInput{MembershipID: owner1.ID(), TenantID: tA, Rank: rk(authority.RankOwner), EffectivePermissions: adminEffective()})
	aAdmin := mkActor(t, authority.ActorInput{MembershipID: admin.ID(), TenantID: tA, Rank: rk(authority.RankAdmin), EffectivePermissions: adminEffective()})
	aSuper := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(90)), TenantID: tP, IsSuperUser: true})

	for _, m := range mismatches([]decision{
		{"owner deactivates a user", aOwner.MayDeactivate(user, []authority.Member{owner1, owner2}), nil},
		{"admin cannot deactivate an owner (rank first)", aAdmin.MayDeactivate(owner1, []authority.Member{owner1}), authority.ErrInsufficientRank},
		{"owner deactivates the other owner while a third remains", aOwner.MayDeactivate(owner2, []authority.Member{owner1, owner2}), nil},
		{"the owner list must hold another active owner", aOwner.MayDeactivate(owner2, []authority.Member{owner2}), authority.ErrLastOwnerProtected},
		{"the sole owner cannot be deactivated by a super user", aSuper.MayDeactivate(owner1, []authority.Member{owner1}), authority.ErrLastOwnerProtected},
		{"nil owner list protects an owner", aSuper.MayDeactivate(owner1, nil), authority.ErrLastOwnerProtected},
		{"self", aAdmin.MayDeactivate(admin, nil), authority.ErrSelfTarget},
	}) {
		t.Error(m)
	}
}

func TestMayRevokeRole_GuardsLastOwnerOnTheOwnerRole(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	mgrR := mkRole(3, tA, role.SystemRoles.Tenant.SalesManager, false, 20, false)
	owner1 := mkMember(t, 11, tA, true, "", ownerR)
	owner2 := mkMember(t, 12, tA, true, "", ownerR)
	ownerAndMgr := mkMember(t, 14, tA, true, "", ownerR, mgrR)
	aOwner := mkActor(t, authority.ActorInput{MembershipID: owner1.ID(), TenantID: tA, Rank: rk(authority.RankOwner), EffectivePermissions: adminEffective()})

	for _, m := range mismatches([]decision{
		{"revoke the owner role while another owner remains", aOwner.MayRevokeRole(owner2, ownerR, []authority.Member{owner1, owner2}), nil},
		{"revoke the owner role from the last owner", aOwner.MayRevokeRole(owner2, ownerR, []authority.Member{owner2}), authority.ErrLastOwnerProtected},
		{"revoke the owner role with no owner list", aOwner.MayRevokeRole(owner2, ownerR, nil), authority.ErrLastOwnerProtected},
		{"revoke another role from the sole owner needs no guard", aOwner.MayRevokeRole(ownerAndMgr, mgrR, []authority.Member{ownerAndMgr}), nil},
	}) {
		t.Error(m)
	}
}

func TestMayDeleteRole(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	adminR := mkRole(2, tA, role.SystemRoles.Tenant.Administrator, true, 10, false, p.Meta.TenantAdmin)
	mgrR := mkRole(3, tA, role.SystemRoles.Tenant.SalesManager, false, 20, false, p.Crm.Leads.Read, p.Crm.Leads.ReadAll, p.Roles.Approve)
	tlR := mkRole(4, tA, role.SystemRoles.Tenant.TeamLead, false, 30, false, p.Crm.Leads.Read)
	custom := mkRole(6, tA, "Lead Router", false, 40, false, p.Crm.Leads.Assign)
	rogue := mkRole(7, tA, "Admin Lite", false, 40, false, p.Meta.TenantAdmin)
	foreign := mkRole(8, tB, "Foreign", false, 40, false)
	superR := mkRole(9, tA, "SuperAdmin", false, 40, true)
	owner := mkMember(t, 11, tA, true, "", ownerR)
	admin := mkMember(t, 13, tA, true, "", adminR)
	mgr := mkMember(t, 15, tA, true, "", mgrR)
	aOwner := mkActor(t, authority.ActorInput{MembershipID: owner.ID(), TenantID: tA, Rank: rk(authority.RankOwner), EffectivePermissions: adminEffective()})
	aAdmin := mkActor(t, authority.ActorInput{MembershipID: admin.ID(), TenantID: tA, Rank: rk(authority.RankAdmin), EffectivePermissions: adminEffective()})
	aMgr := mkActor(t, authority.ActorInput{MembershipID: mgr.ID(), TenantID: tA, Rank: rk(authority.RankManager), EffectivePermissions: []string{p.Crm.Leads.Read, p.Crm.Leads.ReadAll, p.Roles.Approve}})
	aSuper := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(90)), TenantID: tP, IsSuperUser: true})

	for _, m := range mismatches([]decision{
		{"admin deletes a custom role it may confer", aAdmin.MayDeleteRole(custom), nil},
		{"admin deletes a role carrying tenant.admin", aAdmin.MayDeleteRole(rogue), authority.ErrGrantExceedsAuthority},
		{"owner deletes a role carrying tenant.admin", aOwner.MayDeleteRole(rogue), nil},
		{"manager deletes a role with a permission it lacks", aMgr.MayDeleteRole(custom), authority.ErrGrantExceedsAuthority},
		{"manager deletes a team lead role it holds the permissions of", aMgr.MayDeleteRole(tlR), nil},
		{"system default roles are never deleted by the owner", aOwner.MayDeleteRole(adminR), role.ErrSystemDefault},
		{"nor by a super user", aSuper.MayDeleteRole(ownerR), role.ErrSystemDefault},
		{"another tenant's role", aAdmin.MayDeleteRole(foreign), authority.ErrNoAuthority},
		{"a super-admin role needs a super user", aAdmin.MayDeleteRole(superR), authority.ErrInsufficientRank},
		{"a super user deletes it", aSuper.MayDeleteRole(superR), nil},
		{"nil role", aOwner.MayDeleteRole(nil), authority.ErrNoAuthority},
	}) {
		t.Error(m)
	}
}

func TestMayReplaceAndClearManager(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	adminR := mkRole(2, tA, role.SystemRoles.Tenant.Administrator, false, 10, false, p.Meta.TenantAdmin)
	mgrR := mkRole(3, tA, role.SystemRoles.Tenant.SalesManager, false, 20, false, p.Roles.Approve)
	tlR := mkRole(4, tA, role.SystemRoles.Tenant.TeamLead, false, 30, false, p.Roles.Approve)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	userRB := mkRole(25, tB, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	mgrRB := mkRole(26, tB, role.SystemRoles.Tenant.SalesManager, false, 20, false)
	owner := mkMember(t, 11, tA, true, "", ownerR)
	admin := mkMember(t, 13, tA, true, "", adminR)
	mgr := mkMember(t, 15, tA, true, "", mgrR)
	mgr2 := mkMember(t, 16, tA, true, "", mgrR)
	tl := mkMember(t, 17, tA, true, mgr.ID(), tlR)
	unmanaged := mkMember(t, 18, tA, true, "", userR)
	reportsToMgr := mkMember(t, 19, tA, true, mgr.ID(), userR)
	reportsToOwner := mkMember(t, 20, tA, true, owner.ID(), userR)
	foreignMgr := mkMember(t, 22, tB, true, "", mgrRB)
	foreignUser := mkMember(t, 23, tB, true, "", userRB)
	aAdmin := mkActor(t, authority.ActorInput{MembershipID: admin.ID(), TenantID: tA, Rank: rk(authority.RankAdmin), EffectivePermissions: adminEffective()})
	aTL := mkActor(t, authority.ActorInput{MembershipID: tl.ID(), TenantID: tA, Rank: rk(authority.RankTeamLead), EffectivePermissions: []string{p.Roles.Approve}})

	for _, m := range mismatches([]decision{
		{"set a first manager", aAdmin.MayReplaceManager(unmanaged, mgr, nil), nil},
		{"replace a manager", aAdmin.MayReplaceManager(reportsToMgr, mgr2, &mgr), nil},
		{"clear a manager", aAdmin.MayClearManager(reportsToMgr, &mgr), nil},
		{"clear when there is none", aAdmin.MayClearManager(unmanaged, nil), nil},
		{"a current manager is required when the member has one", aAdmin.MayReplaceManager(reportsToMgr, mgr2, nil), authority.ErrNoAuthority},
		{"a current manager is required to clear", aAdmin.MayClearManager(reportsToMgr, nil), authority.ErrNoAuthority},
		{"the wrong current manager", aAdmin.MayReplaceManager(reportsToMgr, mgr2, &mgr2), authority.ErrNoAuthority},
		{"a current manager given for a member that has none", aAdmin.MayReplaceManager(unmanaged, mgr, &mgr2), authority.ErrNoAuthority},
		{"clear with a current manager the member does not have", aAdmin.MayClearManager(unmanaged, &mgr), authority.ErrNoAuthority},
		{"a more senior current manager blocks a team lead", aTL.MayReplaceManager(reportsToOwner, mgr, &owner), authority.ErrInsufficientRank},
		{"and blocks clearing", aTL.MayClearManager(reportsToOwner, &owner), authority.ErrInsufficientRank},
		{"cross-tenant manager is not eligible (422 class)", aAdmin.MayReplaceManager(unmanaged, foreignMgr, nil), authority.ErrManagerNotEligible},
		{"cross-tenant subordinate is out of authority", aAdmin.MayReplaceManager(foreignUser, mgr, nil), authority.ErrNoAuthority},
	}) {
		t.Error(m)
	}
}
