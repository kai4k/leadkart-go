// Package ports holds Identity inbound concrete implementations per
// TDL canon — HTTP handlers, future event subscribers. Packages under
// here translate external requests into Application command/query
// handler calls.
package ports

import (
	"encoding/json"
	"time"
)

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
	Email    string `json:"email"`
	Password string `json:"password"`
	// DeviceLabel is optional from the client. When empty, the handler
	// derives it from User-Agent (truncated to 128 chars), then
	// RemoteAddr, then "Unknown device" — see resolveDeviceLabel in
	// http.go. The domain (refreshtoken.NewFamily) requires a non-empty
	// label; the boundary guarantees that invariant before the call.
	// Auth0 / Stripe / GitHub all do this so clients don't have to
	// compute their own labels.
	DeviceLabel string `json:"device_label,omitempty"`
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

// ----- Audit-log reads (GET /v1/auth/me/activity, GET /v1/tenants/{id}/activity) ----

// AuditEventDto is one row of the audit-log read shape per
// ADR 0027 (outbox doubles as audit). Per-event minimum: action +
// timestamp + outcome; payload is raw JSON the frontend can render
// per-action as needed.
type AuditEventDto struct {
	ID            string          `json:"id"`
	Action        string          `json:"action"`
	ActorID       string          `json:"actor_id,omitempty"`
	TenantID      string          `json:"tenant_id,omitempty"`
	CorrelationID string          `json:"correlation_id,omitempty"`
	OccurredAt    time.Time       `json:"occurred_at"`
	DurationMs    int64           `json:"duration_ms"`
	Succeeded     bool            `json:"succeeded"`
	FailureReason string          `json:"failure_reason,omitempty"`
	Payload       json.RawMessage `json:"payload,omitempty"`
}

// ListAuditEventsResponse — keyset-paginated wire shape per ADR
// 0038. events[] always non-nil; next_cursor empty when last page.
type ListAuditEventsResponse struct {
	Events     []AuditEventDto `json:"events"`
	HasMore    bool            `json:"has_more"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

// ----- Omni-search (GET /v1/search) ------------------------------------------

// SearchResponse — GET /v1/search?q=&limit=&include=
//
// Operator omni-search wire shape per ADR 0040. Two categories
// returned; both slices always non-nil (empty when no matches).
// has_partial=true signals a sub-query exceeded its per-category
// timeout (200ms) — frontend renders what's there and may show a
// "partial results" hint.
type SearchResponse struct {
	Persons    []SearchPersonHit `json:"persons"`
	Tenants    []SearchTenantHit `json:"tenants"`
	HasPartial bool              `json:"has_partial"`
}

// SearchPersonHit is one persons-category match.
type SearchPersonHit struct {
	ID        string    `json:"id"`
	Email     string    `json:"email"`
	FirstName string    `json:"first_name"`
	LastName  string    `json:"last_name"`
	CreatedAt time.Time `json:"created_at"`
}

// SearchTenantHit is one tenants-category match.
type SearchTenantHit struct {
	ID          string    `json:"id"`
	Slug        string    `json:"slug"`
	LegalName   string    `json:"legal_name"`
	DisplayName string    `json:"display_name"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
}

// ----- Capabilities (GET /v1/auth/me/capabilities) ---------------------------

