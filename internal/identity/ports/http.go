package ports

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

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
// verifier is the [authn.Verifier] used by [authn.RequireAuth] to gate
// authenticated routes (currently change-password); pass nil ONLY if
// the caller wires NO authenticated routes (test fixtures may opt out).
// Production wiring always provides the [jwt.Issuer] which satisfies
// [authn.Verifier] via its Verify method.
//
// Routes registered here:
//
//	POST /api/v1/tenants                   register a new tenant + admin user
//	POST /api/v1/auth/login                exchange credentials for ⟨access, refresh⟩
//	POST /api/v1/auth/refresh              rotate refresh token + reissue access
//	POST /api/v1/auth/logout               revoke a refresh-token family
//	POST /api/v1/auth/change-password      authenticated; rotate own password
func AddRoutes(mux *http.ServeMux, log *slog.Logger, a app.Application, verifier authn.Verifier) {
	mux.Handle("POST /api/v1/tenants", handleRegisterTenant(log, a))
	mux.Handle("POST /api/v1/auth/login", handleLogin(log, a))
	mux.Handle("POST /api/v1/auth/refresh", handleRefresh(log, a))
	mux.Handle("POST /api/v1/auth/logout", handleLogout(log, a))

	// Anonymous endpoints (no JWT required) — emailed-token flows.
	mux.Handle("POST /api/v1/auth/request-password-reset", handleRequestPasswordReset(log, a))
	mux.Handle("POST /api/v1/auth/reset-password", handleResetPassword(log, a))
	mux.Handle("POST /api/v1/auth/confirm-email-change", handleConfirmEmailChange(log, a))

	if verifier != nil {
		auth := authn.RequireAuth(verifier)
		mux.Handle("POST /api/v1/auth/change-password", auth(handleChangePassword(log, a)))
		mux.Handle("POST /api/v1/auth/request-email-change", auth(handleRequestEmailChange(log, a)))
		mux.Handle("GET /api/v1/auth/sessions", auth(handleListSessions(log, a)))
		mux.Handle("DELETE /api/v1/auth/sessions/{familyId}", auth(handleRevokeSession(log, a)))
		mux.Handle("DELETE /api/v1/auth/sessions", auth(handleRevokeAllSessions(log, a)))

		// Tenant management — same-tenant-or-platform gate per
		// authn.RequireTenantContext. A tenant Admin can manage their
		// own tenant; Platform / SuperUser operators can manage any
		// (post-impersonation per multi-tenancy.md).
		tenantCtx := authn.RequireTenantContext(verifier, "tenantId")
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
		platform := authn.RequirePlatform(verifier)
		mux.Handle("POST /api/v1/tenants/{tenantId}/suspend",
			platform(handleSuspendTenant(log, a)))
		mux.Handle("POST /api/v1/tenants/{tenantId}/activate",
			platform(handleActivateTenant(log, a)))
		mux.Handle("POST /api/v1/tenants/{tenantId}/mark-for-deletion",
			platform(handleMarkTenantForDeletion(log, a)))
		mux.Handle("POST /api/v1/tenants/{tenantId}/restore",
			platform(handleRestoreTenant(log, a)))
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
			DeviceLabel: req.DeviceLabel,
		})
		switch {
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
		})
	})
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
			writeError(w, http.StatusConflict, ErrCodeEmailAlreadyTaken,
				"another account already uses this email")
			return
		case errors.Is(err, command.ErrEmailChangeRejected):
			writeError(w, http.StatusBadRequest, ErrCodeEmailChangeRejected, "")
			return
		case err != nil:
			log.ErrorContext(r.Context(), "request email change failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
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
// user").
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorResponse{Error: code, Message: message})
}
