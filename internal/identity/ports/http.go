package ports

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// AddRoutes registers Identity HTTP handlers on mux. Mat Ryer 2024 canon:
// ports own request/response translation, not the routing scheme — the
// composition root chooses the URL space.
//
// verifier + stampValidator gate authenticated routes:
//   - verifier is the [authn.Verifier] used to validate the bearer JWT
//     (production wires *jwt.Issuer).
//   - stampValidator is the [authn.StampValidator] used to assert that
//     the JWT's security_stamp claim still matches the source-of-truth
//     stamp on the Person — i.e. the session has not been revoked
//     since the token was minted (per audit-checklist.md §12b cache
//     facade canon + security.md SecurityStamp rotation triggers).
//
// Both MUST be non-nil for the auth-route block to register; pass
// (nil, nil) ONLY in test fixtures that exercise the unauthenticated
// surface only (probe routes / login / refresh / logout / emailed-token
// flows).
//
// Routes registered here:
//
//	POST /api/v1/tenants                   register a new tenant + admin user
//	POST /api/v1/auth/login                exchange credentials for ⟨access, refresh⟩
//	POST /api/v1/auth/refresh              rotate refresh token + reissue access
//	POST /api/v1/auth/logout               revoke a refresh-token family
//	POST /api/v1/auth/change-password      authenticated; rotate own password
func AddRoutes(mux *http.ServeMux, log *slog.Logger, a app.Application, verifier authn.Verifier, stampValidator authn.StampValidator) {
	mux.Handle("POST /api/v1/tenants", handleRegisterTenant(log, a))
	mux.Handle("POST /api/v1/auth/login", handleLogin(log, a))
	mux.Handle("POST /api/v1/auth/refresh", handleRefresh(log, a))
	mux.Handle("POST /api/v1/auth/logout", handleLogout(log, a))

	// Anonymous endpoints (no JWT required) — emailed-token flows.
	mux.Handle("POST /api/v1/auth/request-password-reset", handleRequestPasswordReset(log, a))
	mux.Handle("POST /api/v1/auth/reset-password", handleResetPassword(log, a))
	mux.Handle("POST /api/v1/auth/confirm-email-change", handleConfirmEmailChange(log, a))

	if verifier != nil && stampValidator != nil {
		// Every authenticated route runs RequireFreshStamp — JWT signature
		// + expiry + tenant_id binding + security_stamp freshness check.
		// A token whose underlying Person has rotated its stamp (via
		// password/email change, anonymisation, global suspend) fails
		// 401 stale_token within ~30s of the rotation, even before the
		// outbox-driven cascade subscriber has invalidated the cache.
		auth := authn.RequireFreshStamp(verifier, stampValidator)
		mux.Handle("POST /api/v1/auth/change-password", auth(handleChangePassword(log, a)))
		mux.Handle("POST /api/v1/auth/request-email-change", auth(handleRequestEmailChange(log, a)))
		mux.Handle("GET /api/v1/auth/sessions", auth(handleListSessions(log, a)))
		// /me/capabilities — frontend nav/tier/button-visibility driver.
		// JWT-resident fields only (sub-millisecond, no DB hit). Per
		// ADR 0036 + Phase 1.5 — the JWT carries the resolved permission
		// bundle; this endpoint is a thin projection so the frontend
		// stops base64-decoding tokens to drive UI logic.
		mux.Handle("GET /api/v1/auth/me/capabilities", auth(handleGetCapabilities(log, a)))
		mux.Handle("DELETE /api/v1/auth/sessions/{familyId}", auth(handleRevokeSession(log, a)))
		mux.Handle("DELETE /api/v1/auth/sessions", auth(handleRevokeAllSessions(log, a)))

		// Tenant management — same-tenant-or-platform gate per
		// authn.RequireTenantContext. A tenant Admin can manage their
		// own tenant; Platform / SuperUser operators can manage any
		// (post-impersonation per multi-tenancy.md).
		// Canonical slug lookup — Stripe/Auth0 listing-with-filter shape
		// per ADR 0052 (Wave 9.1c). Returns {tenants: [0|1 match]}.
		// Enumeration-safe per ADR 0044 (no-access = empty list).
		mux.Handle("GET /api/v1/tenants",
			auth(handleListTenantsByFilter(log, a)))

		// Slug lookup (deprecated — superseded by GET /tenants?slug=).
		// Kept for v0.2 frontend-contract compatibility per ADR 0049
		// grandfather rule. Remove in v0.4+ once frontend migrates.
		// Enumeration-safe 404 inline authz per ADR 0044.
		mux.Handle("GET /api/v1/tenants/by-slug/{slug}",
			auth(handleGetTenantBySlug(log, a)))

		tenantCtx := authn.RequireTenantContext(verifier, stampValidator, "tenantId")
		mux.Handle("GET /api/v1/tenants/{tenantId}",
			tenantCtx(handleGetTenant(log, a)))
		mux.Handle("PATCH /api/v1/tenants/{tenantId}/profile",
			tenantCtx(handleUpdateTenantProfile(log, a)))
		mux.Handle("PATCH /api/v1/tenants/{tenantId}/statutory",
			tenantCtx(handleUpdateTenantStatutory(log, a)))
		mux.Handle("PATCH /api/v1/tenants/{tenantId}/admin-contact",
			tenantCtx(handleUpdateTenantAdminContact(log, a)))
		mux.Handle("PATCH /api/v1/tenants/{tenantId}/settings",
			tenantCtx(handleUpdateTenantSettings(log, a)))
		mux.Handle("PATCH /api/v1/tenants/{tenantId}/display-preferences",
			tenantCtx(handleUpdateTenantDisplayPreferences(log, a)))

		// Lifecycle — Platform-only per multi-tenancy.md "SuperUser
		// god-mode" + identity.tenants.{suspend,activate,delete}
		// permissions. Tenants do NOT self-suspend / self-restore via
		// these routes; those flows go through Platform-tier APIs.
		platform := authn.RequirePlatform(verifier, stampValidator)
		mux.Handle("POST /api/v1/tenants/{tenantId}/suspend",
			platform(handleSuspendTenant(log, a)))
		mux.Handle("POST /api/v1/tenants/{tenantId}/activate",
			platform(handleActivateTenant(log, a)))
		mux.Handle("POST /api/v1/tenants/{tenantId}/mark-for-deletion",
			platform(handleMarkTenantForDeletion(log, a)))
		mux.Handle("POST /api/v1/tenants/{tenantId}/restore",
			platform(handleRestoreTenant(log, a)))

		// User (Membership) management — all routes operate within the
		// caller's tenant scope, enforced by the JWT-bridge middleware
		// + RLS GUC. Cross-tenant access surfaces as 404 per
		// security.md enumeration-safety rule.
		mux.Handle("GET /api/v1/users", auth(handleListUsers(log, a)))
		mux.Handle("GET /api/v1/users/{userId}", auth(handleGetUser(log, a)))
		mux.Handle("PATCH /api/v1/users/{userId}/profile",
			auth(handleUpdateUserProfile(log, a)))
		mux.Handle("POST /api/v1/users/{userId}/deactivate",
			auth(handleDeactivateUser(log, a)))
		mux.Handle("POST /api/v1/users/{userId}/reactivate",
			auth(handleReactivateUser(log, a)))
		mux.Handle("POST /api/v1/users/{userId}/roles",
			auth(handleAssignUserRole(log, a)))
		mux.Handle("DELETE /api/v1/users/{userId}/roles/{roleId}",
			auth(handleRevokeUserRole(log, a)))
		mux.Handle("PATCH /api/v1/users/{userId}/permission-overrides",
			auth(handleReplaceUserPermissionOverrides(log, a)))
		mux.Handle("PUT /api/v1/users/{userId}/manager",
			auth(handleAssignUserManager(log, a)))
		mux.Handle("DELETE /api/v1/users/{userId}/manager",
			auth(handleRemoveUserManager(log, a)))
		mux.Handle("POST /api/v1/users", auth(handleCreateUser(log, a)))

		// Anonymise — DPDP §12 / GDPR Art. 17 right-to-erasure cascade.
		// Cross-tenant blast radius (one Person ⇒ many Memberships)
		// gates this on Platform tier per multi-tenancy.md
		// "SuperUser god-mode" + identity.users.anonymise permission.
		mux.Handle("POST /api/v1/users/{userId}/anonymise",
			platform(handleAnonymiseUser(log, a)))

		// Role management — tenant-RLS scoped via JWT bridge.
		mux.Handle("GET /api/v1/roles", auth(handleListRoles(log, a)))
		mux.Handle("GET /api/v1/roles/{roleId}", auth(handleGetRole(log, a)))
		mux.Handle("POST /api/v1/roles", auth(handleCreateRole(log, a)))
		mux.Handle("PATCH /api/v1/roles/{roleId}", auth(handleUpdateRole(log, a)))
		mux.Handle("PUT /api/v1/roles/{roleId}/permissions",
			auth(handleReplaceRolePermissions(log, a)))
		mux.Handle("POST /api/v1/roles/{roleId}/permissions/grant",
			auth(handleGrantRolePermission(log, a)))
		mux.Handle("POST /api/v1/roles/{roleId}/permissions/revoke",
			auth(handleRevokeRolePermission(log, a)))
		// ADR 0054 — hierarchy sub-resource. Null/empty parent_role_id
		// in the body clears the parent (role becomes a root).
		mux.Handle("PATCH /api/v1/roles/{roleId}/parent",
			auth(handleSetRoleParent(log, a)))
		mux.Handle("DELETE /api/v1/roles/{roleId}", auth(handleDeleteRole(log, a)))

		// Platform admin — all under /api/v1/platform/... gated on
		// RequirePlatform per multi-tenancy.md "Platform admin endpoints"
		// (rate-limited to 600/min/operator). Cross-tenant blast
		// radius lives here; per-request audit trail captured by the
		// existing platform middleware (impersonation flow lands in
		// A.7.b — these endpoints don't require an active session).
		mux.Handle("GET /api/v1/platform/tenants",
			platform(handleListAllTenants(log, a)))
		mux.Handle("DELETE /api/v1/platform/tenants/{tenantId}",
			platform(handleHardDeleteTenant(log, a)))
		mux.Handle("GET /api/v1/platform/persons/{personId}",
			platform(handleGetPerson(log, a)))
		// Platform-only cross-tenant identity probe by email. Query-
		// param keyed (NOT path) — emails contain `@` + `.` which
		// confuse some URL parsers + access-log greps. Stripe/Auth0/
		// GitHub all do ?email= here for the same reason.
		mux.Handle("GET /api/v1/platform/persons",
			platform(handleGetPersonByEmail(log, a)))
		mux.Handle("GET /api/v1/platform/persons/{personId}/memberships",
			platform(handleListPersonMemberships(log, a)))
		mux.Handle("PATCH /api/v1/platform/persons/{personId}/profile",
			platform(handleUpdatePersonProfile(log, a)))
		mux.Handle("POST /api/v1/platform/persons/{personId}/global-suspend",
			platform(handleGlobalSuspendPerson(log, a)))
		mux.Handle("POST /api/v1/platform/persons/{personId}/lift-global-suspension",
			platform(handleLiftPersonGlobalSuspension(log, a)))
		mux.Handle("POST /api/v1/platform/persons/{personId}/anonymise",
			platform(handleAnonymisePerson(log, a)))

		// Impersonation sessions per multi-tenancy.md "Impersonation".
		// Reason captured ONCE at session creation; subsequent
		// per-request use carries X-Impersonation-Session-Id (the
		// resolution middleware lands in a follow-up — for v0.2 these
		// endpoints manage the session lifecycle but the per-request
		// header pickup is post-launch operational work).
		mux.Handle("POST /api/v1/platform/impersonation/sessions",
			platform(handleCreateImpersonationSession(log, a)))
		mux.Handle("DELETE /api/v1/platform/impersonation/sessions/{sessionId}",
			platform(handleEndImpersonationSession(log, a)))
		mux.Handle("GET /api/v1/platform/impersonation/sessions",
			platform(handleListImpersonationSessions(log, a)))

		// Operator dashboard at-a-glance stats.
		mux.Handle("GET /api/v1/platform/stats",
			platform(handlePlatformStats(log, a)))

		// Operator omni-search (Cmd+K) — parallel pg_trgm fanout
		// across persons + tenants per ADR 0040. Platform-only at
		// v0.2; tenant-scoped variant lands in a follow-up.
		mux.Handle("GET /api/v1/search",
			platform(handleSearch(log, a)))

		// Audit-log reads per ADR 0027 (outbox doubles as audit) +
		// ADR 0038 (keyset pagination). Self-read is always allowed
		// (privacy default); tenant-scoped read goes through the
		// same RequireTenantContext gate as the rest of /tenants/{id}.
		mux.Handle("GET /api/v1/auth/me/activity",
			auth(handleListMyAuditEvents(log, a)))
		// Sub-resource path (`/audit/events`) deliberately ≠ the by-slug
		// lookup's shape (`/by-slug/{slug}`) — both 2-segment tails would
		// trigger a Go 1.22 ServeMux pattern conflict (literal+wildcard
		// vs wildcard+literal in the same positions = ambiguous). Splitting
		// into `/audit/events` adds the literal "audit" segment at position
		// 5, making this route 6 segments deep — structurally distinct
		// from /by-slug/{slug}.
		mux.Handle("GET /api/v1/tenants/{tenantId}/audit/events",
			tenantCtx(handleListTenantAuditEvents(log, a)))
	}
}

