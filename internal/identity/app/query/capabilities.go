package query

import (
	"context"
	"errors"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/common/cache"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// CapabilitiesView is the DB-enrichment for GET /v1/auth/me/capabilities.
// JWT-resident fields are projected by the HTTP layer; this struct holds
// the fields that require a DB lookup: Person profile + resolved Roles.
// Cache key includes security_stamp — stamp rotation (password change,
// role grant/revoke, anonymise) invalidates automatically (ADR 0028 + 0042).
type CapabilitiesView struct {
	Email     string
	FirstName string
	LastName  string
	Roles     []CapabilityRoleView
}

// CapabilityRoleView is one resolved Role in the capabilities bundle.
type CapabilityRoleView struct {
	ID           string
	Name         string
	IsSuperAdmin bool
}

// GetCapabilitiesQuery resolves enrichment for one membership.
// SecurityStamp is from the JWT and used only as a cache-key component.
type GetCapabilitiesQuery struct {
	PersonID      person.ID
	MembershipID  membership.ID
	TenantID      tenant.ID
	SecurityStamp string
}

// GetCapabilitiesHandler is the un-cached resolver: two DB round-trips
// (Person.GetByID + RoleRepo.GetByIDs). Wrap with
// CachedGetCapabilitiesHandler at the composition root.
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
func (h GetCapabilitiesHandler) Handle(ctx context.Context, q GetCapabilitiesQuery) (CapabilitiesView, error) {
	if q.PersonID.IsZero() {
		return CapabilitiesView{}, errors.New("get_capabilities: person id required")
	}
	if q.MembershipID.IsZero() {
		return CapabilitiesView{}, errors.New("get_capabilities: membership id required")
	}
	if q.TenantID == "" {
		return CapabilitiesView{}, errors.New("get_capabilities: tenant id required")
	}

	p, err := h.persons.GetByID(ctx, q.PersonID)
	if err != nil {
		// Valid JWT but Person absent = data-integrity violation; surface
		// as error so operators see it in logs rather than silently empty.
		return CapabilitiesView{}, fmt.Errorf("get_capabilities: load person: %w", err)
	}

	m, err := h.memberships.GetByID(ctx, q.TenantID, q.MembershipID)
	if err != nil {
		return CapabilitiesView{}, fmt.Errorf("get_capabilities: load membership: %w", err)
	}

	roleIDs := m.RoleAssignments()
	var rolesViews []CapabilityRoleView
	if len(roleIDs) > 0 {
		roles, rerr := h.roles.GetByIDs(ctx, m.TenantID(), roleIDs)
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

// CachedGetCapabilitiesHandler is the cache-wrapped facade (ADR 0042).
// Keyed by (tenant_id, membership_id, security_stamp); stamp rotation
// is the invalidation mechanism — TTL is just a memory bound.
type CachedGetCapabilitiesHandler struct {
	facade *cache.Facade[capabilitiesCacheKey, CapabilitiesView]
}

// capabilitiesCacheKey scopes the cache entry to (tenant_id, membership_id,
// security_stamp). TenantID is explicit per ADR 0062 (no ctx-scope smuggling).
type capabilitiesCacheKey struct {
	TenantID      string
	MembershipID  string
	SecurityStamp string
}

// NewCachedGetCapabilitiesHandler builds the facade. On cache miss the
// factory reads the membership first (1 DB hit) to resolve PersonID,
// then delegates to the inner handler — same cost as un-cached.
// Cache hit ratio is high per ADR 0042 (same user hits this on every page load).
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
			return "leadkart:capabilities:t=" + k.TenantID + ":m=" + k.MembershipID + ":s=" + k.SecurityStamp
		},
		func(ctx context.Context, k capabilitiesCacheKey) (CapabilitiesView, error) {
			// One DB hit per miss to resolve PersonID; TenantID explicit per ADR 0062.
			mem, err := m.GetByID(ctx, tenant.ID(k.TenantID), membership.ID(k.MembershipID))
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
		TenantID:      q.TenantID.String(),
		MembershipID:  q.MembershipID.String(),
		SecurityStamp: q.SecurityStamp,
	})
}
