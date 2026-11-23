package ports

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/companysettings/app"
	"github.com/leadkart/leadkart-go/internal/companysettings/app/command"
	"github.com/leadkart/leadkart-go/internal/companysettings/app/query"
	"github.com/leadkart/leadkart-go/internal/companysettings/domain/commercesettings"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// AddRoutes registers the company commerce settings HTTP handlers on mux. When
// verifier or stampValidator is nil the block stays unregistered (fixtures that
// only exercise unauthenticated surface pass nil).
//
// Gates: reading the policy, its history and the GST list rides
// company_settings.read (the rate list also opens to catalogue readers, who pick
// a rate when they create a product); PATCH /commerce-settings is open to either
// write permission and the aggregate enforces which one each changed key needs;
// the GST list changes under company_settings.tax.manage. Both write permissions
// are held only at the Administrator tier or above (owner ruling 2026-11-23).
func AddRoutes(mux *http.ServeMux, log *slog.Logger, a app.Application, verifier authn.Verifier, stampValidator authn.StampValidator) {
	if verifier == nil || stampValidator == nil {
		return
	}
	p := permission.IdentityPermissions
	settingsRead := authn.RequirePermission(verifier, stampValidator, p.CompanySettings.Read)
	settingsWrite := authn.RequireAnyPermission(verifier, stampValidator, p.CompanySettings.Manage, p.CompanySettings.TaxManage)
	ratesRead := authn.RequireAnyPermission(verifier, stampValidator,
		p.CompanySettings.Read, p.Inventory.Catalog.Read, p.Inventory.Catalog.Manage)
	taxManage := authn.RequirePermission(verifier, stampValidator, p.CompanySettings.TaxManage)

	mux.Handle("GET /api/v1/company/commerce-settings", settingsRead(handleGetSettings(log, a)))
	mux.Handle("PATCH /api/v1/company/commerce-settings", settingsWrite(handlePatchSettings(log, a)))
	mux.Handle("GET /api/v1/company/commerce-settings/history", settingsRead(handleListHistory(log, a)))
	mux.Handle("GET /api/v1/company/gst-rates", ratesRead(handleListGSTRates(log, a)))
	mux.Handle("POST /api/v1/company/gst-rates", taxManage(handleCreateGSTRate(log, a)))
	mux.Handle("PATCH /api/v1/company/gst-rates/{rateId}", taxManage(handlePatchGSTRate(log, a)))
}

// ----- Handlers --------------------------------------------------------------

func handleGetSettings(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effTenant, ok := authn.EffectiveTenantID(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		v, err := a.Queries.GetSettings.Handle(r.Context(), query.GetSettingsQuery{TenantID: tenant.ID(effTenant)})
		if err != nil {
			log.ErrorContext(r.Context(), "get company settings failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusOK, settingsViewToDto(v))
	})
}

func handlePatchSettings(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effTenant, ok := authn.EffectiveTenantID(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		op := ""
		if c.Act != nil {
			op = c.Act.Sub
		}
		var req PatchSettingsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		errs := commercepolicy.FieldErrors{}
		if req.ExpectedVersion == nil {
			errs["expected_version"] = []string{commercepolicy.CodeRequired}
		} else if *req.ExpectedVersion < 0 {
			errs["expected_version"] = []string{commercepolicy.CodeOutOfRange}
		}
		changes := flattenChanges(req.Changes)
		if len(changes) == 0 {
			errs["changes"] = []string{commercepolicy.CodeRequired}
		}
		if len(errs) > 0 {
			writeFieldErrors(w, http.StatusUnprocessableEntity, ErrCodeValidationFailed, errs)
			return
		}
		res, err := a.Commands.ChangeSettings.Handle(r.Context(), command.ChangeSettingsCommand{
			TenantID: tenant.ID(effTenant), ExpectedVersion: *req.ExpectedVersion, Changes: changes, Reason: req.Reason,
			ActorMembershipID: membership.ID(c.MembershipID), ImpersonatorOperatorID: op,
			MayManage:    authn.Satisfies(r.Context(), permission.IdentityPermissions.CompanySettings.Manage),
			MayManageTax: authn.Satisfies(r.Context(), permission.IdentityPermissions.CompanySettings.TaxManage),
		})
		if err != nil {
			writeCommandError(w, r, log, "patch company settings", err)
			return
		}
		// The response is the updated policy (ADR 0038): no refetch is needed.
		dto := SettingsDto{
			TenantID: effTenant, Version: res.Version, Policy: res.Policy,
			Options: commercepolicy.Describe(), UpdatedByMembershipID: res.UpdatedBy.String(),
		}
		if !res.UpdatedAt.IsZero() {
			dto.UpdatedAt = &res.UpdatedAt
		}
		writeJSON(w, http.StatusOK, dto)
	})
}

