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
	"time"

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
	// holdUntil is, for a permission the actor holds ONLY through a time-bound
	// override, when that override ends. Set by NarrowedBy from the fresh read;
	// never present on a token-only actor.
	holdUntil map[string]time.Time
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
	return Actor{valid: true, membershipID: mid, tenantID: tid, rank: rank, effective: effectiveSet(in.EffectivePermissions),
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
// everything tenant.admin implies (never platform-reserved names, and never
// tenant.admin itself, which only the ceiling in mayChangePermissions grants);
// everyone else holds their own effective set. It is NOT the grant ceiling
// (see MayChangePermissions): holding tenant.admin does not allow conferring it.
func (a Actor) holds(name string, tenantID tenant.ID) bool {
	switch {
	case !a.valid || !permission.IsKnown(name):
		return false
	case a.superUser:
		return true
	case a.exemptOperatorIn(tenantID):
		return permission.ImpliedByTenantAdmin(name)
	}
	return a.hasOwn(name)
}

func (a Actor) hasOwn(name string) bool { _, ok := a.effective[name]; return ok }

// effectiveSet is the set of known permission names in names, with every name
// tenant.admin implies added when tenant.admin itself is listed. It is the one
// place the expansion happens (NewActor and NarrowedBy both use it).
func effectiveSet(names []string) map[string]struct{} {
	eff := make(map[string]struct{}, len(names))
	for _, n := range names {
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
	return eff
}

// NeedsFreshFacts reports whether a decision inside tenantID must be taken on
// the actor's freshly loaded membership ([Actor.NarrowedBy]) instead of on the
// token alone: every actor except super users (they hold everything and have
// no tenant membership to read), impersonation sessions (a synthetic
// membership; the session store and the operator's stamp bound them) and
// operators acting under the cross-tenant exemption (their authority is the
// platform permission, not a membership of that tenant). The zero Actor needs
// nothing: it already denies.
func (a Actor) NeedsFreshFacts(tenantID tenant.ID) bool {
	return a.valid && !a.superUser && !a.impersonation && !a.exemptOperatorIn(tenantID)
}

// NarrowedBy returns the actor the policy should decide with once the actor's
// own membership has been read again inside the locked transaction: the MORE
// JUNIOR of the token's rank and fresh.Rank(), and the INTERSECTION of the
// token's effective permissions and freshEffective (the permissions freshly
// resolved from the stored roles and overrides, tenant.admin expanded as in
// NewActor). A token can therefore only ever lose authority relative to the
// stored facts, never gain it.
//
// It returns the zero Actor (which denies everything) when fresh is not valid,
// is not the membership the token names (canonical id and tenant comparison),
// or cannot act (inactive membership, or a person who is suspended, locked,
// anonymised or deactivated). Super-user, platform-operator and impersonation
// flags are carried over unchanged; they are not facts of the membership.
//
// freshViaRoles is the union of the permissions of the member's live roles; with
// the member's live overrides it tells which permissions the member holds only
// through a time-bound grant (recorded for [Actor.CapApprovalExpiry]).
func (a Actor) NarrowedBy(fresh Member, freshEffective, freshViaRoles []string) Actor {
	if !a.valid || !fresh.valid || !fresh.active || fresh.id != a.membershipID || !sameTenant(fresh.tenantID, a.tenantID) {
		return Actor{}
	}
	out := a
	out.rank = max(a.rank, fresh.rank)
	out.effective = make(map[string]struct{}, len(a.effective))
	have := effectiveSet(freshEffective)
	for n := range a.effective {
		if _, ok := have[n]; ok {
			out.effective[n] = struct{}{}
		}
	}
	out.holdUntil = timeBoundHoldings(fresh, out.effective, effectiveSet(freshViaRoles))
	return out
}

// timeBoundHoldings: for every permission in held that the member does not get
// from a role or a permanent grant, the latest time-bound grant that confers it
// (a timed tenant.admin grant confers every name it implies until it ends).
func timeBoundHoldings(fresh Member, held, viaRoles map[string]struct{}) map[string]time.Time {
	permanent := make(map[string]struct{})
	timed := make(map[string]time.Time)
	for _, g := range fresh.overrides.Granted {
		if g.ExpiresAt.IsZero() {
			permanent[g.Name] = struct{}{}
		} else if g.ExpiresAt.After(timed[g.Name]) {
			timed[g.Name] = g.ExpiresAt
		}
	}
	if _, ok := permanent[tenantAdminName()]; ok {
		for n := range effectiveSet([]string{tenantAdminName()}) {
			permanent[n] = struct{}{}
		}
	}
	out := make(map[string]time.Time)
	set := func(n string, until time.Time) {
		_, byRole := viaRoles[n]
		_, always := permanent[n]
		if _, ok := held[n]; !ok || byRole || always {
			return
		}
		if cur, ok := out[n]; !ok || until.After(cur) {
			out[n] = until
		}
	}
	for n, until := range timed {
		set(n, until)
	}
	if until, ok := timed[tenantAdminName()]; ok {
		for n := range effectiveSet([]string{tenantAdminName()}) {
			set(n, until)
		}
	}
	return out
}
