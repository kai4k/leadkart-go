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

// memberWithOverrideGrant is a tA member holding name as a permanent override.
func memberWithOverrideGrant(t *testing.T, n int, name string) authority.Member {
	t.Helper()
	m := membership.UnmarshalFromDB(membership.Snapshot{ID: membership.ID(uid(n)), PersonID: person.ID(uid(n + 500)), TenantID: tA,
		Status: membership.StatusActive, JoinedAt: t0,
		GrantedPermissions: []membership.GrantedOverride{{Permission: permission.FromConstant(name)}}})
	out, err := authority.MemberOf(m, nil, t0, activePerson())
	if err != nil {
		t.Fatalf("MemberOf: %v", err)
	}
	return out
}

// G2m S1a: no LeadKart actor puts a platform-managed name on a company role or member; inside the
// platform tenant it stays possible for an actor holding the platform role fact.
func TestPlatformManagedNames_NeverConferredOutsideThePlatformTenant(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	rm := []string{p.Platform.UsersManage, p.Platform.RolesManage}
	operator := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(91)), TenantID: tP, Rank: rk(authority.RankNone), IsPlatformOperator: true,
		EffectivePermissions: rm, MayManageRoles: true})
	super := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(90)), TenantID: tP, IsSuperUser: true, MayManageRoles: true})
	impersonation := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(92)), TenantID: tA, IsImpersonation: true,
		EffectivePermissions: []string{p.Meta.TenantAdmin}, MayManageRoles: true})
	companyWithFact, _ := companyOwnerActor(t, true)

	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	user := mkMember(t, 18, tA, true, "", userR)
	custom := mkRole(40, tA, "Custom", false, 60, false)
	carrying := mkRole(42, tA, "Carries a managed name", false, 60, false, p.Roles.Update)
	h := authority.RoleHolders{}
	for name, a := range map[string]authority.Actor{"operator": operator, "super user": super, "impersonation": impersonation, "company actor with the fact": companyWithFact} {
		for _, n := range []string{p.Roles.Create, p.Roles.Update, p.Roles.Manage} {
			cases := []decision{
				{name + " creates a company role carrying " + n, a.MayCreateRole(tA, 60, []string{n}, nil), authority.ErrGrantExceedsAuthority},
				{name + " grants " + n + " to a company role", a.MayGrantRolePermission(custom, n, h), authority.ErrGrantExceedsAuthority},
				{name + " replaces a company role's permissions with " + n, a.MayReplaceRolePermissions(custom, []string{n}, h), authority.ErrGrantExceedsAuthority},
				{name + " grants " + n + " to a company member by override", a.MayChangeOverrides(user, authority.Overrides{Granted: []authority.Override{{Name: n}}}), authority.ErrGrantExceedsAuthority},
			}
			for _, m := range mismatches(cases) {
				t.Error(m)
			}
		}
		if err := a.MayAssignRole(user, carrying); !errors.Is(err, authority.ErrGrantExceedsAuthority) {
			t.Errorf("%s assigns a company role carrying identity.roles.update: %v, want ErrGrantExceedsAuthority", name, err)
		}
		if err := a.MayReactivateRole(carrying, h); !errors.Is(err, authority.ErrGrantExceedsAuthority) {
			t.Errorf("%s reactivates a company role carrying identity.roles.update: %v, want ErrGrantExceedsAuthority", name, err)
		}
	}

	// The platform tenant is unaffected: its owner (a platform actor with the fact) manages its roles.
	ownerR := mkRole(51, tP, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	pOwner := mkMember(t, 61, tP, true, "", ownerR)
	pOwnerActor := mkActor(t, authority.ActorInput{MembershipID: pOwner.ID(), TenantID: tP, Rank: rk(authority.RankOwner), IsPlatformOperator: true,
		EffectivePermissions: []string{p.Meta.TenantAdmin, p.Platform.RolesManage}, MayManageRoles: true})
	if err := pOwnerActor.MayCreateRole(tP, 60, []string{p.Roles.Update}, nil); err != nil {
		t.Errorf("the platform owner creates a platform role carrying identity.roles.update: %v", err)
	}
}

