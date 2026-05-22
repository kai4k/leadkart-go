package query

import (
	"context"
	"errors"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/common/cache"
)

// CapabilitiesView is the enriched read shape for
// GET /v1/auth/me/capabilities.
//
// JWT-resident fields (person_id, membership_id, tenant_id,
// tenant_slug, is_platform, is_super_user, permissions) come straight
// from the verified claims and are projected by the HTTP layer. The
// FIELDS in this struct are the enrichment that needs a DB lookup:
//   - Profile (email, first_name, last_name) from Person
//   - Resolved Roles (id, name, is_super_admin) from Membership +
//     RoleRepo.GetByIDs
//
// The cache key includes the security_stamp; stamp rotation (password
// change, role grant/revoke, anonymise) automatically invalidates per
// ADR 0028 + ADR 0042 — TTL is just a memory bound.
type CapabilitiesView struct {
	Email     string
	FirstName string
	LastName  string
	Roles     []CapabilityRoleView
}

// CapabilityRoleView is one resolved Role surface in the capabilities
// bundle. Carries display name (drives the UI "your role: X" widget)
// + the is_super_admin flag so the frontend can render the SuperAdmin
// chip / unlock special UX.
type CapabilityRoleView struct {
	ID           string
	Name         string
	IsSuperAdmin bool
}

// GetCapabilitiesQuery resolves the enrichment fields for one
// membership. SecurityStamp comes from the JWT (cache-key component);
// it's not used by the underlying repository calls — those run under
// the standard tenant scope.
type GetCapabilitiesQuery struct {
	PersonID      person.ID
	MembershipID  membership.ID
	TenantID      tenant.ID
	SecurityStamp string
}

// GetCapabilitiesHandler is the un-cached source-of-truth resolver.
// Two DB round-trips: Person.GetByID + RoleRepo.GetByIDs (Membership
// is already on the JWT — no third hit needed; we read its role
// assignments via Membership.GetByID).
//
// Wrap with CachedGetCapabilitiesHandler at the composition root —
// the HTTP handler always talks to the cached wrapper via
// Application.Queries.
type GetCapabilitiesHandler struct {
	persons     person.Repository
	memberships membership.Repository
	roles       role.Repository
}

// NewGetCapabilitiesHandler wires the un-cached handler.
func NewGetCapabilitiesHandler(p person.Repository, m membership.Repository, r role.Repository) GetCapabilitiesHandler {
	if p == nil {
		panic("query: NewGetCapabilitiesHandler persons repository required")
	}
	if m == nil {
		panic("query: NewGetCapabilitiesHandler memberships repository required")
	}
	if r == nil {
		panic("query: NewGetCapabilitiesHandler roles repository required")
	}
	return GetCapabilitiesHandler{persons: p, memberships: m, roles: r}
}

// Handle resolves the enrichment for the supplied membership.
// Returns an empty view (zero-fields, empty Roles slice) without
// error if either the Person or Membership is missing — capabilities
// is a read-only enrichment surface; absent rows are not a hard
// failure (the caller can still rely on JWT-resident fields).
func (h GetCapabilitiesHandler) Handle(ctx context.Context, q GetCapabilitiesQuery) (CapabilitiesView, error) {
	if q.PersonID.IsZero() {
		return CapabilitiesView{}, errors.New("get_capabilities: person id required")
	}
	if q.MembershipID.IsZero() {
		return CapabilitiesView{}, errors.New("get_capabilities: membership id required")
	}

	p, err := h.persons.GetByID(ctx, q.PersonID)
	if err != nil {
		// Person missing is a data-integrity issue (we have a valid
		// JWT for this person but they're not in the DB). Surface as
		// generic error rather than silently returning an empty view —
		// the operator wants to see this in the logs.
		return CapabilitiesView{}, fmt.Errorf("get_capabilities: load person: %w", err)
	}

	m, err := h.memberships.GetByID(ctx, q.MembershipID)
	if err != nil {
		return CapabilitiesView{}, fmt.Errorf("get_capabilities: load membership: %w", err)
	}

	roleIDs := m.RoleAssignments()
	var rolesViews []CapabilityRoleView
	if len(roleIDs) > 0 {
		roles, rerr := h.roles.GetByIDs(ctx, roleIDs)
		if rerr != nil {
			return CapabilitiesView{}, fmt.Errorf("get_capabilities: load roles: %w", rerr)
		}
		rolesViews = make([]CapabilityRoleView, 0, len(roles))
		for _, r := range roles {
			rolesViews = append(rolesViews, CapabilityRoleView{
				ID:           r.ID().String(),
				Name:         r.Name(),
				IsSuperAdmin: r.IsSuperAdmin(),
			})
		}
	}

	return CapabilitiesView{
		Email:     p.Email().String(),
		FirstName: p.FirstName(),
		LastName:  p.LastName(),
		Roles:     rolesViews,
	}, nil
}

