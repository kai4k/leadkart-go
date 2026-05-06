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

// ----- Errors ----------------------------------------------------------------

// ErrorResponse is the shared 4xx/5xx body shape. RFC 9457 problem-detail
// canon would add `type`/`title`/`detail`; this v0.1 cut keeps it
// minimal. Upgrade in Phase 6 when the .NET ProblemDetails port lands.
type ErrorResponse struct {
	Error   string `json:"error"`              // machine-parseable code
	Message string `json:"message,omitempty"`  // human-readable
}
