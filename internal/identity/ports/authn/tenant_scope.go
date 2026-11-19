package authn

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/leadkart/leadkart-go/internal/common/actclaim"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
)

// TenantScopeHeader is the ADR 0039 operator scope override header.
const TenantScopeHeader = "X-Tenant-Id"

// ScopeActReason is the act-claim reason stamped on every request an operator
// runs in a foreign tenant through the header. With the operator person id as
// act_operator_id it makes the outbox and audit rows of a scoped write durably
// attributable (ADR 0056), where the INFO log line alone is not.
const ScopeActReason = "tenant-via-header"

const (
	errCodeInvalidTenantScope      = "invalid_tenant_scope"
	errCodeTenantScopeNotFound     = "tenant_scope_not_found"
	errCodeTenantScopeConflict     = "tenant_scope_conflict"
	errCodeTenantScopeNotSupported = "tenant_scope_not_supported"
	errCodeTenantScopeForbidden    = "tenant_scope_forbidden"
	errCodeTenantNotActive         = "tenant_not_active"
	errCodeInternal                = "internal_error"
)

// TenantScopeState is what a header-supplied target tenant looks like to the
// scope gate.
type TenantScopeState int

const (
	// TenantScopeUnknown means no such tenant, or a deleted one. Never scoped into.
	TenantScopeUnknown TenantScopeState = iota
	// TenantScopeActive means scope reads and writes are allowed.
	TenantScopeActive
	// TenantScopeInactive means suspended or pending deletion. Scope reads and the
	// tenant lifecycle routes are allowed; other writes are 409.
	TenantScopeInactive
)

// TenantLookupFunc resolves a header-supplied target tenant. A func type, not
// an interface: ports/ never declares interfaces
// (TestArch_PortsAdaptersDontDefineInterfaces); identity/ports provides the
// app-backed implementation (ports.TenantScopeResolver.Lookup).
type TenantLookupFunc func(ctx context.Context, tenantID string) (TenantScopeState, error)

// scopeRequest is what TenantScope stashes on ctx: the raw header values and
// the collaborators the auth gate needs to validate them once the caller's
// token and stamp are known to be good. Nothing is validated, looked up or
// logged until then (ADR 0068 D7).
type scopeRequest struct {
	values []string
	lookup TenantLookupFunc
	log    *slog.Logger
}

type scopeRequestKey struct{}

// TenantScope implements ADR 0039 clauses 1-2 (ADR 0068 D7). It is the ONE
// place the header is read, and it only stashes the raw value (skipping the
// routes the central policy says ignore it; see scope_policy.go). The auth
// gate ([RequireFreshStamp]) applies it after the token and security stamp
// pass: X-Tenant-Id is honoured only for slug-anchored, non-impersonation
// platform operators and ignored for everyone else. Wrap the whole mux.
func TenantScope(lookup TenantLookupFunc, log *slog.Logger) func(http.Handler) http.Handler {
	if lookup == nil || log == nil {
		panic("authn: TenantScope lookup and log required")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			values := r.Header.Values(TenantScopeHeader)
			if !anyNonBlank(values) || scopeIgnoredOn(r) {
				next.ServeHTTP(w, r)
				return
			}
			ctx := context.WithValue(r.Context(), scopeRequestKey{}, &scopeRequest{values: values, lookup: lookup, log: log})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func anyNonBlank(values []string) bool {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return true
		}
	}
	return false
}

// ----- Per-route scope handling --------------------------------------------

type scopeRouteKind int

const (
	scopeHonour        scopeRouteKind = iota // default: the header selects the tenant
	scopeReject                              // a scope away from the home tenant is 403
	scopePathMatch                           // header must equal the path tenant (path wins)
	scopePathLifecycle                       // as scopePathMatch; tenant lifecycle routes also act on inactive tenants
	scopePathOnly                            // header is only conflict-checked, never applied
)

type scopeRoute struct {
	kind    scopeRouteKind
	pathVar string
}

type scopeRouteKey struct{}

func withScopeRoute(sr scopeRoute, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), scopeRouteKey{}, sr)))
	})
}

