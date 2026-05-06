// Package ports holds Identity inbound concrete implementations per
// TDL canon — HTTP handlers, future event subscribers. Packages under
// here translate external requests into Application command/query
// handler calls.
package ports

import "time"

// ----- RegisterTenant --------------------------------------------------------

// RegisterTenantRequest is the wire shape for POST /api/v1/tenants.
//
// JSON snake_case mirrors the .NET Web API; the same Blazor frontend
// can talk to either backend without renaming fields. Validation lives
// in the handler — DTOs only carry shape.
type RegisterTenantRequest struct {
	Slug          string `json:"slug"`
	LegalName     string `json:"legal_name"`
	DisplayName   string `json:"display_name"`
	AdminEmail    string `json:"admin_email"`
	AdminPassword string `json:"admin_password"`
	AdminFirstName string `json:"admin_first_name"`
	AdminLastName  string `json:"admin_last_name"`
}

// RegisterTenantResponse is the 201 body — IDs only. Discoverability
// of the new tenant happens through GET /api/v1/tenants/{id}; the
// minimum response surface here keeps the contract small.
type RegisterTenantResponse struct {
	TenantID     string `json:"tenant_id"`
	PersonID     string `json:"person_id"`
	MembershipID string `json:"membership_id"`
}

// ----- Login -----------------------------------------------------------------

// LoginRequest — POST /api/v1/auth/login.
//
// Per multi-tenancy.md "Login flow consequence": NO tenant_slug field.
// The single-Active-Membership invariant resolves tenant context
// implicitly. Same shape as the .NET version after the Identity-model
// rebuild.
type LoginRequest struct {
	Email        string `json:"email"`
	Password     string `json:"password"`
	DeviceLabel  string `json:"device_label,omitempty"`
}

// LoginResponse / RefreshResponse are identical structurally.
//
// Refresh-token plaintext is returned in the BODY (not a cookie) for
// JWT-bearer clients (mobile, integrations). The future Blazor BFF
// will sit in front and convert the body refresh-token to an HttpOnly
// cookie + ITicketStore session per security.md "BFF cookie".
type LoginResponse struct {
	AccessToken          string    `json:"access_token"`
	RefreshToken         string    `json:"refresh_token"`
	AccessTokenExpiresAt time.Time `json:"access_token_expires_at"`
	TokenType            string    `json:"token_type"` // always "Bearer"
}

// ----- Refresh ---------------------------------------------------------------

// RefreshRequest — POST /api/v1/auth/refresh.
type RefreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

// ----- Logout ----------------------------------------------------------------

// LogoutRequest — POST /api/v1/auth/logout. Idempotent.
type LogoutRequest struct {
	RefreshToken string `json:"refresh_token"`
	Reason       string `json:"reason,omitempty"`
}

// ----- Password reset --------------------------------------------------------

// RequestPasswordResetRequest — POST /api/v1/auth/request-password-reset.
//
// Anonymous endpoint. Per Auth0/Okta canon: ALWAYS returns 204 No
// Content regardless of whether the email is registered — defeats
// account enumeration.
type RequestPasswordResetRequest struct {
	Email string `json:"email"`
}

// ResetPasswordRequest — POST /api/v1/auth/reset-password.
//
// Anonymous endpoint. Caller supplies the plaintext token from the
// emailed link + their chosen new password.
type ResetPasswordRequest struct {
	Token       string `json:"token"`
	NewPassword string `json:"new_password"`
}

// ----- Email change ---------------------------------------------------------

// RequestEmailChangeRequest — POST /api/v1/auth/request-email-change.
//
// Authenticated route. PersonID comes from JWT Subject; new email
// arrives in the body. Confirmation link is emailed to the NEW
// address.
type RequestEmailChangeRequest struct {
	NewEmail string `json:"new_email"`
}

// ConfirmEmailChangeRequest — POST /api/v1/auth/confirm-email-change.
//
// Anonymous endpoint (the confirmation link works without a session
// because the token IS the proof). Caller supplies the plaintext
// token from the emailed link.
type ConfirmEmailChangeRequest struct {
	Token string `json:"token"`
}

// ----- ChangePassword --------------------------------------------------------

// ChangePasswordRequest — POST /api/v1/auth/change-password.
//
// Authenticated route — PersonID comes from the JWT (set by
// RequireAuth + tenancy bridge per A.2.3), NOT from the body. Per
// security.md "Password change": the current password MUST be
// verified even when authenticated, otherwise an attacker with a
// stolen access token could permanently take over an account.
type ChangePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// ----- Sessions --------------------------------------------------------------

// SessionDto is one entry in the GET /api/v1/auth/sessions response.
//
// Auth0 / Okta / GitHub session-management UIs converged on this
// minimal surface: device label + when created + when last refreshed.
// Tokens / hashes are NEVER exposed.
type SessionDto struct {
	FamilyID    string    `json:"family_id"`
	TenantID    string    `json:"tenant_id"`
	DeviceLabel string    `json:"device_label"`
	CreatedAt   time.Time `json:"created_at"`
	LastUsedAt  time.Time `json:"last_used_at"`
}

// ListSessionsResponse — GET /api/v1/auth/sessions.
type ListSessionsResponse struct {
	Sessions []SessionDto `json:"sessions"`
}

// RevokeAllSessionsRequest — DELETE /api/v1/auth/sessions body.
//
// Optional. ExceptCurrent=true keeps the caller's CURRENT session
// alive (Auth0 / Okta default — "sign me out of OTHER devices").
// Reason is an audit string; defaults to "user_revoked_all" server-side.
type RevokeAllSessionsRequest struct {
	ExceptCurrent bool   `json:"except_current,omitempty"`
	Reason        string `json:"reason,omitempty"`
}

// RevokeAllSessionsResponse — DELETE /api/v1/auth/sessions response.
type RevokeAllSessionsResponse struct {
	RevokedCount int `json:"revoked_count"`
}

// ----- Errors ----------------------------------------------------------------

// ErrorResponse is the shared 4xx/5xx body shape. RFC 9457 problem-detail
// canon would add `type`/`title`/`detail`; this v0.1 cut keeps it
// minimal. Upgrade in Phase 6 when the .NET ProblemDetails port lands.
type ErrorResponse struct {
	Error   string `json:"error"`              // machine-parseable code
	Message string `json:"message,omitempty"`  // human-readable
}
