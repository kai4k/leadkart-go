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

	// ErrCodeSessionNotFound — DELETE /api/v1/auth/sessions/{familyId}
	// returned a not-found OR not-owned-by-caller. Same code per
	// `security.md` enumeration-safety: never tell the attacker which
	// arm matched. 404 surface.
	ErrCodeSessionNotFound = "session_not_found"

	// ErrCodeInvalidFamilyID — path parameter `{familyId}` failed
	// UUID parse. 400 surface.
	ErrCodeInvalidFamilyID = "invalid_family_id"

	// ErrCodeResetTokenInvalid — confirm-password-reset rejection
	// (mismatch / expired / no pending). Per security.md "Password
	// reset" + Auth0/Okta canon: NEVER distinguish causes. 400 surface.
	//nolint:gosec // G101: error code, not a credential
	ErrCodeResetTokenInvalid = "reset_token_invalid"

	// ErrCodeEmailChangeRejected — request-email-change rejected
	// (terminal Person, same-as-current, etc.). 400 surface.
	ErrCodeEmailChangeRejected = "email_change_rejected"

	// ErrCodeEmailAlreadyTaken — request-email-change rejected because
	// another Person already owns the new email. 409 surface.
	ErrCodeEmailAlreadyTaken = "email_already_taken"

	// ErrCodeEmailChangeTokenInvalid — confirm-email-change rejection.
	// 400 surface, same enumeration-safety rule as reset.
	//nolint:gosec // G101: error code, not a credential
	ErrCodeEmailChangeTokenInvalid = "email_change_token_invalid"

	// ErrCodeTenantNotFound — tenant ID has no matching row. 404 surface.
	ErrCodeTenantNotFound = "tenant_not_found"

	// ErrCodeInvalidTenantID — path parameter `{tenantId}` failed UUID
	// parse. 400 surface.
	ErrCodeInvalidTenantID = "invalid_tenant_id"

	// ErrCodeTenantInvalid — aggregate-level invariant violation
	// (over-length name, terminal-state transition, etc.). 422 surface.
	ErrCodeTenantInvalid = "tenant_invalid"

	// ErrCodePlatformTenantUndeletable — destructive lifecycle command
	// (Suspend / MarkForDeletion / HardDelete) targeted a tenant that
	// holds an active SuperAdmin role-holder. 422 surface. Operators
	// must rotate SuperAdmins off the tenant first per migration
	// 20260507000008's deletion guard.
	ErrCodePlatformTenantUndeletable = "platform_tenant_undeletable"

	// ErrCodeUserNotFound — membership ID has no row in caller's
	// tenant. 404 surface; collapses "wrong tenant" + "doesn't exist"
	// per security.md enumeration safety.
	ErrCodeUserNotFound = "user_not_found"

	// ErrCodeInvalidUserID — path parameter `{userId}` failed UUID parse.
	// 400 surface.
	ErrCodeInvalidUserID = "invalid_user_id"

	// ErrCodeUserInvalid — Membership aggregate-level invariant
	// violation (deactivate without reason, terminal-state transition,
	// etc.). 422 surface.
	ErrCodeUserInvalid = "user_invalid"

	// ErrCodeInvalidRoleID — path/body role_id failed UUID parse.
	// 400 surface.
	ErrCodeInvalidRoleID = "invalid_role_id"

	// ErrCodeInvalidManagerID — body manager_id failed UUID parse.
	// 400 surface.
	ErrCodeInvalidManagerID = "invalid_manager_id"

	// ErrCodePermissionUnknown — replace-permission-overrides request
	// carried a permission name not in [permission.IdentityPermissions].
	// 422 surface; offending name in message body.
	ErrCodePermissionUnknown = "permission_unknown"

	// ErrCodeRoleNotFound — role ID has no live row in caller's tenant.
	// 404 surface.
	ErrCodeRoleNotFound = "role_not_found"

	// ErrCodeRoleNameTaken — role create / rename collided with an
	// existing live role name in the tenant. 409 surface.
	ErrCodeRoleNameTaken = "role_name_taken"

	// ErrCodeRoleInvalid — Role aggregate-level invariant violation
	// (system-default mutation attempt, hierarchy out of range, etc.).
	// 422 surface.
	ErrCodeRoleInvalid = "role_invalid"

	// ErrCodePersonNotFound — Person ID has no row globally. 404 surface.
	ErrCodePersonNotFound = "person_not_found"

	// ErrCodeInvalidPersonID — path parameter `{personId}` failed UUID parse.
	// 400 surface.
	ErrCodeInvalidPersonID = "invalid_person_id"

	// ErrCodePersonInvalid — Person aggregate-level invariant violation
	// (terminal-state transition, anonymise-while-not-suspended, etc.).
	// 422 surface.
	ErrCodePersonInvalid = "person_invalid"

	// ErrCodeImpersonationInvalid — session-creation validation
	// rejection (reason too short, duration > 4h, etc.). 422 surface.
	ErrCodeImpersonationInvalid = "impersonation_invalid"

	// ErrCodeInvalidSessionID — `{sessionId}` path param failed UUID
	// parse. 400 surface. Distinct from session_not_found which the
	// session-revoke flow collapses into 204 idempotent no-op anyway.
	ErrCodeInvalidSessionID = "invalid_session_id"

	// ErrCodeInvalidDeltaWindow — ?delta_window= on GET /v1/platform/stats
	// wasn't in the closed set {24h, 7d, 30d}. 400 surface; closed-set
	// enforced to prevent cache-key explosion per ADR 0040.
	ErrCodeInvalidDeltaWindow = "invalid_delta_window"

	// ErrCodeInvalidCursor — paginated list endpoint received a cursor
	// that couldn't be base64-decoded or didn't carry valid JSON. 400
	// surface; clients should retry without the cursor (loads page 1).
	ErrCodeInvalidCursor = "invalid_cursor"
)
