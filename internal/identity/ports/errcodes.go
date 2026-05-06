package ports

// HTTP error codes — wire-stable identifiers consumed by clients
// (Svelte SPA, mobile apps, third-party integrations). Renaming any
// constant below is a BREAKING CHANGE: bump the API version and run
// the rename through expand-contract.
//
// Per `coding-standards.md` "No magic strings — production AND tests":
// every error code emitted by this package is declared here once and
// referenced by name everywhere else.
//
// Convention: snake_case strings, lowercase. Stripe + GitHub + AWS
// API responses converged on this shape pre-2020.
const (
	// ErrCodeInvalidBody — POST/PUT/PATCH body fails JSON decode.
	ErrCodeInvalidBody = "invalid_body"

	// ErrCodeInvalidSlug — slug VO factory rejected the input
	// (length / charset / reserved-name fail).
	ErrCodeInvalidSlug = "invalid_slug"

	// ErrCodeInvalidEmail — email VO factory rejected the input.
	ErrCodeInvalidEmail = "invalid_email"

	// ErrCodeEmailHasActiveMembership — surface for the single-Active-
	// Membership invariant per `multi-tenancy.md` "Identity model".
	// Returned 409 when admin creates a user whose email already
	// holds an active Membership in another tenant.
	ErrCodeEmailHasActiveMembership = "email_has_active_membership"

	// ErrCodeInvalidCredentials — login wrong-password / unknown-
	// email / suspended-tenant / no-active-membership all collapse
	// to this code per `security.md` "Login flow — enumeration safety".
	// NEVER differentiate the cause in the response body.
	ErrCodeInvalidCredentials = "invalid_credentials" //nolint:gosec // G101: error code, not a credential

	// ErrCodeRefreshRejected — refresh-token rotation refused
	// (consumed / expired / family revoked / reuse detected). Per
	// RFC 9700 §4.13: never disclose which arm failed — could leak
	// reuse-detection signal to attacker holding stolen token.
	ErrCodeRefreshRejected = "refresh_rejected"

	// ErrCodeInternalError — generic 500 surface. Detail goes to
	// slog.ErrorContext server-side; HTTP body stays empty to avoid
	// leaking stack traces / SQL fragments to clients.
	ErrCodeInternalError = "internal_error"

	// ErrCodeIncorrectCurrentPassword — change-password verify-current
	// gate failed. 401 surface — same shape as login's invalid_credentials
	// per `security.md` "Password change" + Auth0/Okta canon: never
	// distinguish "wrong current password" from other auth failures.
	//nolint:gosec // G101: error code, not a credential
	ErrCodeIncorrectCurrentPassword = "incorrect_current_password"

	// ErrCodePasswordBreached — HIBP-style breach checker rejected the
	// new password. 422 surface; UI prompts user to choose another.
	//nolint:gosec // G101: error code, not a credential
	ErrCodePasswordBreached = "password_breached"

	// ErrCodePasswordSameAsCurrent — change-password caller supplied
	// new == current. 422 surface. Per Auth0 + Okta canon: reject the
	// no-op rather than silently committing nothing.
	//nolint:gosec // G101: error code, not a credential
	ErrCodePasswordSameAsCurrent = "password_same_as_current"

	// ErrCodeInvalidPassword — generic password-shape rejection
	// (empty / too short / too long). Used by change-password +
	// reset-password endpoints when the password is missing or
	// fails domain-layer validation. 400 surface.
	//nolint:gosec // G101: error code, not a credential
	ErrCodeInvalidPassword = "invalid_password"
)
