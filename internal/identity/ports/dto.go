// Package ports holds Identity inbound concretes per TDL canon — HTTP
// handlers (and future event subscribers) that translate external
// requests into Application command/query calls.
package ports

import (
	"encoding/json"
	"time"
)

// ----- RegisterTenant --------------------------------------------------------

// RegisterTenantRequest is the POST /api/v1/tenants body.
//
// snake_case mirrors the .NET Web API so one frontend talks to either
// backend. Validation lives in the handler; DTOs carry shape only.
type RegisterTenantRequest struct {
	Slug           string `json:"slug"`
	LegalName      string `json:"legal_name"`
	DisplayName    string `json:"display_name"`
	AdminEmail     string `json:"admin_email"`
	AdminPassword  string `json:"admin_password"`
	AdminFirstName string `json:"admin_first_name"`
	AdminLastName  string `json:"admin_last_name"`
}

// RegisterTenantResponse is the 201 body — IDs only; fetch the rest via
// GET /api/v1/tenants/{id}.
type RegisterTenantResponse struct {
	TenantID     string `json:"tenant_id"`
	PersonID     string `json:"person_id"`
	MembershipID string `json:"membership_id"`
}

// ----- Login -----------------------------------------------------------------

// LoginRequest — POST /api/v1/auth/login.
//
// No tenant_slug field: the single-Active-Membership invariant resolves
// tenant context implicitly (multi-tenancy.md "Login flow consequence").
type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	// DeviceLabel is optional; when empty the handler derives it
	// (User-Agent → RemoteAddr → "Unknown device", see resolveDeviceLabel).
	DeviceLabel string `json:"device_label,omitzero"`
}

// LoginResponse doubles as the refresh response.
//
// Refresh-token plaintext is in the BODY (not a cookie) for bearer
// clients; the BFF converts it to an HttpOnly cookie (security.md "BFF
// cookie"). MustChangePassword is the BRD forced-rotation flag — the
// frontend MUST route to change-password when true.
type LoginResponse struct {
	AccessToken          string    `json:"access_token"`
	RefreshToken         string    `json:"refresh_token"`
	AccessTokenExpiresAt time.Time `json:"access_token_expires_at"`
	TokenType            string    `json:"token_type"` // always "Bearer"
	MustChangePassword   bool      `json:"must_change_password,omitzero"`
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
	Reason       string `json:"reason,omitzero"`
}

// ----- Password reset --------------------------------------------------------

// RequestPasswordResetRequest — POST /api/v1/auth/request-password-reset.
//
// Anonymous. Always 204 regardless of email registration (Auth0/Okta) —
// defeats enumeration.
type RequestPasswordResetRequest struct {
	Email string `json:"email"`
}

// ResetPasswordRequest — POST /api/v1/auth/reset-password. Anonymous;
// carries the emailed token + the new password.
type ResetPasswordRequest struct {
	Token       string `json:"token"`
	NewPassword string `json:"new_password"`
}

// ----- Email change ---------------------------------------------------------

// RequestEmailChangeRequest — POST /api/v1/auth/request-email-change.
// Authenticated; PersonID from JWT Subject, confirmation link emailed to
// the new address.
type RequestEmailChangeRequest struct {
	NewEmail string `json:"new_email"`
}

// ConfirmEmailChangeRequest — POST /api/v1/auth/confirm-email-change.
// Anonymous — the emailed token IS the proof, no session needed.
type ConfirmEmailChangeRequest struct {
	Token string `json:"token"`
}

// ----- ChangePassword --------------------------------------------------------

// ChangePasswordRequest — POST /api/v1/auth/change-password.
// Authenticated; PersonID from the JWT, not the body. The current
// password MUST be verified even when authenticated, else a stolen
// access token = account takeover (security.md "Password change").
type ChangePasswordRequest struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

// ----- Audit-log reads (GET /v1/auth/me/activity, GET /v1/tenants/{id}/audit/events) ----

// AuditEventDto is one audit-log row per ADR 0027 (outbox doubles as
// audit): action + timestamp + outcome, plus raw-JSON payload.
type AuditEventDto struct {
	ID            string          `json:"id"`
	Action        string          `json:"action"`
	ActorID       string          `json:"actor_id,omitzero"`
	TenantID      string          `json:"tenant_id,omitzero"`
	CorrelationID string          `json:"correlation_id,omitzero"`
	OccurredAt    time.Time       `json:"occurred_at"`
	DurationMs    int64           `json:"duration_ms"`
	Succeeded     bool            `json:"succeeded"`
	FailureReason string          `json:"failure_reason,omitzero"`
	Payload       json.RawMessage `json:"payload,omitzero"`
}