// RejectTenantScope marks a route as not supporting the X-Tenant-Id operator
// scope: when the header would move the request out of the caller's own
// tenant, the auth gate answers 403 tenant_scope_not_supported instead of
// running the handler. Wrap OUTSIDE the auth middleware:
//
//	mux.Handle(pattern, authn.RejectTenantScope(auth(handler)))
//
// For routes whose effect is bound to the caller's own tenant (wallet debit,
// self-service approval workflows) where acting "as" another tenant would be
// silently wrong. Headers from non-operators are ignored as everywhere else.
func RejectTenantScope(next http.Handler) http.Handler {
	return withScopeRoute(scopeRoute{kind: scopeReject}, next)
}

// TenantScopeMatchesPath marks a route whose path carries the target tenant
// in pathVar: the path wins, and a header naming a different tenant is 400
// tenant_scope_conflict. ([RequireTenantPermission] applies this itself.)
func TenantScopeMatchesPath(pathVar string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return withScopeRoute(scopeRoute{kind: scopePathMatch, pathVar: pathVar}, next)
	}
}

// TenantScopeLifecycle is [TenantScopeMatchesPath] for the tenant lifecycle
// routes (suspend / activate / mark-for-deletion / restore): they are exactly
// the writes that must remain possible against a suspended or
// pending-deletion tenant.
func TenantScopeLifecycle(pathVar string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return withScopeRoute(scopeRoute{kind: scopePathLifecycle, pathVar: pathVar}, next)
	}
}

// TenantScopePathOnly marks a route that acts on the path tenant and never on
// the header (hard delete): a header naming a different tenant is still 400
// tenant_scope_conflict, but no scope is applied, looked up or audited.
func TenantScopePathOnly(pathVar string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return withScopeRoute(scopeRoute{kind: scopePathOnly, pathVar: pathVar}, next)
	}
}

// applyScope runs inside the auth gate once the caller's token and security
// stamp are valid. It returns the request to hand downstream: unchanged when
// there is no (applicable) header, re-bound to the operator's target tenant
// when the scope is accepted. ok=false means a response was written.
func applyScope(w http.ResponseWriter, r *http.Request, c *jwt.Claims) (*http.Request, bool) {
	sr, has := r.Context().Value(scopeRequestKey{}).(*scopeRequest)
	if !has {
		return r, true
	}
	if !IsPlatformOperator(c) {
		sr.log.DebugContext(r.Context(), "X-Tenant-Id ignored for non-platform caller", "caller_tenant_id", c.TenantID)
		return r, true
	}
	route, _ := r.Context().Value(scopeRouteKey{}).(scopeRoute)

	reject := func(status int, code, msg, requested string) (*http.Request, bool) {
		sr.log.WarnContext(r.Context(), "operator tenant scope rejected",
			"scope_mode", "tenant-via-header", "reason", code, "requested_tenant_id", requested,
			"caller_tenant_id", c.TenantID, "operator_person_id", c.Subject,
			"method", r.Method, "path", r.URL.Path)
		writeError(w, status, code, msg)
		return r, false
	}

	if len(sr.values) > 1 {
		return reject(http.StatusBadRequest, errCodeInvalidTenantScope, "X-Tenant-Id must be supplied at most once", "")
	}
	raw := strings.TrimSpace(sr.values[0])
	id, ok := CanonicalID(raw)
	if !ok {
		return reject(http.StatusBadRequest, errCodeInvalidTenantScope, "X-Tenant-Id must be a tenant UUID", raw)
	}
	own := sameID(id, c.TenantID)

	switch route.kind {
	case scopeReject:
		if !own {
			return reject(http.StatusForbidden, errCodeTenantScopeNotSupported, "X-Tenant-Id is not supported on this route", id)
		}
	case scopePathMatch, scopePathLifecycle, scopePathOnly:
		if pathTenant, ok := CanonicalID(r.PathValue(route.pathVar)); ok && pathTenant != id {
			return reject(http.StatusBadRequest, errCodeTenantScopeConflict, "X-Tenant-Id conflicts with the tenant in the URL", id)
		}
		if route.kind == scopePathOnly {
			return r, true
		}
	case scopeHonour:
	}
	// Selecting a foreign tenant is itself an authority: platform-tenant
	// membership proves nothing. Decided before any tenant lookup and with one
	// uniform answer, so it cannot be used to probe which tenants exist.
	if !own && !MaySelectTenantScope(c) {
		return reject(http.StatusForbidden, errCodeTenantScopeForbidden, "X-Tenant-Id requires platform tenant scope authority", id)
	}
	if own {
		return r, true // already acting in the home tenant: nothing to select
	}

	state, err := sr.lookup(r.Context(), id)
	if err != nil {
		sr.log.ErrorContext(r.Context(), "tenant scope lookup failed", "err", err)
		writeError(w, http.StatusInternalServerError, errCodeInternal, "internal error")
		return r, false
	}
	switch {
	case state == TenantScopeUnknown:
		return reject(http.StatusNotFound, errCodeTenantScopeNotFound, "X-Tenant-Id does not name an active tenant", id)
	case state == TenantScopeInactive && !isReadMethod(r.Method) && route.kind != scopePathLifecycle:
		return reject(http.StatusConflict, errCodeTenantNotActive, "the scoped tenant is suspended or pending deletion", id)
	}

	sr.log.InfoContext(r.Context(), "operator tenant scope",
		"scope_mode", "tenant-via-header", "effective_tenant_id", id,
		"caller_tenant_id", c.TenantID, "operator_person_id", c.Subject,
		"method", r.Method, "path", r.URL.Path)
	ctx := tenancy.WithID(r.Context(), tenancy.ID(id))
	ctx = actclaim.WithContext(ctx, actclaim.Claim{OperatorID: c.Subject, Reason: ScopeActReason})
	return r.WithContext(ctx), true
}

