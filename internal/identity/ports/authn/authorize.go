package authn

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/leadkart/leadkart-go/internal/common/actclaim"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
)

const (
	errCodeSelfModificationForbidden = "self_modification_forbidden"
	// Bad-id codes mirror ports.ErrCodeInvalid{User,Role,Tenant}ID (the
	// handlers' own codes for the same malformed path value).
	errCodeInvalidUserID   = "invalid_user_id"
	errCodeInvalidTenantID = "invalid_tenant_id"
)

// IsPlatformOperator reports is_platform AND slug-anchored AND not an impersonation
// token (ADR 0039 slug anchor; ADR 0045 downgrade).
func IsPlatformOperator(c *jwt.Claims) bool {
	return c != nil && c.IsPlatform && c.TenantSlug == PlatformTenantSlug && !c.IsImpersonation()
}

// satisfiesIn is THE authorization decision (ADR 0036 + ADR 0068). Rules,
// in order:
//  1. no claims, or a name outside the catalogue (incl. "") -> deny
//  2. super user -> allow
//  3. explicit grant of the name:
//     - platform-reserved name other than tenant.admin -> allow only for a
//     slug-anchored, non-impersonation platform operator (defence in depth:
//     a tenant principal never exercises platform authority, whatever its
//     claims say);
//     - any other name (incl. tenant.admin itself) -> allow only when the
//     effective tenant is the caller's own (D2: cross-tenant reach comes
//     only from platform.* equivalents, never from a tenant-plane grant)
//  4. tenant.admin implies every non-reserved name in the caller's OWN tenant
//  5. a platform operator's platform.* grant satisfies the identity.*
//     equivalent in any tenant
func satisfiesIn(c *jwt.Claims, effective tenancy.ID, required string) bool {
	if c == nil || !permission.IsKnown(required) {
		return false
	}
	if c.IsSuperUser {
		return true
	}
	own := sameID(c.TenantID, effective.String())
	// Owner ruling 2026-11-21: a permission in authority.AdminTierOnlyPermissions
	// (identity.users.create) is satisfied through a TENANT-plane grant, explicit or
	// implied by tenant.admin, only at the Administrator tier or above (an
	// impersonation session is Admin-equivalent). A stale grant below the tier is
	// inert here, which is also why it is absent from effective_permissions. The
	// platform rule (super user above, the operator's platform equivalents below)
	// is not a tenant tier and is unaffected.
	tierOK := authority.MayHoldAt(authority.RankFromClaim(c.Rank), required) || c.IsImpersonation()
	if slices.Contains(c.Permissions, required) {
		if permission.IsPlatformReserved(required) && required != permission.IdentityPermissions.Meta.TenantAdmin {
			return IsPlatformOperator(c)
		}
		if own && tierOK {
			return true
		}
	}
	if own && tierOK &&
		slices.Contains(c.Permissions, permission.IdentityPermissions.Meta.TenantAdmin) &&
		permission.ImpliedByTenantAdmin(required) {
		return true
	}
	if IsPlatformOperator(c) {
		for _, eq := range permission.PlatformEquivalents(required) {
			if slices.Contains(c.Permissions, eq) {
				return true
			}
		}
	}
	return false
}

// Satisfies reports whether the authenticated caller on ctx may exercise
// permName in the request's effective tenant: the tenancy ctx bound by
// RequireAuth (the caller's own tenant) or re-bound to the PATH tenant by
// RequireTenantPermission, so an inline check under a
// path-scoped gate agrees with the gate. With no tenant on ctx it falls back
// to the caller's own tenant. Use for handler-inline checks (e.g. read_all
// narrowing) in every module. False without claims or for unknown names.
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
// TENANT. On success the request context's tenant is re-bound to the
// canonical path tenant so everything downstream (inline Satisfies,
// repositories) sees the one tenant the gate decided on (tdl_canon §4).
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
		// The path tenant wins over X-Tenant-Id; a conflicting header is 400.
		return TenantScopeMatchesPath(pathVar)(auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			if !c.IsSuperUser && !IsPlatformOperator(c) && !sameID(urlTenant, c.TenantID) {
				writeError(w, http.StatusForbidden, errCodeForbidden, "tenant context does not match url")
				return
			}
			for _, p := range permNames {
				if satisfiesIn(c, tenancy.ID(urlTenant), p) {
					next.ServeHTTP(w, bindTenant(r, c, urlTenant))
					return
				}
			}
			writeError(w, http.StatusForbidden, errCodeForbidden, "missing any of: "+strings.Join(permNames, ", "))
		})))
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

// SatisfiesInTenant evaluates permName in an explicit tenant. Slug lookups
// resolve the tenant from the DB, so the ctx or path tenant is not the one to
// decide on. False without claims, for an unparseable tenant id and for names
// outside the catalogue.
func SatisfiesInTenant(ctx context.Context, tenantID tenancy.ID, permName string) bool {
	c, ok := ClaimsFromContext(ctx)
	if !ok {
		return false
	}
	canonical, ok := CanonicalID(tenantID.String())
	return ok && satisfiesIn(c, tenancy.ID(canonical), permName)
}

// DenySelfPerson refuses /platform/persons/{pathVar}/... mutations on the
// caller's OWN person (an operator must not globally suspend or edit
// themselves). Ids are compared as UUID values; an unparseable path value
// passes through to the handler, which answers 400. Super users are exempt.
// INNER middleware: compose inside the platform gate.
func DenySelfPerson(pathVar string) func(http.Handler) http.Handler {
	if strings.TrimSpace(pathVar) == "" {
		panic("authn: DenySelfPerson pathVar required")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, ok := ClaimsFromContext(r.Context())
			if !ok {
				writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "missing claims")
				return
			}
			if !c.IsSuperUser && sameID(r.PathValue(pathVar), c.Subject) {
				writeError(w, http.StatusForbidden, errCodeSelfModificationForbidden,
					"operators cannot change their own person")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// bindTenant returns r with the tenancy context re-bound to the (canonical)
// path tenant a gate has just authorised the caller for. An anchored operator
// acting in a foreign tenant leaves the same durable attribution (act claim)
// whether the tenant came from X-Tenant-Id or from the URL alone; a claim the
// header scope already set is kept.
func bindTenant(r *http.Request, c *jwt.Claims, canonicalTenant string) *http.Request {
	ctx := tenancy.WithID(r.Context(), tenancy.ID(canonicalTenant))
	if IsPlatformOperator(c) && !sameID(c.TenantID, canonicalTenant) {
		if _, has := actclaim.FromContext(ctx); !has {
			ctx = actclaim.WithContext(ctx, actclaim.Claim{OperatorID: c.Subject, Reason: ScopeActReasonPath})
		}
	}
	return r.WithContext(ctx)
}