// ListAuditEventsResponse — keyset-paginated per ADR 0038. events[]
// always non-nil; next_cursor empty on the last page.
type ListAuditEventsResponse struct {
	Events     []AuditEventDto `json:"events"`
	HasMore    bool            `json:"has_more"`
	NextCursor string          `json:"next_cursor,omitzero"`
}

// ----- Omni-search (GET /v1/search) ------------------------------------------

// SearchResponse — GET /v1/search omni-search per ADR 0040. Both slices
// always non-nil. has_partial=true means a sub-query exceeded its 200ms
// per-category timeout; render what's there.
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

// CapabilitiesDto is the GET /v1/auth/me/capabilities shape — the
// resolved permission/role/profile bundle (Auth0 /userinfo + MS Graph
// /me). See handleGetCapabilities for field provenance and derivation.
//
// permissions and roles are always non-nil ([] for a fresh member);
// each role carries is_super_admin so the frontend renders the
// SuperAdmin chip without re-parsing permissions.
type CapabilitiesDto struct {
	PersonID     string              `json:"person_id"`
	MembershipID string              `json:"membership_id"`
	TenantID     string              `json:"tenant_id"`
	TenantSlug   string              `json:"tenant_slug"`
	Email        string              `json:"email,omitzero"`
	FirstName    string              `json:"first_name,omitzero"`
	LastName     string              `json:"last_name,omitzero"`
	IsPlatform   bool                `json:"is_platform"`
	IsSuperUser  bool                `json:"is_super_user"`
	Permissions  []string            `json:"permissions"`
	Roles        []CapabilityRoleDto `json:"roles"`
}

// CapabilityRoleDto is one role inside the capabilities bundle: display
// name + is_super_admin for the SuperAdmin chip.
type CapabilityRoleDto struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	IsSuperAdmin bool   `json:"is_super_admin"`
}

// ----- User management -------------------------------------------------------

// UserDto is a Membership composed with its Person's identity fields.
// "User" in HTTP vocabulary = one (Person, Tenant) Membership row.
type UserDto struct {
	MembershipID  string    `json:"membership_id"`
	PersonID      string    `json:"person_id"`
	TenantID      string    `json:"tenant_id"`
	Email         string    `json:"email"`
	FirstName     string    `json:"first_name"`
	LastName      string    `json:"last_name"`
	Status        string    `json:"status"`
	Designation   string    `json:"designation,omitzero"`
	Department    string    `json:"department,omitzero"`
	StatusMessage string    `json:"status_message,omitzero"`
	JoinedAt      time.Time `json:"joined_at,omitzero"`
	LeftAt        time.Time `json:"left_at,omitzero"`
	ReportsTo     string    `json:"reports_to,omitzero"`
	RoleIDs       []string  `json:"role_ids"`
}

