package authority_test

import (
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
)

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
	// The same override is the ordinary ceiling for the platform fact.
	if err := opActor.MayChangeOverrides(user, authority.Overrides{Granted: []authority.Override{{Name: p.Roles.Update}}}); err != nil {
		t.Errorf("platform authority grants identity.roles.update by override: %v", err)
	}
	// Ordinary names are unaffected for the company owner.
	if err := owner.MayChangeOverrides(user, authority.Overrides{Granted: []authority.Override{{Name: p.Crm.Leads.Assign}}}); err != nil {
		t.Errorf("owner grants an ordinary permission by override: %v", err)
	}
}
