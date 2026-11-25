package authn

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"

	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// AuthorityFrom builds the caller's authority.Actor from the verified CLAIMS on
// ctx (ADR 0069). It never reads the tenancy context: under X-Tenant-Id or path
// re-binding that tenant is the SCOPE tenant, while the actor's home tenant,
// rank and effective permissions are the caller's own (the policy compares the
// actor's home tenant with the target's to decide the operator exemption).
//
// An absent rank claim reaches the policy as nil, i.e. RankNone (no authority),
// never as 0 (owner). Fail closed: without claims, or when the actor cannot be
// built (malformed ids, impersonation with platform authority), it returns the
// zero Actor, which denies every decision.
func AuthorityFrom(ctx context.Context) authority.Actor {
	c, ok := ClaimsFromContext(ctx)
	if !ok || c == nil {
		return authority.Actor{}
	}
	own := tenancy.ID(c.TenantID)
	// "the platform tenant" is a fact about the token's tenant (its slug), true for an operator, a
	// platform super user and an impersonation session opened there alike (G2o B1).
	var platformTenant tenant.ID
	if c.TenantSlug == PlatformTenantSlug {
		platformTenant = tenant.ID(c.TenantID)
	}
	a, err := authority.NewActor(authority.ActorInput{
		MembershipID:         membership.ID(c.MembershipID),
		TenantID:             tenant.ID(c.TenantID),
		Rank:                 c.Rank,
		EffectivePermissions: EffectivePermissions(c),
		IsSuperUser:          c.IsSuperUser,
		IsPlatformOperator:   IsPlatformOperator(c),
		IsImpersonation:      c.IsImpersonation(),
		MayManageRoles:       mayManageRoles(c, own),
		PlatformTenantID:     platformTenant,
	})
	if err != nil {
		return authority.Actor{}
	}
	return a
}

// mayManageRoles is the fact the domain policy carries: the caller satisfies a role-management name
// in its own tenant through platform authority (ADR 0073 addendum). The three names share one
// rule (IsPlatformManagedRole), so any of them proves it.
func mayManageRoles(c *jwt.Claims, own tenancy.ID) bool {
	p := permission.IdentityPermissions.Roles
	return satisfiesIn(c, own, p.Update) || satisfiesIn(c, own, p.Create) || satisfiesIn(c, own, p.Manage)
}
