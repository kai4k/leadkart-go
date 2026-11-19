package authn

import (
	"context"

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
	a, err := authority.NewActor(authority.ActorInput{
		MembershipID:         membership.ID(c.MembershipID),
		TenantID:             tenant.ID(c.TenantID),
		Rank:                 c.Rank,
		EffectivePermissions: EffectivePermissions(c),
		IsSuperUser:          c.IsSuperUser,
		IsPlatformOperator:   IsPlatformOperator(c),
		IsImpersonation:      c.IsImpersonation(),
	})
	if err != nil {
		return authority.Actor{}
	}
	return a
}
