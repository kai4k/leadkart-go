package authn

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
)

const (
	errCodeSelfModificationForbidden = "self_modification_forbidden"
	// Bad-id codes mirror ports.ErrCodeInvalid{User,Role,Tenant}ID (the
	// handlers' own codes for the same malformed path value).
	errCodeInvalidUserID   = "invalid_user_id"
	errCodeInvalidTenantID = "invalid_tenant_id"
)

// isPlatformOperator: is_platform AND slug-anchored AND not an impersonation
// token (ADR 0039 slug anchor; ADR 0045 downgrade).
func isPlatformOperator(c *jwt.Claims) bool {
	return c != nil && c.IsPlatform && c.TenantSlug == PlatformTenantSlug && !c.IsImpersonation()
}

// satisfiesIn is THE authorization decision (ADR 0036 + ADR 0068):
// super user → yes; explicit grant → yes; tenant.admin implies every
// non-reserved permission in the holder's OWN tenant; a platform operator's
// platform.* grant satisfies the identity.* equivalent on any tenant.
func satisfiesIn(c *jwt.Claims, effective tenancy.ID, required string) bool {
	if c == nil {
		return false
	}
	if c.IsSuperUser || slices.Contains(c.Permissions, required) {
		return true
	}
	if tenancy.ID(c.TenantID) == effective &&
		slices.Contains(c.Permissions, permission.IdentityPermissions.Meta.TenantAdmin) &&
		permission.ImpliedByTenantAdmin(required) {
		return true
	}
	if isPlatformOperator(c) {
		for _, eq := range permission.PlatformEquivalents(required) {
			if slices.Contains(c.Permissions, eq) {
				return true
			}
		}
	}
	return false
}

// Satisfies reports whether the authenticated caller on ctx may exercise
// permName in the request's effective tenant (tenancy ctx bound by
// RequireAuth; falls back to the claims tenant). Use for handler-inline
// checks (e.g. read_all narrowing) in every module. False without claims.
func Satisfies(ctx context.Context, permName string) bool {
	c, ok := ClaimsFromContext(ctx)
	if !ok {
		return false
	}
	effective, ok := tenancy.FromContext(ctx)
	if !ok || effective.IsZero() {
		effective = tenancy.ID(c.TenantID)
	}
	return satisfiesIn(c, effective, permName)
}

// EffectivePermissions lists every catalogue permission c satisfies in its
// own tenant, sorted — the capabilities projection the frontend gates on.
func EffectivePermissions(c *jwt.Claims) []string {
	out := []string{}
	if c == nil {
		return out
	}
	for _, p := range permission.All() {
		if satisfiesIn(c, tenancy.ID(c.TenantID), p.Name()) {
			out = append(out, p.Name())
		}
	}
	slices.Sort(out)
	return out
}

// RequireTenantPermission gates /tenants/{pathVar}/... routes: freshness-
// checked JWT AND (path tenant == claims tenant OR caller is super user /
// platform operator) AND at least one of permNames satisfied IN THE PATH
// TENANT. Replaces RequireTenantContext for routes that need a permission.
func RequireTenantPermission(verifier Verifier, validator StampValidator, pathVar string, permNames ...string) func(http.Handler) http.Handler {
	if verifier == nil || validator == nil {
		panic("authn: RequireTenantPermission verifier + validator required")
	}
	if strings.TrimSpace(pathVar) == "" {
		panic("authn: RequireTenantPermission pathVar required")
	}
	if len(permNames) == 0 {
		panic("authn: RequireTenantPermission requires ≥1 permission name")
	}
	for _, p := range permNames {
		_ = permission.FromConstant(p)
	}
	auth := RequireFreshStamp(verifier, validator)
	return func(next http.Handler) http.Handler {
		return auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, ok := ClaimsFromContext(r.Context())
			if !ok {
				writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "missing claims")
				return
			}
			urlTenant, ok := CanonicalID(r.PathValue(pathVar))
			if !ok {
				writeError(w, http.StatusBadRequest, errCodeInvalidTenantID, pathVar+" path parameter must be a UUID")
				return
			}
			if !c.IsSuperUser && !isPlatformOperator(c) && !sameID(urlTenant, c.TenantID) {
				writeError(w, http.StatusForbidden, errCodeForbidden, "tenant context does not match url")
				return
			}
			for _, p := range permNames {
				if satisfiesIn(c, tenancy.ID(urlTenant), p) {
					next.ServeHTTP(w, r)
					return
				}
			}
			writeError(w, http.StatusForbidden, errCodeForbidden, "missing any of: "+strings.Join(permNames, ", "))
		}))
	}
}

// RequirePermissionOrSelf passes when the path's membership id is the
// caller's own (self-service read/profile) OR permName is satisfied.
func RequirePermissionOrSelf(verifier Verifier, validator StampValidator, pathVar, permName string) func(http.Handler) http.Handler {
	if verifier == nil || validator == nil {
		panic("authn: RequirePermissionOrSelf verifier + validator required")
	}
	if strings.TrimSpace(pathVar) == "" {
		panic("authn: RequirePermissionOrSelf pathVar required")
	}
	_ = permission.FromConstant(permName)
	auth := RequireFreshStamp(verifier, validator)
	return func(next http.Handler) http.Handler {
		return auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, ok := ClaimsFromContext(r.Context())
			if !ok {
				writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "missing claims")
				return
			}
			if sameID(r.PathValue(pathVar), c.MembershipID) {
				next.ServeHTTP(w, r)
				return
			}
			if Satisfies(r.Context(), permName) {
				next.ServeHTTP(w, r)
				return
			}
			writeError(w, http.StatusForbidden, errCodeForbidden, "missing permission: "+permName)
		}))
	}
}

// DenySelfTarget refuses requests whose path membership id is the caller's
// own (no self-assigned roles/overrides/status/manager — ADR 0068 D3).
// Ids are compared as UUID values, never as strings. Super users are exempt
// (D3), nobody else. INNER middleware: compose inside an auth gate.
func DenySelfTarget(pathVar string) func(http.Handler) http.Handler {
	if strings.TrimSpace(pathVar) == "" {
		panic("authn: DenySelfTarget pathVar required")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, ok := ClaimsFromContext(r.Context())
			if !ok {
				writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "missing claims")
				return
			}
			if c.IsSuperUser {
				next.ServeHTTP(w, r)
				return
			}
			target, ok := CanonicalID(r.PathValue(pathVar))
			if !ok {
				writeError(w, http.StatusBadRequest, errCodeInvalidUserID, pathVar+" path parameter must be a UUID")
				return
			}
			self, ok := CanonicalID(c.MembershipID)
			if !ok {
				// Cannot prove the target is someone else → fail closed.
				writeError(w, http.StatusForbidden, errCodeForbidden, "caller identity unavailable")
				return
			}
			if target == self {
				writeError(w, http.StatusForbidden, errCodeSelfModificationForbidden,
					"callers cannot change their own roles, permissions, status or manager")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