// ListUsersResponse — GET /api/v1/users. Keyset-paginated per ADR 0038.
// No total count (O(n) under RLS); the frontend drives "load more" off
// has_more.
type ListUsersResponse struct {
	Users      []UserDto `json:"users"`
	HasMore    bool      `json:"has_more"`
	NextCursor string    `json:"next_cursor,omitzero"`
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
// Atomic replace of both overlays; empty arrays clear them. Names must be
// in [permission.IdentityPermissions]; unknown → 422.
type ReplaceUserPermissionOverridesRequest struct {
	Granted []string `json:"granted"`
	Revoked []string `json:"revoked"`
}

// AssignUserManagerRequest — PUT /api/v1/users/{userId}/manager.
// ManagerID must be a same-tenant Membership (composite FK enforces it;
// cross-tenant → 422).
type AssignUserManagerRequest struct {
	ManagerID string `json:"manager_id"`
}

// CreateUserRequest — POST /api/v1/users. Find-or-create-by-email
// (Auth0/Entra): the server attaches to an existing global Person or
// creates one.
type CreateUserRequest struct {
	Email     string `json:"email"`
	Password  string `json:"password"`
	FirstName string `json:"first_name"`
	LastName  string `json:"last_name"`
}

// CreateUserResponse — POST /api/v1/users 201 body. PersonExisted tells
// the admin whether the email was attached to an existing global
// identity vs a fresh Person.
type CreateUserResponse struct {
	PersonID      string `json:"person_id"`
	MembershipID  string `json:"membership_id"`
	PersonExisted bool   `json:"person_existed"`
}

// ----- Platform: cross-tenant Person + tenant ops ----------------------------

// PersonDto is a Person for Platform read endpoints. Password hash +
// security stamp are NEVER exposed.
type PersonDto struct {
	ID                     string    `json:"id"`
	Email                  string    `json:"email"`
	FirstName              string    `json:"first_name"`
	LastName               string    `json:"last_name"`
	IsActive               bool      `json:"is_active"`
	IsAnonymised           bool      `json:"is_anonymised"`
	IsGloballySuspended    bool      `json:"is_globally_suspended"`
	GlobalSuspensionReason string    `json:"global_suspension_reason,omitzero"`
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
// Updates the GLOBAL Person profile, distinct from the per-Tenant fields
// in [UpdateUserProfileRequest].
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

// ListTenantsResponse — GET /api/v1/tenants?slug=acme. Canonical slug
// lookup per ADR 0052: 0-1 matches (Stripe/Auth0), not 404.
// Enumeration-safe (ADR 0044) — empty Tenants[] is "no such slug" OR
// "you can't see it", indistinguishable by design.
type ListTenantsResponse struct {
	Tenants []TenantDto `json:"tenants"`
}

// ----- Platform: Impersonation sessions --------------------------------------

// CreateImpersonationSessionRequest — POST /api/v1/platform/impersonation/sessions.
// Reason ≥10 chars; DurationMinutes optional (default 30, max 240).
type CreateImpersonationSessionRequest struct {
	TargetTenantID  string `json:"target_tenant_id"`
	Reason          string `json:"reason"`
	DurationMinutes int    `json:"duration_minutes,omitzero"`
}

// CreateImpersonationSessionResponse — 201 body per ADR 0045: scoped
// access token + expiry, used for the session lifetime. No refresh path
// (AWS STS AssumeRole canon); re-open to extend. Token aud is
// leadkart-impersonation; non-accepting routes reject it server-side.
type CreateImpersonationSessionResponse struct {
	SessionID               string    `json:"session_id"`
	ExpiresAtUTC            time.Time `json:"expires_at_utc"`
	AccessToken             string    `json:"access_token"`
	AccessTokenExpiresAtUTC time.Time `json:"access_token_expires_at_utc"`
	TokenType               string    `json:"token_type"` // always "Bearer"
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

// PlatformStatsResponse — GET /api/v1/platform/stats. Operator dashboard
// counts. Deltas is populated only on ?delta_window=24h|7d|30d (closed
// set per ADR 0040) and counts "new rows since now() - window" per base
// metric. Cached 5min keyed by delta_window.
type PlatformStatsResponse struct {
	TenantsTotal      int                  `json:"tenants_total"`
	TenantsActive     int                  `json:"tenants_active"`
	TenantsSuspended  int                  `json:"tenants_suspended"`
	PersonsTotal      int                  `json:"persons_total"`
	MembershipsActive int                  `json:"memberships_active"`
	Deltas            *PlatformStatsDeltas `json:"deltas,omitzero"`
}

// PlatformStatsDeltas carries "Δ in the last <window>". Same metric
// names as the base counts for uniform "<base> (+<delta>)" rendering.
type PlatformStatsDeltas struct {
	Window            string `json:"window"` // "24h" | "7d" | "30d"
	TenantsTotal      int    `json:"tenants_total"`
	TenantsActive     int    `json:"tenants_active"`
	PersonsTotal      int    `json:"persons_total"`
	MembershipsActive int    `json:"memberships_active"`
}

// ----- Role management -------------------------------------------------------

// RoleDto is a [role.Role] for read endpoints. ParentRoleID (ADR 0054)
// is empty for a root role; drives the hierarchy tree + inheritance
// preview.
type RoleDto struct {
	ID              string    `json:"id"`
	TenantID        string    `json:"tenant_id"`
	Name            string    `json:"name"`
	IsSystemDefault bool      `json:"is_system_default"`
	IsSuperAdmin    bool      `json:"is_super_admin"`
	HierarchyLevel  int       `json:"hierarchy_level"`
	Permissions     []string  `json:"permissions"`
	CreatedAt       time.Time `json:"created_at"`
	ParentRoleID    string    `json:"parent_role_id,omitzero"`
}

// ListRolesResponse — GET /api/v1/roles.
type ListRolesResponse struct {
	Roles []RoleDto `json:"roles"`
}

// CreateRoleRequest — POST /api/v1/roles. HierarchyLevel in
// [HierarchyLevelMin, HierarchyLevelMax]. IsSuperAdmin is absent on
// purpose (seed-only; see CreateRoleCommand). ParentRoleID (ADR 0054)
// optional — omit/empty/null = root; same-tenant only, DB enforces
// cross-tenant + cycle prevention.
type CreateRoleRequest struct {
	Name           string `json:"name"`
	HierarchyLevel int    `json:"hierarchy_level"`
	ParentRoleID   string `json:"parent_role_id,omitzero"`
}

// CreateRoleResponse — POST /api/v1/roles 201 body.
type CreateRoleResponse struct {
	RoleID string `json:"role_id"`
}

// UpdateRoleRequest — PATCH /api/v1/roles/{roleId}. Both optional: empty
// Name skips rename, nil HierarchyLevel skips re-level (pointer over -1
// sentinel).
type UpdateRoleRequest struct {
	Name           string `json:"name,omitzero"`
	HierarchyLevel *int   `json:"hierarchy_level,omitzero"`
}

// ReplaceRolePermissionsRequest — PUT /api/v1/roles/{roleId}/permissions.
type ReplaceRolePermissionsRequest struct {
	Permissions []string `json:"permissions"`
}

// RolePermissionRequest — POST /api/v1/roles/{roleId}/permissions/{grant,revoke}.
type RolePermissionRequest struct {
	Permission string `json:"permission"`
}

// SetRoleParentRequest — PATCH /api/v1/roles/{roleId}/parent (ADR 0058).
// Pointer-string so JSON null (or empty) clears the parent (soft-deletes
// the active edge → root). Non-empty must be a same-tenant role UUID; DB
// rejects cross-tenant + cycle. reason is optional audit text, 10-1024
// chars when set.
type SetRoleParentRequest struct {
	ParentRoleID *string `json:"parent_role_id"`
	Reason       string  `json:"reason,omitzero"`
}

// ----- Tenant management -----------------------------------------------------

// TenantDto is a Tenant for read endpoints — full profile, statutory,
// contact, settings, display prefs, lifecycle timestamps. Mirrors the
// .NET TenantDto.
type TenantDto struct {
	ID          string `json:"id"`
	Slug        string `json:"slug"`
	LegalName   string `json:"legal_name"`
	DisplayName string `json:"display_name"`
	// admin_email removed in migration 20260507000008 — now derived
	// (CompanyOwner membership → person.email). Use /v1/users instead.
	Status              string            `json:"status"`
	CreatedAt           time.Time         `json:"created_at"`
	ActivatedAt         time.Time         `json:"activated_at,omitzero"`
	SuspendedAt         time.Time         `json:"suspended_at,omitzero"`
	DeletionScheduledAt time.Time         `json:"deletion_scheduled_at,omitzero"`
	DeletionReason      string            `json:"deletion_reason,omitzero"`
	GSTNumber           string            `json:"gst_number,omitzero"`
	PANNumber           string            `json:"pan_number,omitzero"`
	DrugLicenceNumber   string            `json:"drug_licence_number,omitzero"`
	AdminPhone          string            `json:"admin_phone,omitzero"`
	AdminAddress        AdminAddressDto   `json:"admin_address"`
	PasswordPolicy      PasswordPolicyDto `json:"password_policy"`
	Locale              string            `json:"locale,omitzero"`
	TimeZone            string            `json:"time_zone,omitzero"`
	DateFormat          string            `json:"date_format,omitzero"`
	Currency            string            `json:"currency,omitzero"`
}

// AdminAddressDto is the postal-address slice of [TenantDto].
type AdminAddressDto struct {
	Street    string `json:"street,omitzero"`
	City      string `json:"city,omitzero"`
	District  string `json:"district,omitzero"`
	State     string `json:"state,omitzero"`
	StateCode string `json:"state_code,omitzero"`
	Pincode   string `json:"pincode,omitzero"`
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
// Empty strings clear a declaration; a zero [tenant.Statutory] is valid
// for not-yet-onboarded tenants.
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

// SuspendTenantRequest — POST .../suspend. Reason required (data-retention.md audit).
type SuspendTenantRequest struct {
	Reason string `json:"reason"`
}

// MarkTenantForDeletionRequest — POST .../mark-for-deletion. Reason
// required (DPDP §12 + SOC2 CC4.1 audit).
type MarkTenantForDeletionRequest struct {
	Reason string `json:"reason"`
}

// ----- Sessions --------------------------------------------------------------

// SessionDto is one GET /api/v1/auth/sessions entry: device label +
// created + last-refreshed (Auth0/Okta/GitHub shape). Tokens/hashes
// NEVER exposed.
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

// RevokeAllSessionsRequest — DELETE /api/v1/auth/sessions body. Optional.
// ExceptCurrent keeps the caller's session ("sign me out of OTHER
// devices"). Reason defaults to "user_revoked_all" server-side.
type RevokeAllSessionsRequest struct {
	ExceptCurrent bool   `json:"except_current,omitzero"`
	Reason        string `json:"reason,omitzero"`
}

// RevokeAllSessionsResponse — DELETE /api/v1/auth/sessions response.
type RevokeAllSessionsResponse struct {
	RevokedCount int `json:"revoked_count"`
}

// ----- Permission-elevation approval workflow (ADR 0055) --------------------

// CreatePermissionRequestRequest — POST /api/v1/permission-requests body.
// permission must be in [permission.IdentityPermissions] (unknown → 422);
// duration_days optional (0 = default 7-day window); reason ≥10 chars.
type CreatePermissionRequestRequest struct {
	Permission   string `json:"permission"`
	DurationDays int    `json:"duration_days,omitzero"`
	Reason       string `json:"reason"`
}

// CreatePermissionRequestResponse — POST /api/v1/permission-requests 201 body.
// ApproverMembershipID is the requester's current manager (empty when
// none — then only Platform operators can approve).
type CreatePermissionRequestResponse struct {
	RequestID            string `json:"request_id"`
	ApproverMembershipID string `json:"approver_membership_id,omitzero"`
	Status               string `json:"status"` // always "pending" on the 201 path
}

// ApprovePermissionRequestRequest — POST .../{id}/approve. decision_reason
// optional, max 1024 chars (DB CHECK).
type ApprovePermissionRequestRequest struct {
	DecisionReason string `json:"decision_reason,omitzero"`
}

// DenyPermissionRequestRequest — POST .../{id}/deny. decision_reason
// required (ADR 0055 audit).
type DenyPermissionRequestRequest struct {
	DecisionReason string `json:"decision_reason"`
}

// PermissionRequestDto is the read shape of a permission-elevation
// request. Wire-stable; ADR 0055 expands via new optional fields only.
type PermissionRequestDto struct {
	ID                    string    `json:"id"`
	TenantID              string    `json:"tenant_id"`
	RequesterMembershipID string    `json:"requester_membership_id"`
	Permission            string    `json:"permission"`
	DurationDays          int       `json:"duration_days"`
	Reason                string    `json:"reason"`
	State                 string    `json:"state"`
	ApproverMembershipID  string    `json:"approver_membership_id,omitzero"`
	DecidedAt             time.Time `json:"decided_at,omitzero"`
	DecisionReason        string    `json:"decision_reason,omitzero"`
	ExpiresAt             time.Time `json:"expires_at,omitzero"`
	CreatedAt             time.Time `json:"created_at"`
	UpdatedAt             time.Time `json:"updated_at"`
}

// ListPermissionRequestsResponse — paginated GET response. requests
// always non-nil; cursor semantics per ADR 0038.
type ListPermissionRequestsResponse struct {
	Requests   []PermissionRequestDto `json:"requests"`
	HasMore    bool                   `json:"has_more"`
	NextCursor string                 `json:"next_cursor,omitzero"`
}

// ----- Errors ----------------------------------------------------------------

// ErrorResponse is the shared 4xx/5xx body — RFC 9457 Problem Details
// plus LeadKart-legacy fields. The legacy error+message stay populated
// for clients branching on error; field-level errors go in the errors
// map (RFC 9457 §3.1 extension).
//
// Enumeration safety (ADR 0044): empty Message + nil Errors when the
// caller lacks access — the status code is the only signal, never
// existence info.
//
// Wire shape:
//
//	{
//	  "type":    "https://leadkart.api/errors/validation",   // RFC 9457 §3.1.1
//	  "title":   "Validation failed",                          // §3.1.2
//	  "status":  422,                                          // §3.1.3
//	  "detail":  "One or more fields are invalid",             // §3.1.4
//	  "error":   "validation_failed",                          // LeadKart legacy
//	  "message": "One or more fields are invalid",             // LeadKart legacy
//	  "errors": {                                              // RFC 9457 ext
//	    "email":    ["must be a valid email address"],
//	    "password": ["must be at least 12 characters", "must contain a digit"]
//	  }
//	}
type ErrorResponse struct {
	// RFC 9457 Problem Details fields.
	Type   string `json:"type,omitzero"`
	Title  string `json:"title,omitzero"`
	Status int    `json:"status,omitzero"`
	Detail string `json:"detail,omitzero"`

	// LeadKart legacy fields (kept for backward compat).
	Error   string `json:"error"`            // machine-parseable code
	Message string `json:"message,omitzero"` // human-readable

	// Field-level errors (RFC 9457 §3.1 extension; key = JSON field
	// name, value = list of error messages). Populated by validation
	// failures; nil for non-validation errors.
	Errors map[string][]string `json:"errors,omitzero"`
}
