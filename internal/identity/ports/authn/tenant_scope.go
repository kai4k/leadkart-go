package authn

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/app/jwt"
)

// TenantScopeHeader is the ADR 0039 operator scope override header.
const TenantScopeHeader = "X-Tenant-Id"

const (
	errCodeInvalidTenantScope      = "invalid_tenant_scope"
	errCodeTenantScopeNotFound     = "tenant_scope_not_found"
	errCodeTenantScopeConflict     = "tenant_scope_conflict"
	errCodeTenantScopeNotSupported = "tenant_scope_not_supported"
	errCodeInternal                = "internal_error"
)

// TenantExistsFunc validates a header-supplied target tenant: it reports
// whether tenantID names an existing, non-deleted tenant. A func type, not an
// interface: ports/ never declares interfaces (TestArch_PortsAdaptersDontDefineInterfaces);
// identity/ports provides the app-backed implementation
// (ports.TenantScopeResolver.TenantExists).
type TenantExistsFunc func(ctx context.Context, tenantID string) (bool, error)

type scopeOverrideKey struct{}

type scopeOverride struct {
	personID string
	tenantID tenancy.ID
}

// TenantScope implements ADR 0039 clauses 1-2 (ADR 0068 D7): X-Tenant-Id is
// honoured only for slug-anchored, non-impersonation platform operators;
// ignored for everyone else. A valid override is stashed on ctx and bound
// as the request tenant by RequireAuth. Wrap the whole mux; anonymous and
// invalid-token requests pass through untouched (per-route gates answer).
//
// This is the ONE place the header is read. The override is bound to the
// verified person id, so a stashed value can never be consumed for a
// different principal, and RequireAuth re-checks IsPlatformOperator.
func TenantScope(verifier Verifier, tenantExists TenantExistsFunc, log *slog.Logger) func(http.Handler) http.Handler {
	if verifier == nil || tenantExists == nil || log == nil {
		panic("authn: TenantScope verifier, tenantExists and log required")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := strings.TrimSpace(r.Header.Get(TenantScopeHeader))
			token := extractBearer(r.Header)
			if raw == "" || token == "" {
				next.ServeHTTP(w, r)
				return
			}
			c, err := verifier.Verify(token)
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}
			if !IsPlatformOperator(c) {
				log.DebugContext(r.Context(), "X-Tenant-Id ignored for non-platform caller", "caller_tenant_id", c.TenantID)
				next.ServeHTTP(w, r)
				return
			}
			id, ok := CanonicalID(raw)
			if !ok {
				writeError(w, http.StatusBadRequest, errCodeInvalidTenantScope, "X-Tenant-Id must be a tenant UUID")
				return
			}
			exists, err := tenantExists(r.Context(), id)
			if err != nil {
				log.ErrorContext(r.Context(), "tenant scope lookup failed", "err", err)
				writeError(w, http.StatusInternalServerError, errCodeInternal, "internal error")
				return
			}
			if !exists {
				writeError(w, http.StatusNotFound, errCodeTenantScopeNotFound, "X-Tenant-Id does not name an active tenant")
				return
			}
			log.InfoContext(r.Context(), "operator tenant scope",
				"scope_mode", "tenant-via-header", "effective_tenant_id", id,
				"caller_tenant_id", c.TenantID, "operator_person_id", c.Subject,
				"method", r.Method, "path", r.URL.Path)
			ctx := context.WithValue(r.Context(), scopeOverrideKey{}, scopeOverride{personID: c.Subject, tenantID: tenancy.ID(id)})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// scopeOverrideFor returns the validated header tenant for this principal.
func scopeOverrideFor(ctx context.Context, c *jwt.Claims) (tenancy.ID, bool) {
	o, ok := ctx.Value(scopeOverrideKey{}).(scopeOverride)
	if !ok || c == nil || o.personID != c.Subject || !IsPlatformOperator(c) {
		return "", false
	}
	return o.tenantID, true
}

// scopeConflicts reports whether a header scope disagrees with a path tenant.
func scopeConflicts(ctx context.Context, c *jwt.Claims, pathTenant string) bool {
	o, ok := scopeOverrideFor(ctx, c)
	return ok && string(o) != pathTenant
}

// EffectiveTenantID returns the tenant the request acts in: the one bound on
// ctx by RequireAuth (the caller's own tenant, or the validated operator
// header scope) or re-bound by a path gate. Handlers that need the tenant id
// MUST use this (or tenancy.FromContext), never claims.TenantID, so header-
// scoped operators act in the tenant they selected. Falls back to the
// caller's own tenant when ctx carries no tenant.
func EffectiveTenantID(ctx context.Context) (string, bool) {
	if id, ok := tenancy.FromContext(ctx); ok && !id.IsZero() {
		return id.String(), true
	}
	if c, ok := ClaimsFromContext(ctx); ok && strings.TrimSpace(c.TenantID) != "" {
		return c.TenantID, true
	}
	return "", false
}

// ActorMembershipID returns the caller's membership id for audit columns
// that carry a (membership_id, tenant_id) composite FK, or "" when the
// request is operator-scoped into a foreign tenant: the operator's
// membership lives in the platform tenant, so stamping it into the scoped
// tenant's rows would violate the FK. Provenance for scoped writes is the
// "operator tenant scope" audit log line (ADR 0039).
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

type rejectScopeKey struct{}

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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), rejectScopeKey{}, true)))
	})
}

func scopeRejected(ctx context.Context) bool {
	v, _ := ctx.Value(rejectScopeKey{}).(bool)
	return v
}
