// Package authn hosts the HTTP authentication + authorization middleware
// stack for Identity-protected routes.
//
// Auth posture per `.NET security.md` "Access token" canon, ported:
//
//   - Bearer JWT in the `Authorization` header (header form: `Bearer <token>`).
//     Cookie-based sessions are the BFF concern (LeadKart Web app); the
//     bearer header is the API path covered here.
//   - Per-request verification: signature + expiry + nbf + algorithm-pin
//     (HS256, RFC 8725 §3.1) + kid resolution against the Issuer's
//     current/previous keys. JWT structural failures => 401.
//   - Authorisation: the `permission` claim is multi-valued; middleware
//     factories assert membership of a specific permission name on each
//     request. SuperUser short-circuit: `is_super_user=true` allows the
//     request unconditionally per multi-tenancy.md "SuperUser god-mode".
//   - The verified [jwt.Claims] is stashed on the request context so
//     downstream handlers read identity without re-parsing.
//
// Module placement: lives in `internal/identity/ports/authn` because
// the middleware translates HTTP-layer artefacts (Authorization header,
// 401/403 responses) into Identity-layer types (jwt.Claims, permission
// names) — Mat Ryer 2024 canon for inbound port code.
package authn

import (
	"context"
	"net/http"
	"slices"
	"strings"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/app/actclaim"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
)

// ----- Context key + accessor ----------------------------------------------

// ctxKey is the unexported tag used to stash the verified [jwt.Claims]
// onto the request context. Exported as a method so a future cross-
// package consumer (e.g. a route handler in another module) can read
// the claims without seeing the type.
type ctxKey struct{}

// WithClaims returns a copy of ctx carrying the supplied claims. Used
// by the verification middleware after a successful JWT parse; tests
// can also inject a synthetic claims set.
func WithClaims(ctx context.Context, c *jwt.Claims) context.Context {
	return context.WithValue(ctx, ctxKey{}, c)
}

// ClaimsFromContext returns the verified claims attached to ctx by
// [RequireAuth] or [RequirePermission], plus a presence flag. Returns
// (nil, false) for unauthenticated paths — the middleware short-circuits
// before downstream handlers see those, so a `false` here in handler
// code is a wiring bug.
func ClaimsFromContext(ctx context.Context) (*jwt.Claims, bool) {
	c, ok := ctx.Value(ctxKey{}).(*jwt.Claims)
	if !ok || c == nil {
		return nil, false
	}
	return c, true
}

// ----- Verifier wrapper ----------------------------------------------------

// Verifier is the interface the middleware needs from [jwt.Issuer]'s
// Verify path — declared at the consumer side so tests can substitute
// a fake without spinning a full Issuer with HMAC keys.
type Verifier interface {
	Verify(token string) (*jwt.Claims, error)
}

// ----- Bearer extraction ---------------------------------------------------

const bearerPrefix = "Bearer "

// extractBearer returns the bearer token from the Authorization header,
// or empty string if absent / malformed. Strict per RFC 6750 §2.1: the
// scheme is case-insensitive but the trailing space is required.
func extractBearer(h http.Header) string {
	auth := h.Get("Authorization")
	if len(auth) <= len(bearerPrefix) {
		return ""
	}
	// Case-insensitive scheme compare; preserves original token bytes.
	if !strings.EqualFold(auth[:len(bearerPrefix)], bearerPrefix) {
		return ""
	}
	return strings.TrimSpace(auth[len(bearerPrefix):])
}

// ----- 401 / 403 helpers ---------------------------------------------------

// errCode constants are the wire-stable identifiers consumed by the
// SPA + curl-friendly clients. Mirror the existing
// [internal/identity/ports.ErrCode*] vocabulary; not imported back
// because that package depends on Application services we don't need
// here. Keep these in lockstep with ports/errcodes.go.
const (
	errCodeUnauthenticated = "unauthenticated"
	errCodeForbidden       = "forbidden"
)

func writeError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("WWW-Authenticate", `Bearer realm="leadkart"`)
	w.WriteHeader(status)
	// {"error":"<code>","message":"<msg>"} — same shape as ports.ErrorResponse.
	body := `{"error":"` + code + `","message":"` + message + `"}`
	_, _ = w.Write([]byte(body))
}

// ----- RequireAuth ---------------------------------------------------------