// MaySelectTenantScope reports whether the caller may act in a tenant other
// than its own through X-Tenant-Id: a super user, or a slug-anchored platform
// operator holding platform.tenants.view (platform-reserved, so a tenant
// principal never satisfies it whatever its claims say).
func MaySelectTenantScope(c *jwt.Claims) bool {
	return c != nil && satisfiesIn(c, tenancy.ID(c.TenantID), permission.IdentityPermissions.Platform.TenantsView)
}

func isReadMethod(m string) bool { return m == http.MethodGet || m == http.MethodHead }

// EffectiveTenantID returns the tenant the request acts in: the one bound on
// ctx by the auth gate (the caller's own tenant, or the validated operator
// header scope) or re-bound by a path gate. Handlers that need the tenant id
// MUST use this (or tenancy.FromContext), never claims.TenantID, so header-
// scoped operators act in the tenant they selected. It fails closed: with no
// tenant bound (a wiring bug: every authenticated route binds one) it returns
// ("", false) and callers answer 401/500 rather than guessing from claims.
func EffectiveTenantID(ctx context.Context) (string, bool) {
	if id, ok := tenancy.FromContext(ctx); ok && !id.IsZero() {
		return id.String(), true
	}
	return "", false
}

// ActorMembershipID returns the caller's membership id for audit columns
// that carry a (membership_id, tenant_id) composite FK, or "" when the
// request is operator-scoped into a foreign tenant: the operator's
// membership lives in the platform tenant, so stamping it into the scoped
// tenant's rows would violate the FK. Durable provenance for scoped writes is the
// act claim (act_operator_id on the outbox / audit rows, see [ScopeActReason])
// plus the "operator tenant scope" INFO log line (ADR 0039, ADR 0056).
//
// This is the ONE actor-membership policy: modules whose domain requires a
// non-empty actor on every mutation (crm, orders, tasks, dispatch, inventory)
// cannot adopt it without a product decision on what to record; see the
// scope-fix report.
func ActorMembershipID(ctx context.Context) string {
	c, ok := ClaimsFromContext(ctx)
	if !ok {
		return ""
	}
	if eff, ok := tenancy.FromContext(ctx); ok && !eff.IsZero() && !sameID(c.TenantID, eff.String()) {
		return ""
	}
	return c.MembershipID
}