// ----- Handlers --------------------------------------------------------------

func handleRegisterTenant(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req RegisterTenantRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		s, err := slug.New(req.Slug)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidSlug, err.Error())
			return
		}
		addr, err := email.New(req.AdminEmail)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidEmail, err.Error())
			return
		}

		out, err := a.Commands.RegisterTenant.Handle(r.Context(), command.RegisterTenantCommand{
			Slug:           s,
			LegalName:      req.LegalName,
			DisplayName:    req.DisplayName,
			AdminEmail:     addr,
			AdminPassword:  req.AdminPassword,
			AdminFirstName: req.AdminFirstName,
			AdminLastName:  req.AdminLastName,
		})
		switch {
		case errors.Is(err, command.ErrEmailHasActiveMembership):
			writeError(w, http.StatusConflict, ErrCodeEmailHasActiveMembership,
				"this email already has an active membership in another tenant")
			return
		case err != nil:
			log.ErrorContext(r.Context(), "register tenant failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}

		writeJSON(w, http.StatusCreated, RegisterTenantResponse{
			TenantID:     out.TenantID.String(),
			PersonID:     out.PersonID.String(),
			MembershipID: out.MembershipID.String(),
		})
	})
}

// resolveDeviceLabel derives a humane device label for the
// refresh-token family. Fallback chain when the client doesn't supply
// `device_label`:
//
//  1. trimmed `device_label` from the request body
//  2. trimmed User-Agent header (truncated to 128 chars)
//  3. RemoteAddr
//  4. literal "Unknown device"
//
// Matches Auth0 / Stripe / GitHub UX where the API derives the label so
// every client doesn't have to compute its own. The domain still
// requires a non-empty label per refreshtoken.NewFamily — the boundary
// is responsible for guaranteeing that invariant before the call.
const deviceLabelMaxLen = 128

