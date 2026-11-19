// Package authority is the identity module's single authorization policy for
// authority-changing writes (ADR 0069): rank (who may act on whom), the grant
// ceiling (what may be conferred or removed), approval eligibility, and the
// owner invariants. Pure: no ctx, no I/O, no HTTP. Commands load the facts
// inside their locked transaction and ask; the HTTP port builds the Actor once.
//
// Fail-closed doctrine: the zero value and every unchecked input give the most
// restrictive answer. The zero Actor and the zero Member deny every decision,
// an omitted rank is RankNone, and ids are compared only in canonical form.
package authority

import (
	"fmt"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ActorInput carries the verified facts NewActor validates. TenantID is the
// caller's OWN (claims) tenant, never the request's re-bound scope tenant.
type ActorInput struct {
	MembershipID         membership.ID
	TenantID             tenant.ID
	Rank                 *int     // the rank claim; nil (claim absent) is RankNone, never owner
	EffectivePermissions []string // satisfied in the caller's own tenant; a tenant.admin entry is expanded by NewActor
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

// NewActor validates in. Both ids are canonicalised (a spelling that does not
// parse is ErrNoAuthority). An impersonation session is Admin-equivalent
// whatever rank it claims (ADR 0069 H2) and never platform or super.
func NewActor(in ActorInput) (Actor, error) {
	mid, err := membership.ParseID(string(in.MembershipID))
	if err != nil {
		return Actor{}, fmt.Errorf("%w: membership id is not a canonical uuid", ErrNoAuthority)
	}
	tid, err := tenant.ParseID(string(in.TenantID))
	if err != nil {
		return Actor{}, fmt.Errorf("%w: tenant id is not a canonical uuid", ErrNoAuthority)
	}
	if in.IsImpersonation && (in.IsPlatformOperator || in.IsSuperUser) {
		return Actor{}, fmt.Errorf("%w: an impersonation session carries no platform or super-user authority", ErrNoAuthority)
	}
	rank := RankNone
	if in.Rank != nil {
		rank = ClampRank(*in.Rank)
	}
	if in.IsImpersonation {
		rank = RankAdmin
	}
	eff := make(map[string]struct{}, len(in.EffectivePermissions))
	for _, n := range in.EffectivePermissions {
		if permission.IsKnown(n) {
			eff[n] = struct{}{}
		}
	}
	if _, ok := eff[tenantAdminName()]; ok {
		for _, p := range permission.All() {
			if permission.ImpliedByTenantAdmin(p.Name()) {
				eff[p.Name()] = struct{}{}
			}
		}
	}
	return Actor{valid: true, membershipID: mid, tenantID: tid, rank: rank, effective: eff,
		superUser: in.IsSuperUser, platformOperator: in.IsPlatformOperator, impersonation: in.IsImpersonation}, nil
}

// Valid reports whether a was built by NewActor (the zero Actor is invalid and denies everything).
func (a Actor) Valid() bool { return a.valid }

// MembershipID is the caller's own membership id (canonical, from claims).
func (a Actor) MembershipID() membership.ID { return a.membershipID }

// TenantID is the caller's own tenant (canonical, from claims), never the re-bound scope tenant.
func (a Actor) TenantID() tenant.ID { return a.tenantID }

// IsSuperUser reports whether the caller is a platform super user.
func (a Actor) IsSuperUser() bool { return a.valid && a.superUser }

// IsPlatformOperator reports whether the caller is a slug-anchored, non-impersonation platform operator.
func (a Actor) IsPlatformOperator() bool { return a.valid && a.platformOperator }

// IsImpersonation reports whether the caller is an impersonation session.
func (a Actor) IsImpersonation() bool { return a.valid && a.impersonation }

// Rank is RankNone for the zero Actor (never "owner"). Every rank comparison
// in this package reads it through here, never the raw field.
func (a Actor) Rank() Rank {
	if !a.valid {
		return RankNone
	}
	return a.rank
}

// holds reports whether the actor has the permission in a tenant it acts on:
// super users hold everything known; an operator exempt for that tenant holds
// tenant.admin and everything it implies (never platform-reserved names);
// everyone else holds their own effective set. It is NOT the grant ceiling
// (see MayChangePermissions): holding tenant.admin does not allow conferring it.
func (a Actor) holds(name string, tenantID tenant.ID) bool {
	switch {
	case !a.valid || !permission.IsKnown(name):
		return false
	case a.superUser:
		return true
	case a.exemptOperatorIn(tenantID):
		return name == tenantAdminName() || permission.ImpliedByTenantAdmin(name)
	}
	return a.hasOwn(name)
}

func (a Actor) hasOwn(name string) bool { _, ok := a.effective[name]; return ok }
