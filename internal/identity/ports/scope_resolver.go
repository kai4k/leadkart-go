package ports

import (
	"context"
	"errors"

	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// TenantScopeResolver adapts the GetTenant query to authn.TenantLookupFunc.
type TenantScopeResolver struct{ a app.Application }

// NewTenantScopeResolver wires the resolver used by authn.TenantScope.
func NewTenantScopeResolver(a app.Application) TenantScopeResolver { return TenantScopeResolver{a: a} }

// Lookup classifies a header-supplied target tenant. Suspended and
// pending-deletion tenants are valid scope targets for reads and lifecycle
// routes (operators must be able to inspect and reactivate / restore them)
// but not for other writes; deleted and missing tenants are unknown.
func (r TenantScopeResolver) Lookup(ctx context.Context, id string) (authn.TenantScopeState, error) {
	v, err := r.a.Queries.GetTenant.Handle(ctx, query.GetTenantQuery{TenantID: tenant.ID(id)})
	if errors.Is(err, tenant.ErrNotFound) {
		return authn.TenantScopeUnknown, nil
	}
	if err != nil {
		return authn.TenantScopeUnknown, err
	}
	switch v.Status {
	case tenant.StatusDeleted.String():
		return authn.TenantScopeUnknown, nil
	case tenant.StatusSuspended.String(), tenant.StatusPendingDeletion.String():
		return authn.TenantScopeInactive, nil
	default:
		return authn.TenantScopeActive, nil
	}
}