// CachedGetCapabilitiesHandler is the cache-wrapped facade in front
// of the un-cached resolver. Per ADR 0042 — keyed by (membership_id,
// security_stamp); stamp rotation invalidates implicitly.
//
// Cache key shape: "leadkart:capabilities:m=<membership-id>:s=<stamp>"
// — the stamp IS the invalidation mechanism, so the TTL
// (CapabilitiesTTL = 2min L1 / 15min L2) is just a memory bound.
type CachedGetCapabilitiesHandler struct {
	facade *cache.Facade[capabilitiesCacheKey, CapabilitiesView]
}

// capabilitiesCacheKey scopes the cache key to the immutable inputs
// (membership_id + security_stamp). Other inputs (person_id,
// tenant_id) are derived from the membership, so omitting them keeps
// the key short.
type capabilitiesCacheKey struct {
	MembershipID  string
	SecurityStamp string
}

// NewCachedGetCapabilitiesHandler builds the facade. Factory closure
// reconstructs the query from the cache key, then dispatches to the
// inner un-cached handler — but the factory needs the PersonID +
// TenantID which AREN'T in the cache key.
//
// Trade-off: the factory reads the membership first (1 DB hit) to
// resolve PersonID + TenantID, then calls the un-cached handler. On
// cache hit, neither lookup runs. The cost-of-miss is the same as
// the un-cached path (the un-cached handler also reads membership).
// Cache hit ratio target per ADR 0042 (Capabilities profile) is high
// — a single operator/user session repeats this call many times
// across page loads.
func NewCachedGetCapabilitiesHandler(inner GetCapabilitiesHandler, hc *cache.HybridCache, m membership.Repository) CachedGetCapabilitiesHandler {
	if hc == nil {
		panic("query: NewCachedGetCapabilitiesHandler hybrid cache required")
	}
	if m == nil {
		panic("query: NewCachedGetCapabilitiesHandler memberships repository required for factory")
	}
	facade := cache.NewFacade[capabilitiesCacheKey, CapabilitiesView](
		hc, "capabilities",
		func(k capabilitiesCacheKey) string {
			return "leadkart:capabilities:m=" + k.MembershipID + ":s=" + k.SecurityStamp
		},
		func(ctx context.Context, k capabilitiesCacheKey) (CapabilitiesView, error) {
			// Resolve the membership first to derive PersonID + TenantID.
			// We can't put those in the cache key (would explode the key
			// space + they're derivable). One DB hit per miss; cache hits
			// avoid it.
			mem, err := m.GetByID(ctx, membership.ID(k.MembershipID))
			if err != nil {
				return CapabilitiesView{}, fmt.Errorf("capabilities cache factory: load membership: %w", err)
			}
			return inner.Handle(ctx, GetCapabilitiesQuery{
				PersonID:      mem.PersonID(),
				MembershipID:  mem.ID(),
				TenantID:      mem.TenantID(),
				SecurityStamp: k.SecurityStamp,
			})
		},
		cache.WithTTL(cache.CapabilitiesTTL()),
	)
	return CachedGetCapabilitiesHandler{facade: facade}
}

// Handle returns the cached enrichment. On miss the factory runs;
// on hit the cached view returns immediately.
func (h CachedGetCapabilitiesHandler) Handle(ctx context.Context, q GetCapabilitiesQuery) (CapabilitiesView, error) {
	return h.facade.Get(ctx, capabilitiesCacheKey{
		MembershipID:  q.MembershipID.String(),
		SecurityStamp: q.SecurityStamp,
	})
}
