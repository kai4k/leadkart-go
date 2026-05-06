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
)
