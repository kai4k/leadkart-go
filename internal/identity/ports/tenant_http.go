package ports

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ----- GetTenant ------------------------------------------------------------

func handleGetTenant(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseTenantIDPath(w, r)
		if !ok {
			return
		}
		view, err := a.Queries.GetTenant.Handle(r.Context(), query.GetTenantQuery{TenantID: id})
		switch {
		case errors.Is(err, tenant.ErrNotFound):
			writeError(w, http.StatusNotFound, ErrCodeTenantNotFound, "")
			return
		case err != nil:
			log.ErrorContext(r.Context(), "get tenant failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusOK, projectViewToDto(view))
	})
}

// ----- UpdateTenantProfile --------------------------------------------------

func handleUpdateTenantProfile(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseTenantIDPath(w, r)
		if !ok {
			return
		}
		var req UpdateTenantProfileRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		err := a.Commands.UpdateTenantProfile.Handle(r.Context(), command.UpdateTenantProfileCommand{
			TenantID:    id,
			LegalName:   req.LegalName,
			DisplayName: req.DisplayName,
		})
		writeTenantMutationResult(w, log, r, err)
	})
}

// ----- UpdateTenantStatutory ------------------------------------------------

func handleUpdateTenantStatutory(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseTenantIDPath(w, r)
		if !ok {
			return
		}
		var req UpdateTenantStatutoryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		err := a.Commands.UpdateTenantStatutory.Handle(r.Context(), command.UpdateTenantStatutoryCommand{
			TenantID:          id,
			GSTNumber:         req.GSTNumber,
			PANNumber:         req.PANNumber,
			DrugLicenceNumber: req.DrugLicenceNumber,
		})
		writeTenantMutationResult(w, log, r, err)
	})
}

// ----- UpdateTenantAdminContact ---------------------------------------------

func handleUpdateTenantAdminContact(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseTenantIDPath(w, r)
		if !ok {
			return
		}
		var req UpdateTenantAdminContactRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		err := a.Commands.UpdateTenantAdminContact.Handle(r.Context(), command.UpdateTenantAdminContactCommand{
			TenantID:         id,
			Phone:            req.Phone,
			AddressStreet:    req.Address.Street,
			AddressCity:      req.Address.City,
			AddressDistrict:  req.Address.District,
			AddressState:     req.Address.State,
			AddressStateCode: req.Address.StateCode,
			AddressPincode:   req.Address.Pincode,
		})
		writeTenantMutationResult(w, log, r, err)
	})
}

// ----- UpdateTenantSettings -------------------------------------------------

func handleUpdateTenantSettings(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseTenantIDPath(w, r)
		if !ok {
			return
		}
		var req UpdateTenantSettingsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		err := a.Commands.UpdateTenantSettings.Handle(r.Context(), command.UpdateTenantSettingsCommand{
			TenantID:          id,
			MinLength:         req.PasswordPolicy.MinLength,
			RequireUppercase:  req.PasswordPolicy.RequireUppercase,
			RequireLowercase:  req.PasswordPolicy.RequireLowercase,
			RequireDigit:      req.PasswordPolicy.RequireDigit,
			RequireSymbol:     req.PasswordPolicy.RequireSymbol,
			MaxFailedAttempts: req.PasswordPolicy.MaxFailedAttempts,
			LockoutMinutes:    req.PasswordPolicy.LockoutMinutes,
		})
		writeTenantMutationResult(w, log, r, err)
	})
}

// ----- UpdateTenantDisplayPreferences ---------------------------------------

func handleUpdateTenantDisplayPreferences(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseTenantIDPath(w, r)
		if !ok {
			return
		}
		var req UpdateTenantDisplayPreferencesRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		err := a.Commands.UpdateTenantDisplayPreferences.Handle(r.Context(), command.UpdateTenantDisplayPreferencesCommand{
			TenantID:   id,
			Locale:     req.Locale,
			TimeZone:   req.TimeZone,
			DateFormat: req.DateFormat,
			Currency:   req.Currency,
		})
		writeTenantMutationResult(w, log, r, err)
	})
}

// ----- SuspendTenant --------------------------------------------------------

func handleSuspendTenant(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseTenantIDPath(w, r)
		if !ok {
			return
		}
		var req SuspendTenantRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		err := a.Commands.SuspendTenant.Handle(r.Context(), command.SuspendTenantCommand{
			TenantID: id,
			Reason:   req.Reason,
		})
		writeTenantMutationResult(w, log, r, err)
	})
}

// ----- ActivateTenant -------------------------------------------------------