func handleListHistory(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effTenant, ok := authn.EffectiveTenantID(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		q := r.URL.Query()
		cursor, err := pagination.Decode(q.Get("cursor"))
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidQuery, "invalid cursor")
			return
		}
		page, err := a.Queries.ListHistory.Handle(r.Context(), query.ListHistoryQuery{
			TenantID: tenant.ID(effTenant), Cursor: cursor, PageSize: parsePageSize(q.Get("page_size")),
		})
		switch {
		case errors.Is(err, pagination.ErrInvalidCursor):
			writeError(w, http.StatusBadRequest, ErrCodeInvalidQuery, "invalid cursor")
			return
		case err != nil:
			log.ErrorContext(r.Context(), "list company settings history failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		items := make([]HistoryEntryDto, 0, len(page.Items))
		for _, e := range page.Items {
			items = append(items, HistoryEntryDto{
				Version: e.Version, Policy: e.Policy, ChangedKeys: e.ChangedKeys, Reason: e.Reason,
				ActorMembershipID: e.ActorMembershipID, ImpersonatorOperatorID: e.ImpersonatorOperatorID, ChangedAt: e.ChangedAt,
			})
		}
		writeJSON(w, http.StatusOK, ListHistoryResponse{Items: items, HasMore: page.HasMore, NextCursor: page.NextCursor})
	})
}

func handleListGSTRates(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effTenant, ok := authn.EffectiveTenantID(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		view, err := a.Queries.ListGSTRates.Handle(r.Context(), query.ListGSTRatesQuery{
			TenantID: tenant.ID(effTenant), ActiveOnly: r.URL.Query().Get("active") == "true",
		})
		if err != nil {
			log.ErrorContext(r.Context(), "list gst rates failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		items := make([]GSTRateDto, 0, len(view.Items))
		for _, rt := range view.Items {
			items = append(items, rateToDto(rt))
		}
		writeJSON(w, http.StatusOK, ListGSTRatesResponse{PolicyVersion: view.PolicyVersion, Items: items})
	})
}

func handleCreateGSTRate(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effTenant, ok := authn.EffectiveTenantID(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		op := ""
		if c.Act != nil {
			op = c.Act.Sub
		}
		var req CreateGSTRateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		if req.RateBps == nil {
			writeFieldErrors(w, http.StatusUnprocessableEntity, ErrCodeValidationFailed,
				map[string][]string{"rate_bps": {commercepolicy.CodeRequired}})
			return
		}
		out, err := a.Commands.AddGSTRate.Handle(r.Context(), command.AddGSTRateCommand{
			TenantID: tenant.ID(effTenant), RateBps: *req.RateBps, Label: req.Label, ConfirmedBy: req.ConfirmedBy,
			ConfirmedOn: req.ConfirmedOn, Reason: req.Reason,
			ActorMembershipID: membership.ID(c.MembershipID), ImpersonatorOperatorID: op,
		})
		if err != nil {
			writeCommandError(w, r, log, "create gst rate", err)
			return
		}
		writeJSON(w, http.StatusCreated, GSTRateChangedDto{GSTRateDto: rateToDto(out.Rate), PolicyVersion: out.PolicyVersion})
	})
}

func handlePatchGSTRate(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effTenant, ok := authn.EffectiveTenantID(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeUnauthenticated, "")
			return
		}
		op := ""
		if c.Act != nil {
			op = c.Act.Sub
		}
		rateID, ok := parsePathUUID(w, r, "rateId")
		if !ok {
			return
		}
		var req PatchGSTRateRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		out, err := a.Commands.UpdateGSTRate.Handle(r.Context(), command.UpdateGSTRateCommand{
			TenantID: tenant.ID(effTenant), RateID: rateID.String(), Label: req.Label, ConfirmedBy: req.ConfirmedBy,
			ConfirmedOn: req.ConfirmedOn, Active: req.Active, Reason: req.Reason,
			ActorMembershipID: membership.ID(c.MembershipID), ImpersonatorOperatorID: op,
		})
		if err != nil {
			writeCommandError(w, r, log, "patch gst rate", err)
			return
		}
		writeJSON(w, http.StatusOK, GSTRateChangedDto{GSTRateDto: rateToDto(out.Rate), PolicyVersion: out.PolicyVersion})
	})
}