// CapabilitiesDto is the wire shape of GET /v1/auth/me/capabilities.
//
// Mirrors Auth0 /userinfo + Microsoft Graph /me semantics — returns
// the resolved permission/role/profile bundle for the calling
// membership so the frontend never has to decode the JWT to drive
// nav / tier / button-visibility.
//
// Field provenance:
//   - JWT-resident (zero DB hit): person_id, membership_id, tenant_id,
//     tenant_slug, is_platform, is_super_user, permissions[].
//   - Enriched (cached via cache.CapabilitiesTTL — 2min L1 / 15min L2;
//     ADR 0042): email, first_name, last_name, roles[]. Cache key
//     includes the security_stamp so stamp rotation invalidates
//     implicitly per ADR 0028.
//
// Caller derivation rules (see handler godoc):
//   - is_platform IS the slug-anchored claim per Phase 1.5 hardening —
//     true iff (claim.is_platform AND tenant_slug == "platform").
//   - permissions is the closed-set catalogue list (always returned,
//     [] for a fresh member with no role-driven permissions).
//   - roles always returned ([] for fresh member); each entry carries
//     is_super_admin so the frontend can render the SuperAdmin chip
//     without re-parsing the permissions array.
type CapabilitiesDto struct {
	PersonID     string              `json:"person_id"`
	MembershipID string              `json:"membership_id"`
	TenantID     string              `json:"tenant_id"`
	TenantSlug   string              `json:"tenant_slug"`
	Email        string              `json:"email,omitempty"`
	FirstName    string              `json:"first_name,omitempty"`
	LastName     string              `json:"last_name,omitempty"`
	IsPlatform   bool                `json:"is_platform"`
	IsSuperUser  bool                `json:"is_super_user"`
	Permissions  []string            `json:"permissions"`
	Roles        []CapabilityRoleDto `json:"roles"`
}

// CapabilityRoleDto is one resolved Role surface inside the
// capabilities bundle. Carries display name (drives the UI
// "your role: X" widget) plus is_super_admin so the frontend can
// render the SuperAdmin chip / unlock special UX without
// re-parsing the permissions array.
type CapabilityRoleDto struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	IsSuperAdmin bool   `json:"is_super_admin"`
}

// ----- User management -------------------------------------------------------

// UserDto is the wire-shape of a Membership composed with its
// underlying Person's identity fields. "User" in HTTP vocabulary
// always means one (Person, Tenant) Membership row.
type UserDto struct {
	MembershipID  string    `json:"membership_id"`
	PersonID      string    `json:"person_id"`
	TenantID      string    `json:"tenant_id"`
	Email         string    `json:"email"`
	FirstName     string    `json:"first_name"`
	LastName      string    `json:"last_name"`
	Status        string    `json:"status"`
	Designation   string    `json:"designation,omitempty"`
	Department    string    `json:"department,omitempty"`
	StatusMessage string    `json:"status_message,omitempty"`
	JoinedAt      time.Time `json:"joined_at,omitzero"`
	LeftAt        time.Time `json:"left_at,omitzero"`
	ReportsTo     string    `json:"reports_to,omitempty"`
	RoleIDs       []string  `json:"role_ids"`
}