func resolveDeviceLabel(supplied string, r *http.Request) string {
	if v := strings.TrimSpace(supplied); v != "" {
		return truncateLabel(v)
	}
	if ua := strings.TrimSpace(r.UserAgent()); ua != "" {
		return truncateLabel(ua)
	}
	if r.RemoteAddr != "" {
		return r.RemoteAddr
	}
	return "Unknown device"
}

func truncateLabel(s string) string {
	if len(s) <= deviceLabelMaxLen {
		return s
	}
	return s[:deviceLabelMaxLen]
}

func handleLogin(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req LoginRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		addr, err := email.New(req.Email)
		if err != nil {
			// Even malformed-email failures return the generic
			// invalid_credentials shape so an attacker can't probe
			// "what does this server consider a valid email?".
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}

		out, err := a.Commands.Login.Handle(r.Context(), command.LoginCommand{
			Email:       addr,
			Password:    req.Password,
			DeviceLabel: resolveDeviceLabel(req.DeviceLabel, r),
		})
		switch {
		case errors.Is(err, command.ErrAccountLocked):
			// 423 Locked per RFC 4918 §11.3 + ADR 0053. Retry-After
			// header carries integer seconds until unlock per RFC 7231
			// §7.1.3 (the "delta-seconds" form, not the HTTP-date form
			// — easier for SPAs to consume).
			writeLockoutError(w, command.LockedUntilFromError(err))
			return
		case errors.Is(err, command.ErrInvalidCredentials):
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		case err != nil:
			log.ErrorContext(r.Context(), "login failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}

		writeJSON(w, http.StatusOK, LoginResponse{
			AccessToken:          out.AccessToken,
			RefreshToken:         out.RefreshTokenPlain,
			AccessTokenExpiresAt: out.AccessTokenExpiresAt,
			TokenType:            "Bearer",
			MustChangePassword:   out.MustChangePassword,
		})
	})
}

