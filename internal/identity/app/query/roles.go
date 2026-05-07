package query

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// RoleView is the wire-shape of a [role.Role] for read endpoints.
type RoleView struct {
	ID              string
	TenantID        string
	Name            string
	IsSystemDefault bool
	IsSuperAdmin    bool
	HierarchyLevel  int
	Permissions     []string
	CreatedAt       time.Time
}

// ----- GetRoleQuery --------------------------------------------------------

// GetRoleQuery returns the role detail by ID.
type GetRoleQuery struct {
	RoleID role.ID
}

// GetRoleHandler runs the read.
type GetRoleHandler struct {
	roles role.Repository
}

// NewGetRoleHandler wires the handler.
func NewGetRoleHandler(r role.Repository) GetRoleHandler {
	if r == nil {
		panic("query: NewGetRoleHandler roles repository required")
	}
	return GetRoleHandler{roles: r}
}

// Handle returns the [RoleView] or [role.ErrNotFound].
func (h GetRoleHandler) Handle(ctx context.Context, q GetRoleQuery) (RoleView, error) {
	if q.RoleID.IsZero() {
		return RoleView{}, errors.New("get_role: role id required")
	}
	r, err := h.roles.GetByID(ctx, q.RoleID)
	if err != nil {
		return RoleView{}, fmt.Errorf("get_role: %w", err)
	}
	return projectRole(r), nil
}

// ----- ListRolesQuery ------------------------------------------------------

// ListRolesQuery lists every live role in the supplied tenant.
type ListRolesQuery struct {
	TenantID tenant.ID
}

// ListRolesHandler runs the list.
type ListRolesHandler struct {
	roles role.Repository
}

// NewListRolesHandler wires the handler.
func NewListRolesHandler(r role.Repository) ListRolesHandler {
	if r == nil {
		panic("query: NewListRolesHandler roles repository required")
	}
	return ListRolesHandler{roles: r}
}

// Handle returns [RoleView]s ordered by hierarchy_level, name.
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
		out = append(out, projectRole(r))
	}
	return out, nil
}

// ----- helpers -------------------------------------------------------------

func projectRole(r *role.Role) RoleView {
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
	}
}