// RequireAuth verifies the bearer token + stashes the claims on ctx.
// Use as the outer auth gate when an endpoint needs an authenticated
// caller but does not gate on a specific permission. Returns 401 on
// any verification failure (missing header, malformed Bearer, signature
// failure, expiry, kid-unknown).
//
// Logging: deliberately silent on the success path. The verification
// failure is logged at the caller via the platform middleware stack;
// repeating it here would double the row count in audit + tracing.
func RequireAuth(verifier Verifier) func(http.Handler) http.Handler {
	if verifier == nil {
		panic("authn: RequireAuth verifier required")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			token := extractBearer(r.Header)
			if token == "" {
				writeError(w, http.StatusUnauthorized, errCodeUnauthenticated,
					"missing or malformed Authorization bearer header")
				return
			}
			claims, err := verifier.Verify(token)
			if err != nil {
				writeError(w, http.StatusUnauthorized, errCodeUnauthenticated,
					"invalid token")
				return
			}
			// Bind BOTH the verified claims AND the tenant context onto
			// the request ctx. Downstream repositories run under
			// TxScopeTenant which expects `tenancy.FromContext(ctx)` to
			// resolve — without this bridge, every protected handler
			// would have to re-extract the tenant from claims manually
			// (boilerplate that creep into every endpoint = drift risk).
			//
			// Empty / missing tenant_id claim is a token-shape bug:
			// JWT issuance always populates it. Treat as unauthenticated
			// rather than letting the request reach a handler with an
			// unbound tenant ctx that fails opaquely at the repo layer.
			if strings.TrimSpace(claims.TenantID) == "" {
				writeError(w, http.StatusUnauthorized, errCodeUnauthenticated,
					"token missing tenant_id claim")
				return
			}
			ctx := WithClaims(r.Context(), claims)
			ctx = tenancy.WithID(ctx, tenancy.ID(claims.TenantID))
			// Stash the RFC 8693 actor claim on ctx for downstream
			// outbox-writer propagation per ADR 0056. Zero claim
			// (non-impersonation hot path) is a no-op inside
			// actclaim.WithContext — ctx chains stay minimal.
			if claims.Act != nil {
				ctx = actclaim.WithContext(ctx, actclaim.Claim{
					OperatorID: claims.Act.Sub,
					SessionID:  claims.Act.SessionID,
					Reason:     claims.Act.Reason,
				})
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ----- RequirePermission ---------------------------------------------------

// RequirePermission gates a handler on (a) a verified + freshness-
// checked JWT AND (b) the named permission appearing in the token's
// `permission` claim. Returns
//
//   - 401 if the token is missing / invalid / stale
//     (delegated to [RequireFreshStamp])
//   - 403 if the token is valid but lacks the permission
//
// SuperUser short-circuit: a token with `is_super_user=true` allows
// the request regardless of the permission claim per
// `multi-tenancy.md` "SuperUser god-mode". The flag is computed once
// at JWT issuance + audited at the action site, so the runtime check
// is just a boolean test.
//
// permName MUST be a known catalogue entry (validated via
// [permission.FromConstant] which panics on unknown — programmer error
// at wiring time, not request time). Pass
// `permission.IdentityPermissions.X.Y`, never a string literal.
//
// The freshness gate runs BEFORE the permission gate so a revoked
// session can never satisfy a permission check, even in the SuperUser
// short-circuit path.
func RequirePermission(verifier Verifier, validator StampValidator, permName string) func(http.Handler) http.Handler {
	if verifier == nil {
		panic("authn: RequirePermission verifier required")
	}
	if validator == nil {
		panic("authn: RequirePermission validator required")
	}
	// Validate the catalogue entry at wiring time — fail-fast.
	_ = permission.FromConstant(permName)

	auth := RequireFreshStamp(verifier, validator)
	return func(next http.Handler) http.Handler {
		return auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, ok := ClaimsFromContext(r.Context())
			if !ok {
				// Defensive: RequireAuth should have either populated or
				// short-circuited; this branch is a wiring bug.
				writeError(w, http.StatusUnauthorized, errCodeUnauthenticated,
					"missing claims")
				return
			}
			if c.IsSuperUser {
				next.ServeHTTP(w, r)
				return
			}
			if !slices.Contains(c.Permissions, permName) {
				writeError(w, http.StatusForbidden, errCodeForbidden,
					"missing permission: "+permName)
				return
			}
			next.ServeHTTP(w, r)
		}))
	}
}

// ----- RequireAnyPermission ------------------------------------------------

// RequireAnyPermission gates a handler on (a) a verified + freshness-
// checked JWT AND (b) at least ONE of permNames appearing in the
// token's `permission` claim. The disjunctive variant of
// [RequirePermission] — useful when a route is reachable under multiple
// permission paths (e.g. `view` OR `manage` both grant read access).
//
// Returns 401 / 403 on the same shape as [RequirePermission]. SuperUser
// short-circuit applies. Empty permNames slice panics at wiring time
// (programmer error — would let every authenticated request through).
func RequireAnyPermission(verifier Verifier, validator StampValidator, permNames ...string) func(http.Handler) http.Handler {
	if verifier == nil {
		panic("authn: RequireAnyPermission verifier required")
	}
	if validator == nil {
		panic("authn: RequireAnyPermission validator required")
	}
	if len(permNames) == 0 {
		panic("authn: RequireAnyPermission requires ≥1 permission name")
	}
	for _, p := range permNames {
		_ = permission.FromConstant(p)
	}

	auth := RequireFreshStamp(verifier, validator)
	return func(next http.Handler) http.Handler {
		return auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, ok := ClaimsFromContext(r.Context())
			if !ok {
				writeError(w, http.StatusUnauthorized, errCodeUnauthenticated,
					"missing claims")
				return
			}
			if c.IsSuperUser {
				next.ServeHTTP(w, r)
				return
			}
			for _, p := range permNames {
				if slices.Contains(c.Permissions, p) {
					next.ServeHTTP(w, r)
					return
				}
			}
			writeError(w, http.StatusForbidden, errCodeForbidden,
				"missing any of: "+strings.Join(permNames, ", "))
		}))
	}
}