// writeLockoutError emits the 423 Locked surface for Login's lockout
// branch per ADR 0053. Retry-After carries the seconds-until-unlock
// per RFC 7231 §7.1.3 delta-seconds form. lockedUntil may be zero
// (defensive — when LockedUntilFromError can't extract it); we still
// emit 423 + a default Retry-After equal to person.LockoutDuration to
// avoid leaking handler state.
func writeLockoutError(w http.ResponseWriter, lockedUntil time.Time) {
	retrySeconds := int(time.Until(lockedUntil).Seconds())
	if retrySeconds <= 0 {
		// Defensive — locked-until is in the past or zero. Use a
		// reasonable default so the client backs off instead of
		// retrying immediately + producing the same 423.
		retrySeconds = int((15 * time.Minute).Seconds())
	}
	w.Header().Set("Retry-After", strconv.Itoa(retrySeconds))
	writeError(w, http.StatusLocked, ErrCodeAccountLocked, "")
}

func handleRefresh(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req RefreshRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}

		out, err := a.Commands.Refresh.Handle(r.Context(), command.RefreshCommand{
			RefreshTokenPlain: req.RefreshToken,
		})
		switch {
		case errors.Is(err, command.ErrRefreshRejected):
			writeError(w, http.StatusUnauthorized, ErrCodeRefreshRejected, "")
			return
		case err != nil:
			log.ErrorContext(r.Context(), "refresh failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}

		writeJSON(w, http.StatusOK, LoginResponse{
			AccessToken:          out.AccessToken,
			RefreshToken:         out.RefreshTokenPlain,
			AccessTokenExpiresAt: out.AccessTokenExpiresAt,
			TokenType:            "Bearer",
		})
	})
}