func handleActivateTenant(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseTenantIDPath(w, r)
		if !ok {
			return
		}
		err := a.Commands.ActivateTenant.Handle(r.Context(), command.ActivateTenantCommand{
			TenantID: id,
		})
		writeTenantMutationResult(w, log, r, err)
	})
}

// ----- MarkTenantForDeletion ------------------------------------------------

func handleMarkTenantForDeletion(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseTenantIDPath(w, r)
		if !ok {
			return
		}
		var req MarkTenantForDeletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		err := a.Commands.MarkTenantForDeletion.Handle(r.Context(), command.MarkTenantForDeletionCommand{
			TenantID: id,
			Reason:   req.Reason,
		})
		writeTenantMutationResult(w, log, r, err)
	})
}

// ----- RestoreTenant --------------------------------------------------------

func handleRestoreTenant(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseTenantIDPath(w, r)
		if !ok {
			return
		}
		err := a.Commands.RestoreTenant.Handle(r.Context(), command.RestoreTenantCommand{
			TenantID: id,
		})
		writeTenantMutationResult(w, log, r, err)
	})
}

// ----- Helpers --------------------------------------------------------------

// parseTenantIDPath extracts + validates the {tenantId} path
// parameter. Writes a 400 with [ErrCodeInvalidTenantID] on failure
// and returns ok=false; caller short-circuits.
func parseTenantIDPath(w http.ResponseWriter, r *http.Request) (tenant.ID, bool) {
	raw := r.PathValue("tenantId")
	if _, err := uuid.Parse(raw); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidTenantID,
			"tenantId path parameter must be a UUID")
		return "", false
	}
	return tenant.ID(raw), true
}

// writeTenantMutationResult collapses the small set of expected
// outcomes from a tenant-mutation command handler into HTTP responses.
//
//   - nil          → 204 No Content
//   - tenant.ErrNotFound / command.ErrTenantNotFound → 404
//   - tenant.ErrInvalid (wrapped from aggregate) → 422 (lifecycle
//     transition rejected, over-length names, missing reason, etc.)
//   - tenant.ErrSlugTaken → 409 (mostly relevant for register flow,
//     included here for completeness when slug-update endpoints land)
//   - anything else → 500 + slog.ErrorContext
func writeTenantMutationResult(w http.ResponseWriter, log *slog.Logger, r *http.Request, err error) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, tenant.ErrNotFound),
		errors.Is(err, command.ErrTenantNotFound):
		writeError(w, http.StatusNotFound, ErrCodeTenantNotFound, "")
	case errors.Is(err, tenant.ErrInvalid):
		writeError(w, http.StatusUnprocessableEntity, ErrCodeTenantInvalid, err.Error())
	default:
		log.ErrorContext(r.Context(), "tenant mutation failed", "err", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
	}
}

// projectViewToDto bridges the application-layer query view to the
// HTTP DTO. Done at the boundary so the application layer remains
// free of `json:` tags + time.Time formatting concerns.
func projectViewToDto(v query.TenantView) TenantDto {
	return TenantDto{
		ID:                  v.ID,
		Slug:                v.Slug,
		LegalName:           v.LegalName,
		DisplayName:         v.DisplayName,
		Status:              v.Status,
		CreatedAt:           v.CreatedAt,
		ActivatedAt:         v.ActivatedAt,
		SuspendedAt:         v.SuspendedAt,
		DeletionScheduledAt: v.DeletionScheduledAt,
		DeletionReason:      v.DeletionReason,
		GSTNumber:           v.GSTNumber,
		PANNumber:           v.PANNumber,
		DrugLicenceNumber:   v.DrugLicenceNumber,
		AdminPhone:          v.AdminPhone,
		AdminAddress: AdminAddressDto{
			Street:    v.AdminAddress.Street,
			City:      v.AdminAddress.City,
			District:  v.AdminAddress.District,
			State:     v.AdminAddress.State,
			StateCode: v.AdminAddress.StateCode,
			Pincode:   v.AdminAddress.Pincode,
		},
		PasswordPolicy: PasswordPolicyDto{
			MinLength:         v.PasswordPolicy.MinLength,
			RequireUppercase:  v.PasswordPolicy.RequireUppercase,
			RequireLowercase:  v.PasswordPolicy.RequireLowercase,
			RequireDigit:      v.PasswordPolicy.RequireDigit,
			RequireSymbol:     v.PasswordPolicy.RequireSymbol,
			MaxFailedAttempts: v.PasswordPolicy.MaxFailedAttempts,
			LockoutMinutes:    v.PasswordPolicy.LockoutMinutes,
		},
		Locale:     v.Locale,
		TimeZone:   v.TimeZone,
		DateFormat: v.DateFormat,
		Currency:   v.Currency,
	}
}
