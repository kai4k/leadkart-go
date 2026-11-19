package authority_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

func TestRankOf_CustomLevelZeroIsNotOwner(t *testing.T) {
	t.Parallel()
	custom := mkRole(1, tA, "Imposter", false, 0, false)
	if got := authority.RankOf([]*role.Role{custom}); got != authority.Rank(1) {
		t.Fatalf("custom level-0 role: RankOf = %d, want 1 (only the CompanyOwner system role is rank 0)", got)
	}
	owner := mkRole(2, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false)
	if got := authority.RankOf([]*role.Role{owner}); got != authority.RankOwner {
		t.Fatalf("CompanyOwner: RankOf = %d, want 0", got)
	}
}

func TestRank_TierOutOfRangeIsUser(t *testing.T) {
	t.Parallel()
	for _, n := range []int{-1, -100, 100, 1000} {
		if got := authority.Rank(n).Tier(); got != authority.TierUser {
			t.Errorf("Rank(%d).Tier() = %s, want user", n, got)
		}
	}
}

func TestOperator_InOwnTenantFollowsRank(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	ownerR := mkRole(1, tP, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	mgrR := mkRole(3, tP, role.SystemRoles.Tenant.SalesManager, false, 20, false)
	userR := mkRole(5, tA, role.SystemRoles.Tenant.SalesExecutive, false, 50, false)
	platformOwner := mkMember(t, 31, tP, true, "", ownerR)
	platformMgr := mkMember(t, 32, tP, true, "", mgrR)
	foreignUser := mkMember(t, 33, tA, true, "", userR)
	op := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(91)), TenantID: tP, Rank: rk(50), IsPlatformOperator: true, EffectivePermissions: []string{p.Platform.UsersManage}})

	if err := op.MayActOn(platformOwner); !errors.Is(err, authority.ErrInsufficientRank) {
		t.Errorf("operator on own-tenant owner: %v, want insufficient rank", err)
	}
	if err := op.MayActOn(platformMgr); !errors.Is(err, authority.ErrInsufficientRank) {
		t.Errorf("operator (rank 50) on own-tenant manager: %v, want insufficient rank", err)
	}
	if err := op.MayActOn(foreignUser); err != nil {
		t.Errorf("operator on another tenant's member: %v, want nil", err)
	}
	if err := op.MayApprove(platformOwner, p.Crm.Leads.Read); !errors.Is(err, authority.ErrInsufficientRank) {
		t.Errorf("operator approving own-tenant owner: %v, want insufficient rank", err)
	}
	if err := op.MayChangePermissions(tP, []string{p.Meta.TenantAdmin}); !errors.Is(err, authority.ErrGrantExceedsAuthority) {
		t.Errorf("operator conferring tenant.admin inside its own tenant: %v, want grant exceeds authority", err)
	}
	if err := op.MayChangePermissions(tA, []string{p.Meta.TenantAdmin}); err != nil {
		t.Errorf("operator conferring tenant.admin in another tenant: %v, want nil", err)
	}
	if err := op.MayAssignRole(mkMember(t, 34, tP, true, ""), ownerR); !errors.Is(err, authority.ErrInsufficientRank) {
		t.Errorf("operator assigning CompanyOwner inside own tenant: %v, want insufficient rank", err)
	}
}

func TestNewActor_CanonicalisesIDsSoSelfRuleHolds(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	ownerR := mkRole(1, tA, role.SystemRoles.Tenant.CompanyOwner, true, 0, false, p.Meta.TenantAdmin)
	self := mkMember(t, 11, tA, true, "", ownerR)
	upper := mkActor(t, authority.ActorInput{MembershipID: membership.ID(strings.ToUpper(self.ID().String())), TenantID: tenant.ID(strings.ToUpper(tA.String())),
		Rank: rk(authority.RankOwner), EffectivePermissions: adminEffective()})
	if err := upper.MayActOn(self); !errors.Is(err, authority.ErrSelfTarget) {
		t.Fatalf("uppercase own membership id: MayActOn(self) = %v, want self target", err)
	}
	if err := upper.MayAssignRole(self, ownerR); !errors.Is(err, authority.ErrSelfTarget) {
		t.Fatalf("uppercase own membership id: MayAssignRole(self) = %v, want self target", err)
	}
	if upper.MembershipID() != self.ID() || upper.TenantID() != tA {
		t.Fatalf("actor ids not canonical: %s / %s", upper.MembershipID(), upper.TenantID())
	}
	for _, bad := range []string{"not-a-uuid", " " + uid(1), uid(1) + "\n", "00000000-0000-0000-0000-000000000000"} {
		if _, err := authority.NewActor(authority.ActorInput{MembershipID: membership.ID(bad), TenantID: tA}); !errors.Is(err, authority.ErrNoAuthority) {
			t.Errorf("membership id %q accepted: %v", bad, err)
		}
		if _, err := authority.NewActor(authority.ActorInput{MembershipID: membership.ID(uid(1)), TenantID: tenant.ID(bad)}); !errors.Is(err, authority.ErrNoAuthority) {
			t.Errorf("tenant id %q accepted: %v", bad, err)
		}
	}
}

func TestNewActor_ExpandsTenantAdmin(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	a := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(1)), TenantID: tA, Rank: rk(authority.RankAdmin),
		EffectivePermissions: []string{p.Meta.TenantAdmin}})
	if err := a.MayChangePermissions(tA, []string{p.Crm.Leads.ReadAll}); err != nil {
		t.Fatalf("admin holding only tenant.admin may not confer read_all: %v", err)
	}
}

func TestMayChangePermissions_UnknownAndEmptyRefusedForEveryone(t *testing.T) {
	t.Parallel()
	super := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(90)), TenantID: tP, Rank: rk(authority.RankOwner), IsSuperUser: true})
	admin := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(13)), TenantID: tA, Rank: rk(authority.RankAdmin), EffectivePermissions: adminEffective()})
	for _, who := range []authority.Actor{super, admin} {
		for _, name := range []string{"", "not.a.permission"} {
			if err := who.MayChangePermissions(tA, []string{name}); !errors.Is(err, authority.ErrGrantExceedsAuthority) {
				t.Errorf("name %q: %v, want grant exceeds authority", name, err)
			}
		}
	}
}

func TestMayAssignRole_RoleTenantMustMatchTargetForEveryone(t *testing.T) {
	t.Parallel()
	userR := mkRole(5, tB, "Exec", false, 50, false)
	target := mkMember(t, 18, tA, true, "")
	super := mkActor(t, authority.ActorInput{MembershipID: membership.ID(uid(90)), TenantID: tP, Rank: rk(authority.RankOwner), IsSuperUser: true})
	if err := super.MayAssignRole(target, userR); !errors.Is(err, authority.ErrNoAuthority) {
		t.Fatalf("super assigning another tenant's role: %v, want no authority", err)
	}
}
