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
// ParentRoleID (ADR 0058) is empty for root roles; populated via the
// role_hierarchy_edges table at read time (hierarchy is its own aggregate).
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

// NewGetRoleHandler wires the handler. edges may be nil; nil is treated as "always root".
func NewGetRoleHandler(r role.Repository, edges rolehierarchy.Repository) GetRoleHandler {
	if r == nil {
		panic("query: NewGetRoleHandler roles repository required")
	}
	return GetRoleHandler{roles: r, edges: edges}
}

// Handle returns the [RoleView] or [role.ErrNotFound].
// Performs a secondary indexed lookup to populate ParentRoleID.
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

// lookupParent returns the active parent role ID, or "" for roots.
// ErrEdgeNotFound is the expected "no parent" signal, not an error.
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

// NewListRolesHandler wires the handler. edges may be nil (treated as "always root").
func NewListRolesHandler(r role.Repository, edges rolehierarchy.Repository) ListRolesHandler {
	if r == nil {
		panic("query: NewListRolesHandler roles repository required")
	}
	return ListRolesHandler{roles: r, edges: edges}
}

// Handle returns [RoleView]s ordered by hierarchy_level, name.
// Per-role parent lookup is N small indexed queries; acceptable for
// the ≤30-role catalogs BRD §241 implies. Bulk if catalogs grow.
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