// G2m S1b: an inert managed name is always removable, by a company owner who does not hold it.
func TestPlatformManagedNames_AlwaysRemovable(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	owner, _ := companyOwnerActor(t, false)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	carrying := mkRole(42, tA, "Carries a managed name", false, 60, false, p.Roles.Update, p.Crm.Leads.Read)
	user := mkMember(t, 18, tA, true, "", userR, carrying)
	withOverride := memberWithOverrideGrant(t, 19, p.Roles.Manage)
	cases := []decision{
		{"owner revokes a role carrying a managed name", owner.MayRevokeRole(user, carrying, nil), nil},
		{"owner removes a managed-name override", owner.MayChangeOverrides(withOverride, authority.Overrides{Granted: []authority.Override{}, Revoked: []string{}}), nil},
	}
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
	// Removing is not conferring: the ordinary ceiling still applies to every other name.
	unheld := mkRole(43, tA, "Carries an unheld name", false, 60, false, p.Roles.Update, p.Crm.Leads.Assign)
	if err := owner.MayRevokeRole(user, unheld, nil); err != nil {
		// the owner holds every tenant.admin-implied name, so this must pass too
		t.Errorf("owner revokes a role with an ordinary name and a managed name: %v", err)
	}
}

// G2m M1: the role-write fact is decided for the TARGET tenant. A platform-tenant actor holding
// platform.users.manage only is exempt in a company, yet may not write its roles.
func TestRoleWrites_FactIsDecidedPerTargetTenant(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	usersOnly := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(91)), TenantID: tP, Rank: rk(authority.RankOwner), IsPlatformOperator: true,
		EffectivePermissions: []string{p.Meta.TenantAdmin, p.Platform.UsersManage}, MayManageRoles: true})
	withRoles := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(92)), TenantID: tP, Rank: rk(authority.RankOwner), IsPlatformOperator: true,
		EffectivePermissions: []string{p.Meta.TenantAdmin, p.Platform.UsersManage, p.Platform.RolesManage}, MayManageRoles: true})
	custom := mkRole(40, tA, "Custom", false, 60, false, p.Crm.Leads.Read)
	ownTenant := mkRole(44, tP, "Platform Custom", false, 60, false)
	h := authority.RoleHolders{}
	cases := []decision{
		{"users.manage only creates a company role", usersOnly.MayCreateRole(tA, 60, []string{p.Crm.Leads.Read}, nil), authority.ErrNoAuthority},
		{"users.manage only edits a company role", usersOnly.MayEditRole(custom, 55, h), authority.ErrNoAuthority},
		{"users.manage only edits a role of its own tenant", usersOnly.MayEditRole(ownTenant, 55, h), nil},
		{"roles.manage creates a company role", withRoles.MayCreateRole(tA, 60, []string{p.Crm.Leads.Read}, nil), nil},
	}
	for _, m := range mismatches(cases) {
		t.Error(m)
	}
}

// G2m M2: the managed-name rule carries the decision for actors whose ordinary ceiling would say yes
// without the fact: a company super-admin role and an exempt operator without the fact.
func TestPlatformManagedNames_RuleHoldsWhereTheCeilingWouldPass(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	user := mkMember(t, 18, tA, true, "", userR)
	grant := authority.Overrides{Granted: []authority.Override{{Name: p.Roles.Update}}}
	// Built with NewActor (mkActor forces the fact on): both lack the fact.
	noFact := func(in authority.ActorInput) authority.Actor {
		a, err := authority.NewActor(in)
		if err != nil {
			t.Fatalf("NewActor: %v", err)
		}
		return a
	}
	companySuper := noFact(authority.ActorInput{MembershipID: membership.ID(uid(93)), TenantID: tA, IsSuperUser: true})
	usersOnlyOperator := noFact(authority.ActorInput{MembershipID: membership.ID(uid(91)), TenantID: tP, Rank: rk(authority.RankNone), IsPlatformOperator: true,
		EffectivePermissions: []string{p.Platform.UsersManage}})
	for name, a := range map[string]authority.Actor{"a company super-admin role": companySuper, "an operator holding only platform.users.manage": usersOnlyOperator} {
		if err := a.MayChangeOverrides(user, grant); !errors.Is(err, authority.ErrGrantExceedsAuthority) {
			t.Errorf("%s confers identity.roles.update by override: %v, want ErrGrantExceedsAuthority", name, err)
		}
	}
}

