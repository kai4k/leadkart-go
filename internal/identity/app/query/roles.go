package query

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/rolehierarchy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// RoleView is the wire-shape of a [role.Role] for read endpoints.
//
// ParentRoleID (ADR 0058) — empty when the role is a root. Populated
// by JOIN to identity.role_hierarchy_edges at read time; the Role
// aggregate itself no longer carries the field (hierarchy is its
// own aggregate per Wave 9.4).
type RoleView struct {
	ID              string
	TenantID        string
	Name            string
	IsSystemDefault bool
	IsSuperAdmin    bool
	HierarchyLevel  int
	Permissions     []string
	CreatedAt       time.Time
	ParentRoleID    string
}

// ----- GetRoleQuery --------------------------------------------------------

// GetRoleQuery returns the role detail by ID.
type GetRoleQuery struct {
	TenantID tenant.ID
	RoleID   role.ID
}

// GetRoleHandler runs the read.
type GetRoleHandler struct {
	roles role.Repository
	edges rolehierarchy.Repository
}

// NewGetRoleHandler wires the handler. `edges` may be nil in test
// fixtures that don't exercise the parent-population path; the
// handler treats nil as "always root" for those callers.
func NewGetRoleHandler(r role.Repository, edges rolehierarchy.Repository) GetRoleHandler {
	if r == nil {
		panic("query: NewGetRoleHandler roles repository required")
	}
	return GetRoleHandler{roles: r, edges: edges}
}

// Handle returns the [RoleView] or [role.ErrNotFound]. Performs a
// secondary lookup against [rolehierarchy.Repository] to populate the
// view's ParentRoleID field (cheap — single indexed lookup on the
// edges table; bounded tenant catalog).
func (h GetRoleHandler) Handle(ctx context.Context, q GetRoleQuery) (RoleView, error) {
	if q.TenantID.IsZero() {
		return RoleView{}, errors.New("get_role: tenant id required")
	}
	if q.RoleID.IsZero() {
		return RoleView{}, errors.New("get_role: role id required")
	}
	r, err := h.roles.GetByID(ctx, q.TenantID, q.RoleID)
	if err != nil {
		return RoleView{}, fmt.Errorf("get_role: %w", err)
	}
	parentID, err := h.lookupParent(ctx, q.TenantID, r.ID())
	if err != nil {
		return RoleView{}, fmt.Errorf("get_role: lookup parent: %w", err)
	}
	return projectRole(r, parentID), nil
}

// lookupParent returns the active parent role for `id` OR empty
// string when the role is a root. ErrEdgeNotFound is the expected
// "no parent" signal — not an error.
func (h GetRoleHandler) lookupParent(ctx context.Context, tenantID tenant.ID, id role.ID) (string, error) {
	if h.edges == nil {
		return "", nil
	}
	edge, err := h.edges.GetActiveByChild(ctx, tenantID, id)
	if errors.Is(err, rolehierarchy.ErrEdgeNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return edge.ParentRoleID().String(), nil
}

// ----- ListRolesQuery ------------------------------------------------------

// ListRolesQuery lists every live role in the supplied tenant.
type ListRolesQuery struct {
	TenantID tenant.ID
}

// ListRolesHandler runs the list.
type ListRolesHandler struct {
	roles role.Repository
	edges rolehierarchy.Repository
}

// NewListRolesHandler wires the handler. `edges` is optional for
// fixtures (see GetRoleHandler's commentary).
func NewListRolesHandler(r role.Repository, edges rolehierarchy.Repository) ListRolesHandler {
	if r == nil {
		panic("query: NewListRolesHandler roles repository required")
	}
	return ListRolesHandler{roles: r, edges: edges}
}

// Handle returns [RoleView]s ordered by hierarchy_level, name.
// Parent population: for each role we look up its active edge. At
// the per-tenant catalog sizes BRD line 241 implies (≤30 roles
// typical) this is N small indexed lookups — cheaper than a JOIN-
// heavy alternative + avoids leaking edge schema into the roles
// sqlc query. If a future tenant grows the catalog into the
// thousands we'd swap this to a single bulk query.
func (h ListRolesHandler) Handle(ctx context.Context, q ListRolesQuery) ([]RoleView, error) {
	if q.TenantID.IsZero() {
		return nil, errors.New("list_roles: tenant id required")
	}
	roles, err := h.roles.ListByTenant(ctx, q.TenantID)
	if err != nil {
		return nil, fmt.Errorf("list_roles: %w", err)
	}
	out := make([]RoleView, 0, len(roles))
	for _, r := range roles {
		parentID, perr := h.lookupParent(ctx, q.TenantID, r.ID())
		if perr != nil {
			return nil, fmt.Errorf("list_roles: lookup parent for %s: %w", r.ID(), perr)
		}
		out = append(out, projectRole(r, parentID))
	}
	return out, nil
}

func (h ListRolesHandler) lookupParent(ctx context.Context, tenantID tenant.ID, id role.ID) (string, error) {
	if h.edges == nil {
		return "", nil
	}
	edge, err := h.edges.GetActiveByChild(ctx, tenantID, id)
	if errors.Is(err, rolehierarchy.ErrEdgeNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return edge.ParentRoleID().String(), nil
}

// ----- helpers -------------------------------------------------------------

func projectRole(r *role.Role, parentID string) RoleView {
	perms := r.Permissions()
	names := make([]string, len(perms))
	for i, p := range perms {
		names[i] = p.Name()
	}
	return RoleView{
		ID:              r.ID().String(),
		TenantID:        r.TenantID().String(),
		Name:            r.Name(),
		IsSystemDefault: r.IsSystemDefault(),
		IsSuperAdmin:    r.IsSuperAdmin(),
		HierarchyLevel:  r.HierarchyLevel(),
		Permissions:     names,
		CreatedAt:       r.CreatedAt().UTC(),
		ParentRoleID:    parentID,
	}
}
