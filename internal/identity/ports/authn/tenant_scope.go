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

// ScopeActReasonPath is the act-claim reason for an anchored operator acting in
// a foreign tenant through the URL alone (a /tenants/{tenantId}/... route with
// no X-Tenant-Id): the same durable attribution, a different provenance.
const ScopeActReasonPath = "tenant-via-path"

const (
	errCodeInvalidTenantScope      = "invalid_tenant_scope"
	errCodeTenantScopeNotFound     = "tenant_scope_not_found"
	errCodeTenantScopeConflict     = "tenant_scope_conflict"
	errCodeTenantScopeNotSupported = "tenant_scope_not_supported"
	errCodeTenantScopeForbidden    = "tenant_scope_forbidden"
	errCodeTenantScopeReadOnly     = "tenant_scope_read_only"
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
	// tableIgnored: the central table says the header does not apply to this
	// route. It binds only a route that carries no registration marker.
	tableIgnored bool
	// done/effective memoise the decision: nested auth gates call applyScope
	// again (each re-binds the caller's own tenant), and the second call must
	// cost no lookup and no log line.
	done      bool
	effective string
}

type scopeRequestKey struct{}

// TenantScope implements ADR 0039 clauses 1-2 (ADR 0068 D7). It is the ONE
// place the header is read, and it only stashes the raw value together with
// what the central table says about the route (see scope_policy.go); a route's
// own registration marker, which TenantScope cannot see from outside the mux,
// overrides the table when the auth gate applies the scope. The auth
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
			if !anyNonBlank(values) {
				next.ServeHTTP(w, r)
				return
			}
			ctx := context.WithValue(r.Context(), scopeRequestKey{}, &scopeRequest{
				values: values, lookup: lookup, log: log, tableIgnored: scopeIgnoredOn(r),
			})
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
	kind     scopeRouteKind
	pathVar  string
	writable bool
}

type scopeRouteKey struct{}

// markRoute is the ONE way a registration marker declares its policy: it
// merges into whatever outer markers already set, and its presence (any
// marker at all) is what makes the route's own policy win over the central
// table's IGNORE rows.
func markRoute(set func(*scopeRoute), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sr, _ := r.Context().Value(scopeRouteKey{}).(scopeRoute)
		set(&sr)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), scopeRouteKey{}, sr)))
	})
}

// HonourTenantScope declares that a route reads the header on purpose even
// though the central table would ignore its URL group (for example the lead-
// credit balance under /platform/): reads act in the scoped tenant, writes are
// read-only unless the route is also [TenantScopeWritable].
func HonourTenantScope(next http.Handler) http.Handler {
	return markRoute(func(*scopeRoute) {}, next)
}

// TenantScopeWritable opts a route into WRITES under a foreign X-Tenant-Id.
// Every other honoured, non-read route is 403 tenant_scope_read_only when the
// header selects a foreign tenant, so a module's mutation cannot stamp the
// operator's own platform-tenant membership id into another tenant's rows.
// Reserve it for the identity routes the operator scope tree writes (users,
// roles, role permissions, overrides, manager). Wrap OUTSIDE the auth gate.
func TenantScopeWritable(next http.Handler) http.Handler {
	return markRoute(func(r *scopeRoute) { r.writable = true }, next)
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
	return markRoute(func(r *scopeRoute) { r.kind = scopeReject }, next)
}

// TenantScopeMatchesPath marks a route whose path carries the target tenant
// in pathVar: the path wins, and a header naming a different tenant is 400
// tenant_scope_conflict. ([RequireTenantPermission] applies this itself.)
// Path-tenant routes are writable by definition.
func TenantScopeMatchesPath(pathVar string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return markRoute(func(r *scopeRoute) { r.kind, r.pathVar = scopePathMatch, pathVar }, next)
	}
}

// TenantScopeLifecycle is [TenantScopeMatchesPath] for the tenant lifecycle
// routes (suspend / activate / mark-for-deletion / restore).
func TenantScopeLifecycle(pathVar string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return markRoute(func(r *scopeRoute) { r.kind, r.pathVar = scopePathLifecycle, pathVar }, next)
	}
}