func handleLogout(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req LogoutRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		if err := a.Commands.Logout.Handle(r.Context(), command.LogoutCommand{
			RefreshTokenPlain: req.RefreshToken,
			Reason:            req.Reason,
		}); err != nil {
			log.ErrorContext(r.Context(), "logout failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func handleChangePassword(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// PersonID comes from the verified JWT (Subject claim, set at
		// issuance per `security.md` "Access token: sub = PersonId").
		// RequireAuth has already populated ctx; missing claims here
		// is a wiring bug.
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok || strings.TrimSpace(c.Subject) == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}

		var req ChangePasswordRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody,
				"request body is not valid JSON")
			return
		}

		err := a.Commands.ChangePassword.Handle(r.Context(), command.ChangePasswordCommand{
			PersonID:        person.ID(c.Subject),
			CurrentPassword: req.CurrentPassword,
			NewPassword:     req.NewPassword,
		})
		switch {
		case errors.Is(err, command.ErrIncorrectCurrentPassword):
			// 401 — same status as login's invalid_credentials so an
			// attacker holding a stolen access token can't distinguish
			// "wrong current password" from "user disabled" via timing
			// or response code.
			writeError(w, http.StatusUnauthorized, ErrCodeIncorrectCurrentPassword, "")
			return
		case errors.Is(err, command.ErrPasswordBreached):
			writeError(w, http.StatusUnprocessableEntity, ErrCodePasswordBreached,
				"this password appears in known breach databases; choose another")
			return
		case errors.Is(err, command.ErrPasswordSameAsCurrent):
			writeError(w, http.StatusUnprocessableEntity, ErrCodePasswordSameAsCurrent,
				"new password must differ from current password")
			return
		case err != nil && (strings.Contains(err.Error(), "new password required") ||
			strings.Contains(err.Error(), "person id required")):
			// Domain-layer shape rejections — 400. PersonID-required
			// hitting here would be a wiring bug (RequireAuth + Subject
			// guard above should have short-circuited), but treat it as
			// invalid_body to avoid leaking server-state.
			writeError(w, http.StatusBadRequest, ErrCodeInvalidPassword, err.Error())
			return
		case err != nil:
			log.ErrorContext(r.Context(), "change password failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}

		w.WriteHeader(http.StatusNoContent)
	})
}

