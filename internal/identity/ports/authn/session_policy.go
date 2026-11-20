package authn

import (
	"net/http"
	"slices"
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
