package ports

import (
	"context"
	"errors"

	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// TenantScopeResolver adapts the GetTenant query to authn.TenantExistsFunc.
type TenantScopeResolver struct{ a app.Application }

// NewTenantScopeResolver wires the resolver used by authn.TenantScope.
func NewTenantScopeResolver(a app.Application) TenantScopeResolver { return TenantScopeResolver{a: a} }

// TenantExists reports whether id is an existing, non-deleted tenant.
// Suspended and pending-deletion tenants are valid scope targets: operators
// must be able to inspect and reactivate / restore them.
func (r TenantScopeResolver) TenantExists(ctx context.Context, id string) (bool, error) {
	v, err := r.a.Queries.GetTenant.Handle(ctx, query.GetTenantQuery{TenantID: tenant.ID(id)})
	if errors.Is(err, tenant.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v.Status != tenant.StatusDeleted.String(), nil
}
