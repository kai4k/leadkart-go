package authn

import (
	"net/http"
	"slices"
	"strings"
)

// errCodePasswordChangeRequired is the 403 a token carrying must_change_password
// gets on every route outside [pendingPasswordChangeAllowlist] (plan D8, ADR 0053).
// 403, not 401: the token is valid and fresh, the caller just must change the
// password first; a 401 would send the BFF into a refresh loop.
const errCodePasswordChangeRequired = "password_change_required"

// pendingPasswordChangeAllowlist is THE list of routes a token carrying
// must_change_password may reach, as ServeMux patterns ("METHOD /path", exactly as
// registered, so the request's matched r.Pattern is compared verbatim):
//
//   - POST /api/v1/auth/change-password - the one way out of the state;
//   - GET /api/v1/auth/me/capabilities - lets the front-end render the
//     forced-change screen (it reports must_change_password).
//
// Login, refresh, logout and the emailed-token flows are anonymous: they never
// pass through the stamp check, so the flag cannot block them and they need no
// entry (an architecture test proves they stay anonymous). Nothing else is exempt,
// and a new route is blocked by default: a request without a route pattern (not
// served by a ServeMux) matches nothing here and is blocked too.
var pendingPasswordChangeAllowlist = []string{
	"GET /api/v1/auth/me/capabilities",
	"POST /api/v1/auth/change-password",
}

// PendingPasswordChangeAllowlist returns a copy of the pinned allowlist, for the
// tests that assert it and enumerate every registered route against it.
func PendingPasswordChangeAllowlist() []string { return slices.Clone(pendingPasswordChangeAllowlist) }

// pendingPasswordChangeAllowed reports whether the matched route may be reached
// with a must_change_password token.
func pendingPasswordChangeAllowed(r *http.Request) bool {
	return r.Pattern != "" && slices.Contains(pendingPasswordChangeAllowlist, r.Pattern)
}

// errCodeImpersonationNotAllowed mirrors ports.ErrCodeImpersonationNotAllowed
// (the authority commands answer the same code from inside the handler).
const errCodeImpersonationNotAllowed = "impersonation_not_allowed"

// impersonationRefusedPrefix: the platform plane. Every /api/v1/platform/ route
// except the two tenant-plane reads in impersonationOpenPlatformReads is
// platform-wide or spends tenant money: impersonation
// sessions (no sub-impersonation, no listing, no ending), person administration,
// tenant deletion, stats, the unverified-contact queue, the lead marketplace
// (purchase spends tenant lead credits: refused while impersonating, pending owner
// confirmation, ADR 0069) and lead-credit top-ups. An impersonation token never
// reaches any of them, however many permissions its Admin-equivalent authority
// would satisfy. (Marketplace browse and the lead-credit balance live under this
// prefix but are tenant-plane reads gated by tenant permissions; they are the
// pinned exception below.)
const impersonationRefusedPrefix = "/api/v1/platform/"

// impersonationRefusedRoutes are the routes OUTSIDE the platform prefix an
// impersonation token must never reach, as ServeMux patterns (matched against the
// request's r.Pattern, like the allowlist above):
//
//   - the platform-anchored routes that live on tenant paths: tenant registration,
//     the lifecycle commands (suspend, activate, mark-for-deletion, restore), the
//     operator omni-search and user anonymisation (cross-tenant blast radius);
//   - the self-service routes that act on "the caller": credential, email,
//     sessions and activity of the synthetic membership an impersonation token
//     carries, and the permission-request commands (an impersonation session
//     cannot submit, approve, deny or cancel; the command refuses it too).
//
// Everything else is reachable with the Admin-equivalent authority the policy
// grants a session (never owner-level, never tenant.admin: the authority policy,
// not this list, decides that). GET /auth/me/capabilities stays open (the
// front-end renders the session from it) and so do the two permission-request
// reads (they list and show requests of the synthetic member: empty by design).
// An architecture test asserts this list against the registered routes.
var impersonationRefusedRoutes = []string{
	"DELETE /api/v1/auth/sessions",
	"DELETE /api/v1/auth/sessions/{familyId}",
	"GET /api/v1/auth/me/activity",
	"GET /api/v1/auth/sessions",
	"GET /api/v1/search",
	"POST /api/v1/auth/change-password",
	"POST /api/v1/auth/request-email-change",
	"POST /api/v1/permission-requests",
	"POST /api/v1/permission-requests/{requestId}/approve",
	"POST /api/v1/permission-requests/{requestId}/cancel",
	"POST /api/v1/permission-requests/{requestId}/deny",
	"POST /api/v1/tenants",
	"POST /api/v1/tenants/{tenantId}/activate",
	"POST /api/v1/tenants/{tenantId}/mark-for-deletion",
	"POST /api/v1/tenants/{tenantId}/restore",
	"POST /api/v1/tenants/{tenantId}/suspend",
	"POST /api/v1/users/{userId}/anonymise",
}

// ImpersonationRefusals returns a copy of the pinned refusal list (the routes
// outside the platform prefix), for the tests that assert it.
func ImpersonationRefusals() []string { return slices.Clone(impersonationRefusedRoutes) }

// ImpersonationRefused reports whether an impersonation token is refused on the
// route registered as pattern ("METHOD /path"): the platform plane (less the two
// pinned tenant-plane reads) or the pinned list. It fails CLOSED: an empty pattern
// (a request not served by a ServeMux, or a wrapper that lost r.Pattern) is
// refused, and a pattern with no method is read as a bare path, so the prefix rule
// still applies to it.
func ImpersonationRefused(pattern string) bool {
	if pattern == "" {
		return true
	}
	if slices.Contains(impersonationOpenPlatformReads, pattern) {
		return false
	}
	if slices.Contains(impersonationRefusedRoutes, pattern) {
		return true
	}
	path := pattern
	if _, rest, ok := strings.Cut(pattern, " "); ok {
		path = rest
	}
	return strings.HasPrefix(path, impersonationRefusedPrefix)
}

// impersonationOpenPlatformReads are the two tenant-plane reads that live under
// /api/v1/platform/ but are gated by TENANT permissions (marketplace.browse,
// lead_credits.read) and read the scoped tenant's own data. They are exempt from
// the platform-prefix rule so an operator supporting a tenant can see its balance
// and what it can buy. Exactly these two GET patterns; every other /platform/
// route, the marketplace purchase (it spends credits) included, stays refused.
var impersonationOpenPlatformReads = []string{
	"GET /api/v1/platform/marketplace/leads",
	"GET /api/v1/platform/lead-credits/balance",
}

// ImpersonationOpenPlatformReads returns a copy of the pinned exemption list, for
// the tests that assert it exactly.
func ImpersonationOpenPlatformReads() []string { return slices.Clone(impersonationOpenPlatformReads) }