func handleListSessions(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok || strings.TrimSpace(c.Subject) == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}
		views, err := a.Queries.ListSessions.Handle(r.Context(), query.ListSessionsQuery{
			PersonID: person.ID(c.Subject),
		})
		if err != nil {
			log.ErrorContext(r.Context(), "list sessions failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		out := ListSessionsResponse{Sessions: make([]SessionDto, 0, len(views))}
		for _, v := range views {
			out.Sessions = append(out.Sessions, SessionDto{
				FamilyID:    v.FamilyID,
				TenantID:    v.TenantID,
				DeviceLabel: v.DeviceLabel,
				CreatedAt:   v.CreatedAt,
				LastUsedAt:  v.LastUsedAt,
			})
		}
		writeJSON(w, http.StatusOK, out)
	})
}

func handleRevokeSession(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok || strings.TrimSpace(c.Subject) == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}
		raw := r.PathValue("familyId")
		if _, err := uuid.Parse(raw); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidFamilyID,
				"familyId path parameter must be a UUID")
			return
		}
		err := a.Commands.RevokeSession.Handle(r.Context(), command.RevokeSessionCommand{
			PersonID: person.ID(c.Subject),
			FamilyID: refreshtoken.FamilyID(raw),
		})
		switch {
		case errors.Is(err, command.ErrSessionNotFound):
			// 404 — collapses "wrong owner" + "doesn't exist" + "already
			// revoked" per security.md enumeration-safety rule.
			writeError(w, http.StatusNotFound, ErrCodeSessionNotFound, "")
			return
		case err != nil:
			log.ErrorContext(r.Context(), "revoke session failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func handleRevokeAllSessions(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok || strings.TrimSpace(c.Subject) == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}

		// Body is OPTIONAL — empty body == default behaviour (revoke
		// every active family). Decoding an empty body via stdlib's
		// json.Decoder returns io.EOF; treat that as "no overrides".
		var req RevokeAllSessionsRequest
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&req) // ignore EOF
		}

		var except refreshtoken.FamilyID
		if req.ExceptCurrent {
			// "ExceptCurrent" requires a way to know WHICH family is
			// the current one. The JWT does not carry the FamilyID
			// (refresh-token families are session infra; access tokens
			// don't reveal them). Until the v0.3+ "current device"
			// header lands, ExceptCurrent is an explicit opt-in that
			// has no effect — same total-revoke result as omitting
			// the field. Documented in OpenAPI.
			//
			// TODO(v0.3): add `X-Refresh-Family-Id` header carry-over
			// or thread the family-id through access-token claims if
			// product wants per-device "sign me out of others".
			_ = except
		}

		out, err := a.Commands.RevokeAllSessions.Handle(r.Context(), command.RevokeAllSessionsCommand{
			PersonID:       person.ID(c.Subject),
			ExceptFamilyID: except,
			Reason:         req.Reason,
		})
		if err != nil {
			log.ErrorContext(r.Context(), "revoke all sessions failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusOK, RevokeAllSessionsResponse{
			RevokedCount: out.RevokedCount,
		})
	})
}