// TenantScopePathOnly marks a route that acts on the path tenant and never on
// the header (hard delete): a header naming a different tenant is still 400
// tenant_scope_conflict, but no scope is applied, looked up or audited.
func TenantScopePathOnly(pathVar string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return markRoute(func(r *scopeRoute) { r.kind, r.pathVar = scopePathOnly, pathVar }, next)
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
	// Idempotent: a nested gate re-binds the caller's own tenant, so re-apply
	// the memoised decision without another lookup or log line.
	if sr.done {
		return withEffectiveScope(r, c, sr.effective), true
	}
	route, marked := r.Context().Value(scopeRouteKey{}).(scopeRoute)
	if sr.tableIgnored && !marked {
		sr.done = true
		return r, true
	}
	if !IsPlatformOperator(c) {
		sr.done = true
		sr.log.DebugContext(r.Context(), "X-Tenant-Id ignored for non-platform caller", "caller_tenant_id", c.TenantID)
		return r, true
	}

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

	if ref := preLookupRefusal(r, c, route, id, own); ref != nil {
		return reject(ref.status, ref.code, ref.msg, id)
	}
	if own || route.kind == scopePathOnly {
		// Acting in the home tenant, or a path-only route that never applies
		// the header: nothing to select.
		sr.done = true
		return r, true
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
	case state == TenantScopeInactive && !isReadMethod(r.Method) && route.kind == scopeHonour:
		return reject(http.StatusConflict, errCodeTenantNotActive, "the scoped tenant is suspended or pending deletion", id)
	}

	sr.log.InfoContext(r.Context(), "operator tenant scope",
		"scope_mode", "tenant-via-header", "effective_tenant_id", id,
		"caller_tenant_id", c.TenantID, "operator_person_id", c.Subject,
		"method", r.Method, "path", r.URL.Path)
	sr.done, sr.effective = true, id
	return withEffectiveScope(r, c, id), true
}

type scopeRefusal struct {
	status    int
	code, msg string
}

// preLookupRefusal holds every refusal that is decided from the route policy
// and the caller alone, BEFORE any tenant lookup, so the answer is identical
// for existing, unknown and inactive tenants (no enumeration oracle).
func preLookupRefusal(r *http.Request, c *jwt.Claims, route scopeRoute, id string, own bool) *scopeRefusal {
	switch route.kind {
	case scopeReject:
		if !own {
			return &scopeRefusal{http.StatusForbidden, errCodeTenantScopeNotSupported, "X-Tenant-Id is not supported on this route"}
		}
	case scopePathMatch, scopePathLifecycle, scopePathOnly:
		if pathTenant, ok := CanonicalID(r.PathValue(route.pathVar)); ok && pathTenant != id {
			return &scopeRefusal{http.StatusBadRequest, errCodeTenantScopeConflict, "X-Tenant-Id conflicts with the tenant in the URL"}
		}
	case scopeHonour:
	}
	if own || route.kind == scopePathOnly {
		return nil
	}
	// Selecting a foreign tenant is itself an authority: platform-tenant
	// membership proves nothing.
	if !MaySelectTenantScope(c) {
		return &scopeRefusal{http.StatusForbidden, errCodeTenantScopeForbidden, "X-Tenant-Id requires platform tenant scope authority"}
	}
	// Writes under a foreign scope are opt-in: the operator's own membership
	// id would otherwise be stamped into the scoped tenant's rows.
	if route.kind == scopeHonour && !route.writable && !isReadMethod(r.Method) {
		return &scopeRefusal{http.StatusForbidden, errCodeTenantScopeReadOnly, "this route is read-only under X-Tenant-Id"}
	}
	return nil
}

// withEffectiveScope binds the operator's accepted target tenant and the
// durable act claim onto r. effective "" means no scope was applied.
func withEffectiveScope(r *http.Request, c *jwt.Claims, effective string) *http.Request {
	if effective == "" {
		return r
	}
	ctx := tenancy.WithID(r.Context(), tenancy.ID(effective))
	ctx = actclaim.WithContext(ctx, actclaim.Claim{OperatorID: c.Subject, Reason: ScopeActReason})
	return r.WithContext(ctx)
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

// ActorMembershipID returns the caller's membership id for audit columns that
// record WHICH MEMBER of the effective tenant acted, or "" when the request is
// operator-scoped into a foreign tenant: the operator's membership lives in the
// platform tenant, so it is not a member of the scoped tenant and recording it
// there would name someone who does not belong to that tenant's people. (The
// identity tables enforce this with a (membership_id, tenant_id) composite FK;
// module tables carry no such FK, so for them the reason is semantic, not a
// constraint.) Durable provenance for scoped writes is the act claim
// (act_operator_id on the outbox / audit rows, see [ScopeActReason]) plus the
// "operator tenant scope" INFO log line (ADR 0039, ADR 0056).
//
// This is the ONE actor-membership policy. Modules whose domain requires a
// non-empty actor on every mutation (crm, orders, tasks, dispatch, inventory)
// do not adopt it: their mutations are read-only under a foreign X-Tenant-Id
// instead (see [TenantScopeWritable]), so the operator's id is never stamped.
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
