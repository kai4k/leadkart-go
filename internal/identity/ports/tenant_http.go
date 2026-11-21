package ports

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/leadkart/leadkart-go/internal/common/druglicence"
	"github.com/leadkart/leadkart-go/internal/common/gst"
	"github.com/leadkart/leadkart-go/internal/common/pan"
	"github.com/leadkart/leadkart-go/internal/common/phone"
	"github.com/leadkart/leadkart-go/internal/common/postaladdress"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// ----- ListTenants (query-filter shape; canonical slug lookup) --------------

// handleListTenantsByFilter serves GET /api/v1/tenants?slug=acme.
//
// Per ADR 0049 + ADR 0052 (Wave 9.1c): canonical replacement for the
// grandfathered GET /api/v1/tenants/by-slug/{slug} path-segment
// lookup. Stripe / Auth0 / GitHub canon: "find by alternate key" uses
// a query param on the listing endpoint and returns a list of 0-1
// matches (NOT a 404). The frontend checks `result.tenants.length`.
//
// Enumeration safety preserved: callers who lack access to the
// matched tenant get `{tenants: []}` — indistinguishable from
// "no tenant has this slug." Per ADR 0044.
//
// Status code matrix:
//
//	| Caller             | Slug exists, theirs   | Slug exists, others'    | Slug missing | Invalid slug |
//	|--------------------|-----------------------|-------------------------|--------------|--------------|
//	| Tenant admin       | 200 + {tenants: [T]} | 200 + {tenants: []}     | 200 + {tenants: []} | 400 |
//	| Platform operator  | 200 + {tenants: [T]} | 200 + {tenants: [T]}    | 200 + {tenants: []} | 400 |
//	| Unauthenticated    | 401 (middleware)     |                         |              |              |
//
// Future: support other filters (?status=, ?created_after=) per
// Stripe listing-endpoint canon. v0.2 ships slug only.
func handleListTenantsByFilter(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slugParam := r.URL.Query().Get("slug")
		if slugParam == "" {
			// v0.2: slug is the only supported filter; no slug = no
			// query intent. 400 (caller didn't say what they wanted).
			// When other filters land, change this to an empty list +
			// pagination.
			writeError(w, http.StatusBadRequest, ErrCodeInvalidQuery, "slug query parameter required")
			return
		}
		s, err := slug.New(slugParam)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidSlug, err.Error())
			return
		}

		view, err := a.Queries.GetTenantBySlug.Handle(r.Context(), query.GetTenantBySlugQuery{Slug: s})
		switch {
		case errors.Is(err, tenant.ErrNotFound):
			// No match → empty list (Stripe canon; NOT 404).
			writeJSON(w, http.StatusOK, ListTenantsResponse{Tenants: []TenantDto{}})
			return
		case err != nil:
			log.ErrorContext(r.Context(), "list tenants by slug failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}

		// Inline authz — same as handleGetTenantBySlug; enumeration-safe.
		_, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			log.ErrorContext(r.Context(), "list tenants: missing claims in authenticated handler")
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		if !authn.SatisfiesInTenant(r.Context(), tenancy.ID(view.ID), permission.IdentityPermissions.Tenants.View) {
			// No access → empty list (NOT 200-with-result + NOT 404).
			writeJSON(w, http.StatusOK, ListTenantsResponse{Tenants: []TenantDto{}})
			return
		}

		writeJSON(w, http.StatusOK, ListTenantsResponse{Tenants: []TenantDto{projectViewToDto(view)}})
	})
}

// ----- GetTenantBySlug (deprecated; superseded by ?slug= filter) -----------