// Owner ruling 2026-11-25 (ADR 0073 addendum), carried in the domain: creating, editing and deleting a
// role is LeadKart platform authority. The actor carries that as a fact; every role-writing decision
// refuses without it, whatever else the actor holds, and the three names cannot be conferred or
// requested by anyone without it.

func companyOwnerActor(t *testing.T, manage bool) (authority.Actor, authority.Member) {
	t.Helper()
	p := permission.IdentityPermissions
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	owner := mkMember(t, 11, tA, true, "", ownerR)
	a, err := authority.NewActor(authority.ActorInput{MembershipID: owner.ID(), TenantID: tA, Rank: rk(authority.RankOwner),
		EffectivePermissions: []string{p.Meta.TenantAdmin}, MayManageRoles: manage})
	if err != nil {
		t.Fatalf("NewActor: %v", err)
	}
	return a, owner
}

func TestRoleWrites_RefusedInTheDomainWithoutPlatformAuthority(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	custom := mkRole(40, tA, "Custom", false, 60, false, p.Crm.Leads.Read)
	parent := mkRole(41, tA, "Parent", false, 40, false)
	for _, tc := range []struct {
		name   string
		manage bool
		want   error
	}{
		{"a company owner", false, authority.ErrNoAuthority},
		{"platform authority", true, nil},
	} {
		a, _ := companyOwnerActor(t, tc.manage)
		h := authority.RoleHolders{}
		cases := []decision{
			{tc.name + " creates", a.MayCreateRole(tA, 60, []string{p.Crm.Leads.Read}, nil), tc.want},
			{tc.name + " edits", a.MayEditRole(custom, 55, h), tc.want},
			{tc.name + " replaces permissions", a.MayReplaceRolePermissions(custom, []string{p.Crm.Leads.Read}, h), tc.want},
			{tc.name + " grants", a.MayGrantRolePermission(custom, p.Crm.Leads.Read, h), tc.want},
			{tc.name + " revokes", a.MayRevokeRolePermission(custom, p.Crm.Leads.Read, h), tc.want},
			{tc.name + " deactivates", a.MayDeactivateRole(custom, h), tc.want},
			{tc.name + " reactivates", a.MayReactivateRole(custom, h), tc.want},
			{tc.name + " re-parents", a.MaySetRoleParent(custom, parent), tc.want},
		}
		for _, m := range mismatches(cases) {
			t.Error(m)
		}
	}
	if z := (authority.Actor{}); z.MayManageRoles() {
		t.Error("the zero actor may manage roles")
	}
}

func TestPlatformManagedRoleNames_CannotBeConferredOrRequestedByACompany(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	owner, _ := companyOwnerActor(t, false)
	opActor, _ := companyOwnerActor(t, true)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	user := mkMember(t, 18, tA, true, "", userR)
	holdsNames := mkRole(42, tA, "Holds roles power", false, 60, false, p.Roles.Update)
	for _, n := range []string{p.Roles.Create, p.Roles.Update, p.Roles.Manage} {
		if permission.IsRequestable(n) {
			t.Errorf("%s is requestable", n)
		}
		cases := []decision{
			{"owner grants " + n + " by override", owner.MayChangeOverrides(user, authority.Overrides{Granted: []authority.Override{{Name: n}}}), authority.ErrGrantExceedsAuthority},
			{"owner approves a request for " + n, owner.MayApprove(user, n), authority.ErrGrantExceedsAuthority},
		}
		for _, m := range mismatches(cases) {
			t.Error(m)
		}
	}
	// A role that already carries a managed name cannot be assigned by a company either.
	if err := owner.MayAssignRole(user, holdsNames); err == nil {
		t.Error("a company owner assigns a role carrying identity.roles.update")
	}
	// Even the platform fact does not put the names on a COMPANY member (G2m S1a): they are inert there.
	if err := opActor.MayChangeOverrides(user, authority.Overrides{Granted: []authority.Override{{Name: p.Roles.Update}}}); !errors.Is(err, authority.ErrGrantExceedsAuthority) {
		t.Errorf("a company actor with the platform fact grants identity.roles.update to a company member: %v, want ErrGrantExceedsAuthority", err)
	}
	// Ordinary names are unaffected for the company owner.
	if err := owner.MayChangeOverrides(user, authority.Overrides{Granted: []authority.Override{{Name: p.Crm.Leads.Assign}}}); err != nil {
		t.Errorf("owner grants an ordinary permission by override: %v", err)
	}
}
