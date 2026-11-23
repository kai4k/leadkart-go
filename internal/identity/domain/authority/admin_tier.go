package authority

import (
	"fmt"
	"slices"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// The admin-tier-only rule (owner ruling 2026-11-21, ADR 0069 "Decided by the
// owner"): "SuperAdmin and Platform Team, Company Admin and Company Owner are
// allowed to add users only."
//
// A tenant member may add users only at the Administrator tier or above (rank
// <= RankAdmin: the Company Owner, the Administrator and every role seeded or
// created inside that band). The platform side (a super user, an operator
// holding platform.users.create or platform.users.manage, an impersonation
// session, which is Admin-equivalent) passes by the platform rule, not by tenant
// tier. The permission that adds users is therefore HELD and CONFERRED only at
// that tier, and this file is the one place the rule lives:
//
//   - [AdminTierOnlyPermissions] is the exact set;
//   - [Actor.MayCreateMembers] is the command's decision (defence in depth behind
//     the route gate, on the fresh actor);
//   - [Actor.MayRequest] and the checks in mayDecide, MayAssignRole, MayCreateRole,
//     MayEditRole, MayReplaceRolePermissions, MayGrantRolePermission and
//     MayChangeOverrides keep it from being conferred below the tier by a role, an
//     override, a request or an approval (a time-bound grant is an approval);
//   - [MayHoldAt] is what the request gate and the capabilities projection read,
//     so a stale grant in existing data is inert (it never reaches
//     effective_permissions and never passes the gate).

// adminTierOnly is the set. Every route and command that adds a member funnels
// through identity.users.create: POST /api/v1/users is the only one (tenant
// registration adds the first Company Owner of a NEW tenant and is operator-only
// behind platform.tenants.create; the bootstrap CLI is not a route). The company
// commerce settings joined it (owner ruling 2026-11-23, ADR 0070): only the
// company's Owner / Admin tier changes them, so company_settings.manage and
// company_settings.tax.manage are held and conferred only at that tier too.
func adminTierOnly() []string {
	p := permission.IdentityPermissions
	return []string{p.Users.Create, p.CompanySettings.Manage, p.CompanySettings.TaxManage}
}

// AdminTierOnlyPermissions is the exact set of permissions that only the
// Administrator tier or above may hold or confer. The slice is a copy.
func AdminTierOnlyPermissions() []string { return adminTierOnly() }

// IsAdminTierOnly reports whether name is in [AdminTierOnlyPermissions].
func IsAdminTierOnly(name string) bool { return slices.Contains(adminTierOnly(), name) }

// MayHoldAt reports whether a member of the given rank may hold name: every name
// outside the set, and the names in it only at rank <= RankAdmin. It is the one
// test the gate and the projection use.
func MayHoldAt(rank Rank, name string) bool { return !IsAdminTierOnly(name) || rank <= RankAdmin }

// RankFromClaim reads the optional rank claim: absent is RankNone (never owner),
// out of range is RankNone ([ClampRank]).
func RankFromClaim(claim *int) Rank {
	if claim == nil {
		return RankNone
	}
	return ClampRank(*claim)
}

// atAdminTier: the actor is the Administrator tier or above (an impersonation
// session is Admin-equivalent: NewActor forces rank 10).
func (a Actor) atAdminTier() bool { return a.valid && a.Rank() <= RankAdmin }

// MayCreateMembers decides whether the actor may add a user to tenantID: a super
// user; a platform operator holding platform.users.create or platform.users.manage
// (the platform rule, in any tenant including the platform's own); otherwise a
// member of THAT tenant at the Administrator tier or above who holds
// identity.users.create. A member below the tier is refused even when a stale
// grant gives them the permission. It is asked on the fresh actor, inside the
// command.
func (a Actor) MayCreateMembers(tenantID tenant.ID) error {
	pp := permission.IdentityPermissions.Platform
	switch {
	case !a.valid:
		return ErrNoAuthority
	case a.superUser:
		return nil
	case a.platformOperator && (a.hasOwn(pp.UsersCreate) || a.hasOwn(pp.UsersManage)):
		return nil
	case !sameTenant(tenantID, a.tenantID):
		return ErrNoAuthority
	case !a.atAdminTier():
		return fmt.Errorf("%w: rank %d may not add users", ErrAdminTierOnly, a.Rank())
	case !a.hasOwn(permission.IdentityPermissions.Users.Create):
		return fmt.Errorf("%w: needs %s", ErrNoAuthority, permission.IdentityPermissions.Users.Create)
	}
	return nil
}

// NeedsFreshFactsToCreate reports whether MayCreateMembers must be asked of the
// actor re-read inside the transaction ([Actor.NarrowedBy]): everyone who acts as
// a member of the tenant does, but an actor admitted by the platform rule in a
// tenant other than their own (a super user, an operator holding
// platform.users.create or platform.users.manage) has no membership there to
// re-read; their authority is the permission, bounded by the security stamp, as
// for every cross-tenant operator act.
func (a Actor) NeedsFreshFactsToCreate(tenantID tenant.ID) bool {
	if !a.valid || sameTenant(tenantID, a.tenantID) {
		return a.NeedsFreshFacts(tenantID)
	}
	pp := permission.IdentityPermissions.Platform
	byPlatformRule := a.superUser || (a.platformOperator && (a.hasOwn(pp.UsersCreate) || a.hasOwn(pp.UsersManage)))
	return !byPlatformRule && a.NeedsFreshFacts(tenantID)
}

// MayRequest decides whether the actor may ASK for name: a name in the admin-tier
// set is requested only by a member at the tier (the approval re-checks the
// requester's tier on fresh facts).
func (a Actor) MayRequest(name string) error {
	if !a.valid {
		return ErrNoAuthority
	}
	if IsAdminTierOnly(name) && !a.atAdminTier() {
		return fmt.Errorf("%w: %s may not be requested below the Administrator tier", ErrAdminTierOnly, name)
	}
	return nil
}

// tierRefusal is the first name in names that a holder of the given rank may not
// hold, as an error; nil when every name may be held there.
func tierRefusal(rank Rank, what string, names []string) error {
	for _, n := range names {
		if !MayHoldAt(rank, n) {
			return fmt.Errorf("%w: %s may not be given to %s (rank %d)", ErrAdminTierOnly, n, what, rank)
		}
	}
	return nil
}

// roleTierRefusal: a role's holders have at least its level as their rank, so a
// role below the tier may not contain an admin-tier-only name.
func roleTierRefusal(level Rank, names []string) error {
	return tierRefusal(level, "a role at that level", names)
}

// roleMayContain is [roleTierRefusal] at r's own effective level.
func roleMayContain(r *role.Role, names []string) error {
	return roleTierRefusal(levelOf(r), names)
}