// handleGetTenantBySlug serves GET /api/v1/tenants/by-slug/{slug}.
//
// The endpoint resolves a human-readable slug to a tenant. Because
// slugs are guessable (low-entropy, often the company name), the
// authz model MUST be enumeration-safe: callers who lack access to
// the resolved tenant see 404, NOT 403 — indistinguishable from
// "slug doesn't exist" (security.md enumeration-safety canon; ADR 0044).
//
// The mounting middleware is `auth` (RequireFreshStamp), NOT
// `RequireTenantPermission` — the tenant identity isn't in the path as
// a UUID; it's a slug that needs DB resolution before the authz
// comparison. The handler does the gate inline:
//
//	caller can see this tenant IFF:
//	   caller.JWT.is_platform=true (+ slug-anchored)  OR
//	   caller.JWT.tenant_id == resolved tenant.ID
//	otherwise → 404
//
// Vs the existing GET /api/v1/tenants/{tenantId} route which uses
// RequireTenantPermission middleware and returns 403 on mismatch.
// 403-vs-404 is acceptable for UUID paths (UUIDs aren't guessable, so
// enumeration is infeasible); slugs need 404 for the same security
// property. This is the standard GitHub / Stripe / Auth0 pattern for
// natural-key resource lookups.
//
// Status code matrix:
//
//	| Caller             | Slug exists, theirs | Slug exists, others' | Slug missing | Invalid slug |
//	|--------------------|---------------------|----------------------|--------------|--------------|
//	| Tenant admin       | 200 + TenantDto     | 404 (tenant_not_found) | 404         | 400 (invalid_slug) |
//	| Platform operator  | 200 + TenantDto     | 200 + TenantDto      | 404         | 400 |
//	| Unauthenticated    | 401 (caught by middleware before this handler) |   |   |   |
func handleGetTenantBySlug(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, err := slug.New(r.PathValue("slug"))
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidSlug, err.Error())
			return
		}

		view, err := a.Queries.GetTenantBySlug.Handle(r.Context(), query.GetTenantBySlugQuery{Slug: s})
		switch {
		case errors.Is(err, tenant.ErrNotFound):
			// Real 404 — slug doesn't resolve to any tenant.
			writeError(w, http.StatusNotFound, ErrCodeTenantNotFound, "")
			return
		case err != nil:
			log.ErrorContext(r.Context(), "get tenant by slug failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}

		// Inline authz gate per ADR 0044 enumeration safety.
		// RequireFreshStamp populated claims; missing-claims here is a
		// wiring bug (defensive 500).
		_, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			log.ErrorContext(r.Context(), "get tenant by slug: missing claims in authenticated handler")
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}

		// One decision in the RESOLVED tenant: identity.tenants.view in the
		// caller's own tenant, or the platform.tenants.view equivalent for a
		// slug-anchored operator (authn.SatisfiesInTenant).
		if !authn.SatisfiesInTenant(r.Context(), tenancy.ID(view.ID), permission.IdentityPermissions.Tenants.View) {
			// Tenant exists, caller can't see it. Same 404 surface as
			// "slug doesn't exist" — enumeration-safe per ADR 0044.
			// NOTE: NOT 403 — that would confirm "this slug is real,
			// you just can't access it" to the attacker.
			writeError(w, http.StatusNotFound, ErrCodeTenantNotFound, "")
			return
		}

		writeJSON(w, http.StatusOK, projectViewToDto(view))
	})
}

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
		if !decodeFullBody(w, r, &req) {
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
		if !decodeFullBody(w, r, &req) {
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
		if !decodeFullBody(w, r, &req) {
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
		if !decodeFullBody(w, r, &req) {
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
		if !decodeFullBody(w, r, &req) {
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
	id, ok := authn.CanonicalID(r.PathValue("tenantId"))
	if !ok {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidTenantID,
			"tenantId path parameter must be a UUID")
		return "", false
	}
	return tenant.ID(id), true
}

// writeTenantMutationResult collapses the small set of expected
// outcomes from a tenant-mutation command handler into HTTP responses.
//
//   - nil          → 204 No Content
//   - tenant.ErrNotFound / command.ErrTenantNotFound → 404
//   - tenant.ErrHasDependents → 409 tenant_has_dependents (hard delete only; the
//     transaction was rolled back, nothing changed)
//   - tenant.ErrInvalid (wrapped from aggregate) → 422 (lifecycle
//     transition rejected, over-length names, missing reason, etc.)
//   - gst / pan / druglicence / phone / postaladdress ErrInvalid → 422
//     validation_failed with a per-field `errors` entry (gst_number,
//     pan_number, drug_licence_number, phone, address)
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
	case errors.Is(err, command.ErrPlatformTenantUndeletable):
		writeError(w, http.StatusUnprocessableEntity, ErrCodePlatformTenantUndeletable,
			"this tenant holds an active SuperAdmin role and cannot be deleted via the standard lifecycle API")
	case errors.Is(err, tenant.ErrHasDependents):
		writeError(w, http.StatusConflict, ErrCodeTenantHasDependents,
			"the tenant still has dependent records (members, roles, ...) and cannot be deleted; nothing was changed")
	case errors.Is(err, tenant.ErrInvalid):
		writeInvalid(w, ErrCodeTenantInvalid, err)
	case errors.Is(err, gst.ErrInvalid):
		writeFieldInvalid(w, "gst_number", "must be a valid 15-character GSTIN")
	case errors.Is(err, pan.ErrInvalid):
		writeFieldInvalid(w, "pan_number", "must be a valid 10-character PAN")
	case errors.Is(err, druglicence.ErrInvalid):
		writeFieldInvalid(w, "drug_licence_number", "is not a valid drug licence number")
	case errors.Is(err, phone.ErrInvalid):
		writeFieldInvalid(w, "phone", "must be an E.164 phone number")
	case errors.Is(err, postaladdress.ErrInvalid):
		writeFieldInvalid(w, "address", "is not a valid postal address")
	default:
		log.ErrorContext(r.Context(), "tenant mutation failed", "err", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
	}
}

// writeFieldInvalid answers 422 validation_failed with one per-field entry,
// through the shared validation-error writer.
func writeFieldInvalid(w http.ResponseWriter, field, problem string) {
	writeValidationError(w, field+" "+problem, map[string][]string{field: {problem}})
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
