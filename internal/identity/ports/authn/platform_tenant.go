package authn

import (
	"context"
	"net/http"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// PlatformTenantFunc resolves the id of LeadKart's own tenant (slug "platform") from the database.
// It returns "" when the tenant cannot be resolved; callers then fall back to what the token itself
// proves (see [AuthorityFrom]). A func type, not an interface: ports/ never declares interfaces.
type PlatformTenantFunc func(ctx context.Context) tenant.ID

type platformTenantKey struct{}

// WithPlatformTenant returns ctx carrying the resolver [AuthorityFrom] reads.
func WithPlatformTenant(ctx context.Context, resolve PlatformTenantFunc) context.Context {
	return context.WithValue(ctx, platformTenantKey{}, resolve)
}

// PlatformTenant installs the resolver on every request behind it. Wrap the whole mux, next to
// [TenantScope]. The id never comes from the request: the resolver reads the database.
func PlatformTenant(resolve PlatformTenantFunc) func(http.Handler) http.Handler {
	if resolve == nil {
		panic("authn: PlatformTenant resolver required")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(WithPlatformTenant(r.Context(), resolve)))
		})
	}
}

// platformTenantFor is the platform tenant's id for the caller: the resolver's answer when it has one,
// else the token's own tenant when its slug is the platform slug, else unknown (empty).
func platformTenantFor(ctx context.Context, tokenTenantID, tokenSlug string) tenant.ID {
	if resolve, ok := ctx.Value(platformTenantKey{}).(PlatformTenantFunc); ok && resolve != nil {
		if id := resolve(ctx); id != "" {
			return id
		}
	}
	if tokenSlug == PlatformTenantSlug {
		return tenant.ID(tokenTenantID)
	}
	return ""
}