// ----- Helpers ---------------------------------------------------------------

// writeCommandError maps a command error to the wire: 409 for a stale version,
// 403 for a setting the caller may not change, 404 for an unknown rate, 422 with
// errors{field:[code]} for a refused value, 500 otherwise.
func writeCommandError(w http.ResponseWriter, r *http.Request, log *slog.Logger, what string, err error) {
	var vc *commercesettings.VersionConflictError
	var needTax *commercesettings.TaxManageRequiredError
	var needManage *commercesettings.ManageRequiredError
	switch {
	case errors.As(err, &vc):
		cur := vc.Current
		writeJSON(w, http.StatusConflict, ErrorResponse{
			Type: problemType(ErrCodeSettingsVersionConflict), Title: http.StatusText(http.StatusConflict), Status: http.StatusConflict,
			Detail: "the settings changed since this form was loaded; reload and apply the change again",
			Error:  ErrCodeSettingsVersionConflict, Message: "the settings changed since this form was loaded; reload and apply the change again",
			CurrentVersion: &cur,
		})
	case errors.As(err, &needTax):
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "missing permission company_settings.tax.manage for: "+strings.Join(needTax.Keys, ", "))
	case errors.As(err, &needManage):
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "missing permission company_settings.manage for: "+strings.Join(needManage.Keys, ", "))
	case errors.Is(err, commercesettings.ErrGSTRateNotFound):
		writeError(w, http.StatusNotFound, ErrCodeNotFound, "gst rate not found")
	case commercepolicy.Fields(err) != nil:
		writeFieldErrors(w, http.StatusUnprocessableEntity, ErrCodeValidationFailed, commercepolicy.Fields(err))
	default:
		log.ErrorContext(r.Context(), what+" failed", "err", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
	}
}

// flattenChanges turns {"expiry": {"stop_months_before": 2}} into the registry
// keys {"expiry.stop_months_before": 2}.
func flattenChanges(in map[string]map[string]json.RawMessage) commercepolicy.Changes {
	out := commercepolicy.Changes{}
	for group, kv := range in {
		for name, raw := range kv {
			out[group+"."+name] = raw
		}
	}
	return out
}

func settingsViewToDto(v *query.SettingsView) SettingsDto {
	return SettingsDto{
		TenantID: v.TenantID, Version: v.Version, Policy: v.Policy, Options: v.Options,
		UpdatedAt: v.UpdatedAt, UpdatedByMembershipID: v.UpdatedBy,
	}
}

func rateToDto(r commercepolicy.GSTRate) GSTRateDto {
	return GSTRateDto{ID: r.ID, RateBps: r.RateBps, Label: r.Label, ConfirmedBy: r.ConfirmedBy, ConfirmedOn: r.ConfirmedOn, Active: r.Active}
}

// parsePathUUID extracts and validates a UUID path value; it writes 400 and
// returns false on bad input.
func parsePathUUID(w http.ResponseWriter, r *http.Request, name string) (uuid.UUID, bool) {
	raw := r.PathValue(name)
	canon, ok := authn.CanonicalID(raw)
	if ok {
		if id, err := uuid.Parse(canon); err == nil {
			return id, true
		}
	}
	writeError(w, http.StatusBadRequest, ErrCodeInvalidID, name+" path parameter must be a UUID")
	return uuid.Nil, false
}

// parsePageSize returns the supplied page_size, 0 when absent or unreadable (the
// query handler then clamps to the default).
func parsePageSize(raw string) int {
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0
	}
	return n
}

// writeJSON encodes body to w with the status code.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeError emits the Problem Details + legacy error/message shape.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, ErrorResponse{
		Type: problemType(code), Title: http.StatusText(status), Status: status,
		Detail: message, Error: code, Message: message,
	})
}

// msgFieldsInvalid is the fixed message of a refusal that names its fields.
const msgFieldsInvalid = "one or more fields are invalid; see errors"

// writeFieldErrors emits 4xx with errors{field:[code]}.
func writeFieldErrors(w http.ResponseWriter, status int, code string, errs map[string][]string) {
	writeJSON(w, status, ErrorResponse{
		Type: problemType(code), Title: http.StatusText(status), Status: status,
		Detail: msgFieldsInvalid, Error: code, Message: msgFieldsInvalid, Errors: errs,
	})
}

// problemType returns the RFC 9457 type URI for an error code.
func problemType(code string) string {
	if code == "" {
		return ""
	}
	return "https://leadkart.api/errors/" + code
}