// ----- RequireTenantContext -------------------------------------------------

// RequireTenantContext gates a handler on (a) a verified + freshness-
// checked JWT AND (b) the JWT's tenant_id claim matching the path
// parameter named pathVar — OR the caller being a Platform / SuperUser
// operator.
//
// Used by tenant-resource endpoints under
// `/api/v1/tenants/{tenantId}/...`: a tenant Admin can only modify
// their own tenant; a Platform operator can modify any (post-
// impersonation per `multi-tenancy.md`).
//
// pathVar is the name of the path-parameter to match against. Almost
// always "tenantId"; surfaced as a parameter so the same factory
// works for any future per-tenant URL pattern.
//
//   - 401 if the token is missing / invalid / stale.
//   - 403 if neither tenant matches NOR the operator flags are set.
func RequireTenantContext(verifier Verifier, validator StampValidator, pathVar string) func(http.Handler) http.Handler {
	if verifier == nil {
		panic("authn: RequireTenantContext verifier required")
	}
	if validator == nil {
		panic("authn: RequireTenantContext validator required")
	}
	if strings.TrimSpace(pathVar) == "" {
		panic("authn: RequireTenantContext pathVar required")
	}
	auth := RequireFreshStamp(verifier, validator)
	return func(next http.Handler) http.Handler {
		return auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, ok := ClaimsFromContext(r.Context())
			if !ok {
				writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "missing claims")
				return
			}
			// SuperUser / Platform-tier operators bypass — they're
			// expected to operate cross-tenant under the impersonation
			// audit trail. Platform bypass requires BOTH the
			// is_platform flag AND the tenant_slug anchor (defense-
			// in-depth — see [RequirePlatform] godoc).
			if c.IsSuperUser || (c.IsPlatform && c.TenantSlug == PlatformTenantSlug) {
				next.ServeHTTP(w, r)
				return
			}
			urlTenant := strings.TrimSpace(r.PathValue(pathVar))
			jwtTenant := strings.TrimSpace(c.TenantID)
			if urlTenant == "" {
				writeError(w, http.StatusBadRequest, errCodeForbidden,
					"path missing "+pathVar+" parameter")
				return
			}
			if urlTenant != jwtTenant {
				writeError(w, http.StatusForbidden, errCodeForbidden,
					"tenant context does not match url")
				return
			}
			next.ServeHTTP(w, r)
		}))
	}
}

// ----- RequirePlatform -----------------------------------------------------

// PlatformTenantSlug is the canonical slug for the platform tenant.
// Mirrors cmd/bootstrap's platformTenantSlug + the .NET parent's
// SuperUser god-mode convention. Used as a defense-in-depth anchor
// for platform-tier middleware (see [RequirePlatform]).
const PlatformTenantSlug = "platform"

// RequirePlatform gates a handler on (a) a verified + freshness-
// checked JWT AND (b) the `is_platform=true` claim AND (c) the
// `tenant_slug == "platform"` claim — i.e. the caller's Membership is
// in the Platform tenant. Drives access to platform-tier endpoints
// under `/api/v1/platform/...` per `multi-tenancy.md` "Platform admin
// endpoints" (rate-limited 600/min per operator).
//
// The slug anchor is defense-in-depth (per migration 20260507000008
// audit-pass discussion): even if a buggy code path mints a JWT with
// is_platform=true for a non-platform tenant, the slug check catches
// it before the handler runs. Login MUST set tenant_slug honestly —
// it does, since the slug is read from identity.tenants where slug is
// UNIQUE.
//
// Returns 401 if the token is missing/invalid/stale; 403 if any of
// the three checks fail (collapsed into one error to avoid leaking
// which gate failed). SuperUser implies platform membership in
// v0.3+ (SuperAdmin role only seeded on the platform tenant); for
// v0.2 the two flags are checked independently.
func RequirePlatform(verifier Verifier, validator StampValidator) func(http.Handler) http.Handler {
	if verifier == nil {
		panic("authn: RequirePlatform verifier required")
	}
	if validator == nil {
		panic("authn: RequirePlatform validator required")
	}
	auth := RequireFreshStamp(verifier, validator)
	return func(next http.Handler) http.Handler {
		return auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, ok := ClaimsFromContext(r.Context())
			if !ok {
				writeError(w, http.StatusUnauthorized, errCodeUnauthenticated,
					"missing claims")
				return
			}
			// Defense-in-depth: BOTH the is_platform flag AND the
			// slug anchor must agree. Either-or would let an attacker
			// who can forge a JWT (or a buggy issuer path) bypass with
			// just one of the two.
			if !c.IsPlatform || c.TenantSlug != PlatformTenantSlug {
				writeError(w, http.StatusForbidden, errCodeForbidden,
					"platform-tier access required")
				return
			}
			next.ServeHTTP(w, r)
		}))
	}
}
