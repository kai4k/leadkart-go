// Package authority is the identity module's single authorization policy for
// authority-changing writes (ADR 0069): rank (who may act on whom), the grant
// ceiling (what may be conferred or removed), approval eligibility, and the
// owner invariants. Pure: no ctx, no I/O, no HTTP. Commands load the facts
// inside their locked transaction and ask; the HTTP port builds the Actor once.
package authority

import (
	"fmt"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ActorInput carries the verified facts NewActor validates. TenantID is the
// caller's OWN (claims) tenant — never the request's re-bound scope tenant.
type ActorInput struct {
	MembershipID         membership.ID
	TenantID             tenant.ID
	Rank                 Rank
	EffectivePermissions []string // satisfied in the caller's own tenant (tenant.admin expanded)
	IsSuperUser          bool
	IsPlatformOperator   bool // slug-anchored, non-impersonation
	IsImpersonation      bool
}

// Actor is the authority of the caller performing a write. Carried
// explicitly in every authority-changing command (tdl_canon §4). The zero
// Actor holds no authority: every decision denies.
type Actor struct {
	valid            bool
	membershipID     membership.ID
	tenantID         tenant.ID
	rank             Rank
	effective        map[string]struct{}
	superUser        bool
	platformOperator bool
	impersonation    bool
}

// NewActor validates in. An impersonation session is Admin-equivalent
// whatever rank it claims (ADR 0069 H2) and never platform or super.
func NewActor(in ActorInput) (Actor, error) {
	if in.MembershipID.IsZero() {
		return Actor{}, fmt.Errorf("%w: membership id required", ErrNoAuthority)
	}
	if in.TenantID.IsZero() {
		return Actor{}, fmt.Errorf("%w: tenant id required", ErrNoAuthority)
	}
	if in.IsImpersonation && (in.IsPlatformOperator || in.IsSuperUser) {
		return Actor{}, fmt.Errorf("%w: an impersonation session carries no platform or super-user authority", ErrNoAuthority)
	}
	rank := ClampRank(int(in.Rank))
	if in.IsImpersonation {
		rank = RankAdmin
	}
	eff := make(map[string]struct{}, len(in.EffectivePermissions))
	for _, n := range in.EffectivePermissions {
		if permission.IsKnown(n) {
			eff[n] = struct{}{}
		}
	}
	return Actor{valid: true, membershipID: in.MembershipID, tenantID: in.TenantID, rank: rank, effective: eff,
		superUser: in.IsSuperUser, platformOperator: in.IsPlatformOperator, impersonation: in.IsImpersonation}, nil
}

// Valid reports whether a was built by NewActor (the zero Actor is invalid and denies everything).
func (a Actor) Valid() bool { return a.valid }

// MembershipID is the caller's own membership id (from claims).
func (a Actor) MembershipID() membership.ID { return a.membershipID }

// TenantID is the caller's own tenant (from claims), never the re-bound scope tenant.
func (a Actor) TenantID() tenant.ID { return a.tenantID }

// IsSuperUser reports whether the caller is a platform super user.
func (a Actor) IsSuperUser() bool { return a.valid && a.superUser }

// IsPlatformOperator reports whether the caller is a slug-anchored, non-impersonation platform operator.
func (a Actor) IsPlatformOperator() bool { return a.valid && a.platformOperator }

// IsImpersonation reports whether the caller is an impersonation session.
func (a Actor) IsImpersonation() bool { return a.valid && a.impersonation }

// Rank is RankNone for the zero Actor (never "owner").
func (a Actor) Rank() Rank {
	if !a.valid {
		return RankNone
	}
	return a.rank
}

// Holds reports whether a may confer, remove or approve name in a tenant it
// acts on: super users hold everything; exempt operators hold tenant.admin and
// everything it implies (never platform-reserved names); everyone else holds
// their own effective set.
func (a Actor) Holds(name string) bool {
	switch {
	case !a.valid || !permission.IsKnown(name):
		return false
	case a.superUser:
		return true
	case a.operatorExempt():
		return name == tenantAdminName() || permission.ImpliedByTenantAdmin(name)
	}
	return a.hasOwn(name)
}

func (a Actor) hasOwn(name string) bool { _, ok := a.effective[name]; return ok }