func handleRequestPasswordReset(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req RequestPasswordResetRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		// Per Auth0/Okta canon: the EXISTENCE of the email is not
		// disclosed. Even malformed-email failures collapse into the
		// same 204 response so the wire shape never reveals "we
		// rejected your input" vs "no such account".
		addr, err := email.New(req.Email)
		if err != nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if err := a.Commands.RequestPasswordReset.Handle(r.Context(), command.RequestPasswordResetCommand{
			Email: addr,
		}); err != nil {
			log.ErrorContext(r.Context(), "request password reset failed", "err", err)
			// Even on internal error, return 204 to avoid leaking
			// presence info via differentiated status codes. The
			// server-side log carries the diagnostic.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func handleResetPassword(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ResetPasswordRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		err := a.Commands.ConfirmPasswordReset.Handle(r.Context(), command.ConfirmPasswordResetCommand{
			RawToken:    req.Token,
			NewPassword: req.NewPassword,
		})
		switch {
		case errors.Is(err, command.ErrResetTokenInvalid):
			writeError(w, http.StatusBadRequest, ErrCodeResetTokenInvalid, "")
			return
		case errors.Is(err, command.ErrPasswordBreached):
			writeError(w, http.StatusUnprocessableEntity, ErrCodePasswordBreached,
				"this password appears in known breach databases; choose another")
			return
		case errors.Is(err, command.ErrPasswordSameAsCurrent):
			writeError(w, http.StatusUnprocessableEntity, ErrCodePasswordSameAsCurrent,
				"new password must differ from current password")
			return
		case err != nil && strings.Contains(err.Error(), "new password required"):
			writeError(w, http.StatusBadRequest, ErrCodeInvalidPassword, err.Error())
			return
		case err != nil:
			log.ErrorContext(r.Context(), "reset password failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func handleRequestEmailChange(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok || strings.TrimSpace(c.Subject) == "" {
			writeError(w, http.StatusUnauthorized, ErrCodeInvalidCredentials, "")
			return
		}
		var req RequestEmailChangeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		newAddr, err := email.New(req.NewEmail)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidEmail, err.Error())
			return
		}
		err = a.Commands.RequestEmailChange.Handle(r.Context(), command.RequestEmailChangeCommand{
			PersonID: person.ID(c.Subject),
			NewEmail: newAddr,
		})
		switch {
		case errors.Is(err, command.ErrEmailAlreadyTaken):
			// Anti-enumeration: collapse to 204 (same shape as
			// successful submission). A 409 here would let any
			// authenticated user probe the email space (try every
			// candidate, observe 204 vs 409). Auth0 / Okta /
			// Microsoft Entra ID canon: change-email flows MUST NOT
			// disclose target-email registration status. The
			// confirmation step (POST /confirm-email-change) still
			// fails on the unique-index constraint at apply time —
			// the user just learns later, not during enumeration.
			// The originating request is silently dropped + audit
			// logged for SIEM forensics.
			log.WarnContext(r.Context(), "email-change request collapsed to 204 (target already taken)",
				"person_id", c.Subject)
			w.WriteHeader(http.StatusNoContent)
			return
		case errors.Is(err, command.ErrEmailChangeRejected):
			// Same anti-enumeration policy: a request rejected
			// (e.g. trying to set the same email already on the
			// account) collapses to 204. Domain-shape errors are
			// not user-facing here.
			log.WarnContext(r.Context(), "email-change request collapsed to 204 (rejected)",
				"person_id", c.Subject)
			w.WriteHeader(http.StatusNoContent)
			return
		case err != nil:
			log.ErrorContext(r.Context(), "request email change failed", "err", err)
			// Even on internal errors, return 204 to avoid
			// revealing failure-shape differentially. Server-side
			// log carries the diagnostic.
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func handleConfirmEmailChange(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ConfirmEmailChangeRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		err := a.Commands.ConfirmEmailChange.Handle(r.Context(), command.ConfirmEmailChangeCommand{
			RawToken: req.Token,
		})
		switch {
		case errors.Is(err, command.ErrEmailChangeTokenInvalid):
			writeError(w, http.StatusBadRequest, ErrCodeEmailChangeTokenInvalid, "")
			return
		case err != nil:
			log.ErrorContext(r.Context(), "confirm email change failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

// ----- Helpers ---------------------------------------------------------------

// writeJSON encodes body to w with status code. Sets Content-Type +
// best-effort-encodes; errors at this point are unrecoverable (partial
// body already on the wire), so we log nothing.
func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError emits the canonical error shape. message is optional —
// many error paths return an empty string to avoid leaking details
// (e.g. invalid_credentials NEVER says "wrong password" vs "no such
// user"; ADR 0044 enumeration safety).
//
// The wire shape is RFC 9457 Problem Details + LeadKart legacy
// fields. Title + Type are auto-derived from the code for HTTP-level
// consistency; clients on the new shape can branch on `type`/`status`/
// `errors`; clients on the legacy shape continue to branch on `error`.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorResponse{
		Type:    problemType(code),
		Title:   http.StatusText(status),
		Status:  status,
		Detail:  message,
		Error:   code,
		Message: message,
	})
}

// writeValidationError emits a 422 Unprocessable Entity with field-
// level errors per RFC 9457 §3.1 extension fields.
//
// fields maps wire-shape JSON field names (snake_case) to a list of
// validation messages. Multiple messages per field are common
// (password rules: "too short" + "no digit" + "no symbol").
//
// Use this from handlers that perform multi-field validation
// (RegisterTenant, ChangePassword, UpdateTenantStatutory, CreateUser).
// Single-field validation failures (e.g. invalid slug, invalid email)
// continue to use writeError — there's no field-level structure to
// surface.
//
// detail is the top-level human-readable summary ("One or more fields
// are invalid"); per-field detail goes in the fields map.
//
//nolint:unused // RFC 9457 infrastructure; incremental per-handler adoption in follow-up PRs
func writeValidationError(w http.ResponseWriter, detail string, fields map[string][]string) {
	writeJSON(w, http.StatusUnprocessableEntity, ErrorResponse{
		Type:    problemType(ErrCodeValidationFailed),
		Title:   http.StatusText(http.StatusUnprocessableEntity),
		Status:  http.StatusUnprocessableEntity,
		Detail:  detail,
		Error:   ErrCodeValidationFailed,
		Message: detail,
		Errors:  fields,
	})
}

// problemType returns the canonical Problem Details `type` URI for an
// error code per RFC 9457 §3.1.1. Convention: opaque slug under the
// LeadKart API base. Clients should treat the URI as an opaque
// identifier; not a deref-able link in v0.2.
func problemType(code string) string {
	if code == "" {
		return ""
	}
	return "https://leadkart.api/errors/" + code
}

// writeMutationResult is the canonical "mutation succeeded" response
// emitter. When body is nil → 204 No Content (the current default).
// When body is non-nil → 200 OK + body — matches Stripe / GitHub /
// Auth0 canon where mutations return the updated resource so the
// frontend doesn't need a follow-up GET.
//
// Migration plan (incremental per ADR 0046 — to land in a follow-up
// PR): each PATCH/POST mutation handler is upgraded to load the
// post-state + pass it here. Existing handlers calling the old
// shape (writeJSON with status 204) keep working — this is purely
// additive.
//
// Example future adoption:
//
//	// After the command succeeds:
//	updated, err := a.Queries.GetTenant.Handle(ctx, query.GetTenantQuery{TenantID: id})
//	if err != nil { ... }
//	writeMutationResult(w, projectViewToDto(updated))
//
// The 200+body shape is the canonical mutation response for SPAs that
// do optimistic updates and need server-canonical post-state.
//
//nolint:unused // A.8 infrastructure; incremental per-handler adoption in follow-up PRs
func writeMutationResult(w http.ResponseWriter, body interface{}) {
	if body == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, body)
}