// ListUsersResponse — GET /api/v1/users.
//
// Paginated wire shape per ADR 0038 — cursor (keyset) over offset.
// `users` is the canonical resource-name key the frontend already
// expects from v0.1; `has_more` + `next_cursor` are the pagination
// metadata. Total count intentionally omitted (O(n) under RLS;
// frontend uses has_more for "load more" UX per ADR 0038 non-goals).
type ListUsersResponse struct {
	Users      []UserDto `json:"users"`
	HasMore    bool      `json:"has_more"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

// UpdateUserProfileRequest — PATCH /api/v1/users/{userId}/profile.
type UpdateUserProfileRequest struct {
	Designation   string `json:"designation"`
	Department    string `json:"department"`
	StatusMessage string `json:"status_message"`
}

// DeactivateUserRequest — POST /api/v1/users/{userId}/deactivate.
// Reason MUST be non-empty per data-retention.md audit canon.
type DeactivateUserRequest struct {
	Reason string `json:"reason"`
}

// AssignUserRoleRequest — POST /api/v1/users/{userId}/roles.
type AssignUserRoleRequest struct {
	RoleID string `json:"role_id"`
}

// ReplaceUserPermissionOverridesRequest — PATCH .../permission-overrides.
//
// Atomic replacement of both overlays. Empty arrays clear the
// overlay. Permission names MUST match the closed [permission.
// IdentityPermissions] catalogue; unknown names → 422.
type ReplaceUserPermissionOverridesRequest struct {
	Granted []string `json:"granted"`
	Revoked []string `json:"revoked"`
}

// AssignUserManagerRequest — PUT /api/v1/users/{userId}/manager.
//
// ManagerID MUST be a Membership in the same tenant — composite FK
// at the schema level enforces this; cross-tenant ID surfaces as 422.
type AssignUserManagerRequest struct {
	ManagerID string `json:"manager_id"`
}

// CreateUserRequest — POST /api/v1/users. Find-or-create-by-email
// server-side per Auth0/Microsoft Entra ID canon: caller supplies
// email + password + names; server decides whether to attach to an
// existing global Person or create a new one.
type CreateUserRequest struct {
	Email     string `json:"email"`
	Password  string `json:"password"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

// CreateUserResponse — POST /api/v1/users 201 body.
//
// PersonExisted lets the UI distinguish "we attached you to an
// existing global identity" from "we created a fresh Person record" —
// useful for the admin who entered the email so they understand the
// new user already had an account elsewhere.
type CreateUserResponse struct {
	PersonID      string `json:"person_id"`
	MembershipID  string `json:"membership_id"`
	PersonExisted bool   `json:"person_existed"`
}

// ----- Platform: cross-tenant Person + tenant ops ----------------------------

// PersonDto is the wire-shape of a Person for Platform read endpoints.
// Returned by GET /api/v1/platform/persons/{personId}. Password hash
// + security stamp are NEVER exposed.
type PersonDto struct {
	ID                     string    `json:"id"`
	Email                  string    `json:"email"`
	FirstName              string    `json:"first_name"`
	LastName               string    `json:"last_name"`
	IsActive               bool      `json:"is_active"`
	IsAnonymised           bool      `json:"is_anonymised"`
	IsGloballySuspended    bool      `json:"is_globally_suspended"`
	GlobalSuspensionReason string    `json:"global_suspension_reason,omitempty"`
	GloballySuspendedAt    time.Time `json:"globally_suspended_at,omitzero"`
	CreatedAt              time.Time `json:"created_at"`
	AnonymisedAt           time.Time `json:"anonymised_at,omitzero"`
}

// GlobalSuspendPersonRequest — POST /api/v1/platform/persons/{personId}/global-suspend.
// Reason MUST be non-empty per data-retention.md audit canon.
type GlobalSuspendPersonRequest struct {
	Reason string `json:"reason"`
}

// UpdatePersonProfileRequest — PATCH /api/v1/platform/persons/{personId}/profile.
// Updates the GLOBAL Person profile (FirstName, LastName) — distinct
// from the per-Tenant designation/department/status_message which
// move via [UpdateUserProfileRequest].
type UpdatePersonProfileRequest struct {
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

// ListPersonMembershipsResponse — GET /api/v1/platform/persons/{personId}/memberships.
// Cross-tenant view of every Membership the Person holds.
type ListPersonMembershipsResponse struct {
	Memberships []UserDto `json:"memberships"`
}

// ListAllTenantsResponse — GET /api/v1/platform/tenants.
// Cross-tenant operator dashboard listing.
type ListAllTenantsResponse struct {
	Tenants []TenantDto `json:"tenants"`
}

// ----- Platform: Impersonation sessions --------------------------------------

// CreateImpersonationSessionRequest — POST /api/v1/platform/impersonation/sessions.
// Reason MUST be ≥10 chars; DurationMinutes optional (defaults 30,
// max 240). TargetTenantId is the tenant the operator wants to act as.
type CreateImpersonationSessionRequest struct {
	TargetTenantID  string `json:"target_tenant_id"`
	Reason          string `json:"reason"`
	DurationMinutes int    `json:"duration_minutes,omitempty"`
}

// CreateImpersonationSessionResponse — 201 body.
type CreateImpersonationSessionResponse struct {
	SessionID    string    `json:"session_id"`
	ExpiresAtUTC time.Time `json:"expires_at_utc"`
}

// ImpersonationSessionDto is one entry in the GET response.
type ImpersonationSessionDto struct {
	SessionID      string    `json:"session_id"`
	OperatorID     string    `json:"operator_id"`
	TargetTenantID string    `json:"target_tenant_id"`
	Reason         string    `json:"reason"`
	CreatedAt      time.Time `json:"created_at"`
	ExpiresAt      time.Time `json:"expires_at"`
}

// ListImpersonationSessionsResponse — GET .../impersonation/sessions.
type ListImpersonationSessionsResponse struct {
	Sessions []ImpersonationSessionDto `json:"sessions"`
}

// ----- Platform: Stats -------------------------------------------------------

// PlatformStatsResponse — GET /api/v1/platform/stats. Operator
// dashboard at-a-glance counts. Single round-trip from caller.
//
// Deltas is populated when the request specifies ?delta_window=24h|7d|30d
// (closed set per ADR 0040 — cache-key-explosion prevention). Each
// delta count is "new rows since now() - window" for the matching
// base metric. Omitted from the wire shape when no delta was asked
// for (omitempty + pointer-to-struct).
//
// Cached server-side via HybridCache facade keyed by (delta_window)
// with 5min TTL per ADR 0040.
type PlatformStatsResponse struct {
	TenantsTotal      int                  `json:"tenants_total"`
	TenantsActive     int                  `json:"tenants_active"`
	TenantsSuspended  int                  `json:"tenants_suspended"`
	PersonsTotal      int                  `json:"persons_total"`
	MembershipsActive int                  `json:"memberships_active"`
	Deltas            *PlatformStatsDeltas `json:"deltas,omitempty"`
}

// PlatformStatsDeltas carries the "Δ in the last <window>" widget data.
// Same metric names as the base counts so the frontend can render a
// "<base> (+<delta> this <window>)" UI uniformly.
type PlatformStatsDeltas struct {
	Window            string `json:"window"` // "24h" | "7d" | "30d"
	TenantsTotal      int    `json:"tenants_total"`
	TenantsActive     int    `json:"tenants_active"`
	PersonsTotal      int    `json:"persons_total"`
	MembershipsActive int    `json:"memberships_active"`
}

// ----- Role management -------------------------------------------------------

// RoleDto is the wire-shape of a [role.Role] for read endpoints.
type RoleDto struct {
	ID              string    `json:"id"`
	TenantID        string    `json:"tenant_id"`
	Name            string    `json:"name"`
	IsSystemDefault bool      `json:"is_system_default"`
	IsSuperAdmin    bool      `json:"is_super_admin"`
	HierarchyLevel  int       `json:"hierarchy_level"`
	Permissions     []string  `json:"permissions"`
	CreatedAt       time.Time `json:"created_at"`
}

// ListRolesResponse — GET /api/v1/roles.
type ListRolesResponse struct {
	Roles []RoleDto `json:"roles"`
}

// CreateRoleRequest — POST /api/v1/roles.
//
// HierarchyLevel must be in role.HierarchyLevelMin..HierarchyLevelMax.
// IsSuperAdmin is intentionally absent — see CreateRoleCommand
// godoc for the seed-only invariant.
type CreateRoleRequest struct {
	Name           string `json:"name"`
	HierarchyLevel int    `json:"hierarchy_level"`
}

// CreateRoleResponse — POST /api/v1/roles 201 body.
type CreateRoleResponse struct {
	RoleID string `json:"role_id"`
}

// UpdateRoleRequest — PATCH /api/v1/roles/{roleId}.
//
// Both fields optional. Empty Name skips rename. HierarchyLevel
// nil-pointer skips re-level (using a pointer instead of -1 sentinel
// keeps the wire shape clean).
type UpdateRoleRequest struct {
	Name           string `json:"name,omitempty"`
	HierarchyLevel *int   `json:"hierarchy_level,omitempty"`
}

// ReplaceRolePermissionsRequest — PUT /api/v1/roles/{roleId}/permissions.
type ReplaceRolePermissionsRequest struct {
	Permissions []string `json:"permissions"`
}

// RolePermissionRequest — POST /api/v1/roles/{roleId}/permissions/{grant,revoke}.
type RolePermissionRequest struct {
	Permission string `json:"permission"`
}

// ----- Tenant management -----------------------------------------------------

// TenantDto is the wire-shape of a Tenant for read endpoints. Mirrors
// the .NET LeadKart `TenantDto` — full profile + statutory + contact +
// settings + display preferences + lifecycle timestamps.
type TenantDto struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	LegalName   string `json:"legal_name"`
	DisplayName string `json:"display_name"`
	// admin_email removed in migration 20260507000008 — derived value
	// (CompanyOwner-role membership → person.email). Use the
	// /v1/users endpoints to discover current admin contacts.
	Status              string             `json:"status"`
	CreatedAt           time.Time          `json:"created_at"`
	ActivatedAt         time.Time          `json:"activated_at,omitzero"`
	SuspendedAt         time.Time          `json:"suspended_at,omitzero"`
	DeletionScheduledAt time.Time          `json:"deletion_scheduled_at,omitzero"`
	DeletionReason      string             `json:"deletion_reason,omitempty"`
	GSTNumber           string             `json:"gst_number,omitempty"`
	PANNumber           string             `json:"pan_number,omitempty"`
	DrugLicenceNumber   string             `json:"drug_licence_number,omitempty"`
	AdminPhone          string             `json:"admin_phone,omitempty"`
	AdminAddress        AdminAddressDto    `json:"admin_address"`
	PasswordPolicy      PasswordPolicyDto  `json:"password_policy"`
	Locale              string             `json:"locale,omitempty"`
	TimeZone            string             `json:"time_zone,omitempty"`
	DateFormat          string             `json:"date_format,omitempty"`
	Currency            string             `json:"currency,omitempty"`
}

// AdminAddressDto is the postal-address slice of [TenantDto].
type AdminAddressDto struct {
	Street    string `json:"street,omitempty"`
	City      string `json:"city,omitempty"`
	District  string `json:"district,omitempty"`
	State     string `json:"state,omitempty"`
	StateCode string `json:"state_code,omitempty"`
	Pincode   string `json:"pincode,omitempty"`
}

// PasswordPolicyDto is the password-policy slice of [TenantDto].
type PasswordPolicyDto struct {
	MinLength         int  `json:"min_length"`
	RequireUppercase  bool `json:"require_uppercase"`
	RequireLowercase  bool `json:"require_lowercase"`
	RequireDigit      bool `json:"require_digit"`
	RequireSymbol     bool `json:"require_symbol"`
	MaxFailedAttempts int  `json:"max_failed_attempts"`
	LockoutMinutes    int  `json:"lockout_minutes"`
}

// UpdateTenantProfileRequest — PATCH /api/v1/tenants/{tenantId}/profile.
type UpdateTenantProfileRequest struct {
	LegalName   string `json:"legal_name"`
	DisplayName string `json:"display_name"`
}

// UpdateTenantStatutoryRequest — PATCH /api/v1/tenants/{tenantId}/statutory.
//
// Empty strings clear the corresponding declaration; the aggregate
// accepts a zero [tenant.Statutory] for tenants that haven't yet
// onboarded their compliance IDs.
type UpdateTenantStatutoryRequest struct {
	GSTNumber         string `json:"gst_number"`
	PANNumber         string `json:"pan_number"`
	DrugLicenceNumber string `json:"drug_licence_number"`
}

// UpdateTenantAdminContactRequest — PATCH .../admin-contact.
type UpdateTenantAdminContactRequest struct {
	Phone   string          `json:"phone"`
	Address AdminAddressDto `json:"address"`
}

// UpdateTenantSettingsRequest — PATCH .../settings.
type UpdateTenantSettingsRequest struct {
	PasswordPolicy PasswordPolicyDto `json:"password_policy"`
}

// UpdateTenantDisplayPreferencesRequest — PATCH .../display-preferences.
type UpdateTenantDisplayPreferencesRequest struct {
	Locale     string `json:"locale"`
	TimeZone   string `json:"time_zone"`
	DateFormat string `json:"date_format"`
	Currency   string `json:"currency"`
}

// SuspendTenantRequest — POST .../suspend. Reason MUST be non-empty
// (audit requirement per data-retention.md).
type SuspendTenantRequest struct {
	Reason string `json:"reason"`
}

// MarkTenantForDeletionRequest — POST .../mark-for-deletion. Reason
// MUST be non-empty (DPDP §12 + SOC2 CC4.1 audit requirement).
type MarkTenantForDeletionRequest struct {
	Reason string `json:"reason"`
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
