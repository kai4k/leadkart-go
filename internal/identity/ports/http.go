package ports

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
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
	if verifier != nil {
		mux.Handle("POST /api/v1/auth/change-password",
			authn.RequireAuth(verifier)(handleChangePassword(log, a)))
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
