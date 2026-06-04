package ports

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/crm/app"
	"github.com/leadkart/leadkart-go/internal/crm/app/command"
	"github.com/leadkart/leadkart-go/internal/crm/app/query"
	"github.com/leadkart/leadkart-go/internal/crm/domain/calllog"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// tenantFromContext extracts the caller's tenant ID from ctx (bound by
// authn middleware). Returns (zero, false) when missing — handlers
// short-circuit to 401 in that case. Per ADR 0062 the explicit value
// flows downstream to commands + queries; ctx-tenancy is only the
// HTTP-boundary extraction point.
func tenantFromContext(r *http.Request) (tenant.ID, bool) {
	tid, ok := tenancy.FromContext(r.Context())
	if !ok || tid == "" {
		return tenant.ID(""), false
	}
	return tenant.ID(tid), true
}

// pincodeRE mirrors the CHECK constraint on crm.crm_leads.pincode
// (Indian PIN code = 6 digits). Per reviewer M15, the HTTP handler
// MUST validate caller-supplied ?pincode= input rather than letting
// arbitrary strings reach the SQL exact-match filter (where they'd
// silently return zero rows + waste a query).
var pincodeRE = regexp.MustCompile(`^[1-9][0-9]{5}$`)

// AddRoutes registers CRM HTTP handlers on mux per Mat Ryer 2024 canon.
//
// verifier + stampValidator gate every CRM route — CRM is always
// authenticated (no anonymous lead reads). Pass (nil, nil) ONLY in test
// fixtures that exercise the route table without a real JWT verifier;
// the auth-route block then skips registration entirely.
//
// Routes registered here (all under /api/v1/crm/...):
//
//	GET    /api/v1/crm/leads                       cursor-paginated list
//	GET    /api/v1/crm/leads/{leadId}              single lead read
//	POST   /api/v1/crm/leads/{leadId}/assign       manual assignment
//	POST   /api/v1/crm/leads/{leadId}/stage        change stage
//	POST   /api/v1/crm/leads/{leadId}/temperature  change temperature
//	POST   /api/v1/crm/leads/{leadId}/calls        log a call
//	POST   /api/v1/crm/leads/{leadId}/convert      terminal-convert
//	POST   /api/v1/crm/leads/{leadId}/lose         terminal-lose
//
// Per-handler permission gates (ADR 0036 closed-set catalog):
//
//	read:        crm.leads.read
//	assign:      crm.leads.assign
//	stage/temp/call/convert/lose: crm.leads.manage
//
// "Only my assigned leads" filter is enforced INLINE in the handlers —
// callers WITHOUT crm.leads.read_all see a SelfFilter-narrowed list,
// callers WITH it see the full set. Per ADR 0060 + Auth0 "read:any"
// canon.
func AddRoutes(mux *http.ServeMux, log *slog.Logger, a app.Application, verifier authn.Verifier, stampValidator authn.StampValidator) {
	if verifier == nil || stampValidator == nil {
		// Test-fixture entry — caller only wants route-conflict
		// detection. Skipping the registrations is intentional.
		return
	}
	read := authn.RequirePermission(verifier, stampValidator, permission.IdentityPermissions.Crm.Leads.Read)
	manage := authn.RequirePermission(verifier, stampValidator, permission.IdentityPermissions.Crm.Leads.Manage)
	assign := authn.RequirePermission(verifier, stampValidator, permission.IdentityPermissions.Crm.Leads.Assign)

	mux.Handle("GET /api/v1/crm/leads", read(handleListLeads(log, a)))
	mux.Handle("GET /api/v1/crm/leads/{leadId}", read(handleGetLead(log, a)))
	mux.Handle("POST /api/v1/crm/leads/{leadId}/assign", assign(handleAssignLead(log, a)))
	mux.Handle("POST /api/v1/crm/leads/{leadId}/stage", manage(handleChangeStage(log, a)))
	mux.Handle("POST /api/v1/crm/leads/{leadId}/temperature", manage(handleChangeTemperature(log, a)))
	mux.Handle("POST /api/v1/crm/leads/{leadId}/calls", manage(handleLogCall(log, a)))
	mux.Handle("POST /api/v1/crm/leads/{leadId}/convert", manage(handleConvertLead(log, a)))
	mux.Handle("POST /api/v1/crm/leads/{leadId}/lose", manage(handleLoseLead(log, a)))

	// Reminder surface — auto-created reminders (callback / mature_lead)
	// are minted off the bus / scheduler; manual creation + lifecycle
	// mutations come through here per BRD §4.6.
	mux.Handle("POST /api/v1/crm/leads/{leadId}/reminders", manage(handleCreateReminder(log, a)))
	mux.Handle("GET /api/v1/crm/reminders", read(handleListReminders(log, a)))
	mux.Handle("POST /api/v1/crm/reminders/{reminderId}/sent", manage(handleMarkReminderSent(log, a)))
	mux.Handle("POST /api/v1/crm/reminders/{reminderId}/cancel", manage(handleCancelReminder(log, a)))
}

// ----- Handlers --------------------------------------------------------------

func handleListLeads(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		tid, ok := tenantFromContext(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		params := r.URL.Query()
		cursor, err := pagination.Decode(params.Get("cursor"))
		if err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidCursor, err.Error())
			return
		}
		pageSize := 0
		if raw := strings.TrimSpace(params.Get("page_size")); raw != "" {
			if n, perr := strconv.Atoi(raw); perr == nil {
				pageSize = n
			}
		}
		filter, err := parseListFilter(params)
		if err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidStage, err.Error())
			return
		}

		// "Only my leads" gate: callers without crm.leads.read_all see
		// a SelfFilter-narrowed view. Platform / SuperUser short-circuit.
		selfFilter := ""
		if !c.IsSuperUser && !c.IsPlatform &&
			!slices.Contains(c.Permissions, permission.IdentityPermissions.Crm.Leads.ReadAll) {
			selfFilter = c.MembershipID
		}

		// Per reviewer H8: when SelfFilter is active, a caller-supplied
		// ?assignee=X that points at a DIFFERENT membership is a
		// privilege probe — they're trying to filter on someone else's
		// caseload despite lacking ReadAll. The SQL AND-combines both
		// filters (intersection), so historically these returned an
		// empty page silently. 403 with a clear code surfaces intent.
		// Same-membership ?assignee=self is harmless + the SQL collapses
		// to a single predicate.
		if selfFilter != "" && filter.AssigneeMembershipID != "" && filter.AssigneeMembershipID != selfFilter {
			writeError(w, http.StatusForbidden, errCodeForbidden,
				"caller lacks crm.leads.read_all; ?assignee must equal the caller's own membership")
			return
		}

		page, err := a.Queries.ListLeads.Handle(r.Context(), query.ListLeadsQuery{
			TenantID: tid, Cursor: cursor, PageSize: pageSize, Filter: filter, SelfFilter: selfFilter,
		})
		if err != nil {
			log.ErrorContext(r.Context(), "crm: list leads", "err", err)
			writeError(w, http.StatusInternalServerError, errCodeInternalError, "")
			return
		}
		out := ListLeadsResponse{
			Items:      make([]LeadDto, 0, len(page.Items)),
			HasMore:    page.HasMore,
			NextCursor: page.NextCursor,
		}
		for _, v := range page.Items {
			out.Items = append(out.Items, leadViewToDto(v))
		}
		writeJSON(w, http.StatusOK, out)
	})
}

func handleGetLead(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := tenantFromContext(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		id, ok := parseLeadID(w, r)
		if !ok {
			return
		}
		v, err := a.Queries.GetLead.Handle(r.Context(), query.GetLeadQuery{TenantID: tid, LeadID: id})
		switch {
		case errors.Is(err, query.ErrLeadNotFound):
			writeError(w, http.StatusNotFound, errCodeLeadNotFound, "")
			return
		case err != nil:
			log.ErrorContext(r.Context(), "crm: get lead", "err", err)
			writeError(w, http.StatusInternalServerError, errCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusOK, leadViewToDto(v))
	})
}

func handleAssignLead(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := tenantFromContext(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		id, ok := parseLeadID(w, r)
		if !ok {
			return
		}
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		var req AssignLeadRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidBody, "request body is not valid JSON")
			return
		}
		if _, err := uuid.Parse(req.AssigneeMembershipID); err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidMembershipID, "assignee_membership_id must be a UUID")
			return
		}
		out, err := a.Commands.AssignLead.Handle(r.Context(), command.AssignLeadCommand{
			TenantID: tid, LeadID: id, AssigneeMembershipID: req.AssigneeMembershipID,
			AssignedByMembershipID: c.MembershipID, Reason: req.Reason,
		})
		switch {
		case errors.Is(err, command.ErrLeadNotFound):
			writeError(w, http.StatusNotFound, errCodeLeadNotFound, "")
			return
		case errors.Is(err, command.ErrLeadTerminal):
			writeError(w, http.StatusConflict, errCodeLeadTerminal, "")
			return
		case err != nil:
			log.ErrorContext(r.Context(), "crm: assign lead", "err", err)
			writeError(w, http.StatusInternalServerError, errCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusOK, AssignLeadResponse{
			AssignmentID:         out.AssignmentID.String(),
			AssigneeMembershipID: req.AssigneeMembershipID,
		})
	})
}

func handleChangeStage(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := tenantFromContext(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		id, ok := parseLeadID(w, r)
		if !ok {
			return
		}
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		var req ChangeStageRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidBody, "request body is not valid JSON")
			return
		}
		stage, err := crmlead.ParseStage(req.NewStage)
		if err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidStage, err.Error())
			return
		}
		err = a.Commands.ChangeLeadStage.Handle(r.Context(), command.ChangeLeadStageCommand{
			TenantID: tid, LeadID: id, NewStage: stage, ChangedByMembershipID: c.MembershipID, Reason: req.Reason,
		})
		mapMutationErr(w, log, r, err, "change stage")
	})
}

func handleChangeTemperature(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := tenantFromContext(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		id, ok := parseLeadID(w, r)
		if !ok {
			return
		}
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		var req ChangeTemperatureRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidBody, "request body is not valid JSON")
			return
		}
		temp, err := crmlead.ParseTemperature(req.NewTemperature)
		if err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidTemperature, err.Error())
			return
		}
		err = a.Commands.ChangeLeadTemperature.Handle(r.Context(), command.ChangeLeadTemperatureCommand{
			TenantID: tid, LeadID: id, NewTemperature: temp, ChangedByMembershipID: c.MembershipID,
		})
		mapMutationErr(w, log, r, err, "change temperature")
	})
}

func handleLogCall(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := tenantFromContext(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		id, ok := parseLeadID(w, r)
		if !ok {
			return
		}
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		var req LogCallRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidBody, "request body is not valid JSON")
			return
		}
		outcome, err := calllog.ParseOutcome(req.Outcome)
		if err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidOutcome, err.Error())
			return
		}
		out, err := a.Commands.LogCall.Handle(r.Context(), command.LogCallCommand{
			TenantID: tid, LeadID: id, Outcome: outcome, Notes: req.Notes, LoggedByMembershipID: c.MembershipID,
		})
		switch {
		case errors.Is(err, command.ErrLeadNotFound):
			writeError(w, http.StatusNotFound, errCodeLeadNotFound, "")
			return
		case errors.Is(err, command.ErrLeadTerminal):
			writeError(w, http.StatusConflict, errCodeLeadTerminal, "")
			return
		case err != nil:
			log.ErrorContext(r.Context(), "crm: log call", "err", err)
			writeError(w, http.StatusInternalServerError, errCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusCreated, LogCallResponse{CallID: out.CallID.String()})
	})
}

func handleConvertLead(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := tenantFromContext(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		id, ok := parseLeadID(w, r)
		if !ok {
			return
		}
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		err := a.Commands.ConvertLead.Handle(r.Context(), command.ConvertLeadCommand{
			TenantID: tid, LeadID: id, ConvertedByMembershipID: c.MembershipID,
		})
		mapMutationErr(w, log, r, err, "convert lead")
	})
}

func handleLoseLead(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := tenantFromContext(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		id, ok := parseLeadID(w, r)
		if !ok {
			return
		}
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		var req LoseLeadRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidBody, "request body is not valid JSON")
			return
		}
		if strings.TrimSpace(req.Reason) == "" {
			writeError(w, http.StatusUnprocessableEntity, errCodeReasonRequired, "reason is required for lose")
			return
		}
		err := a.Commands.LoseLead.Handle(r.Context(), command.LoseLeadCommand{
			TenantID: tid, LeadID: id, LostByMembershipID: c.MembershipID, Reason: req.Reason,
		})
		mapMutationErr(w, log, r, err, "lose lead")
	})
}

// ----- Helpers --------------------------------------------------------------

func parseLeadID(w http.ResponseWriter, r *http.Request) (crmlead.ID, bool) {
	raw := r.PathValue("leadId")
	if _, err := uuid.Parse(raw); err != nil {
		writeError(w, http.StatusBadRequest, errCodeInvalidLeadID, "leadId path parameter must be a UUID")
		return "", false
	}
	return crmlead.ID(raw), true
}

func parseListFilter(params url.Values) (crmlead.ListFilter, error) {
	pincode := strings.TrimSpace(params.Get("pincode"))
	if pincode != "" && !pincodeRE.MatchString(pincode) {
		return crmlead.ListFilter{}, errors.New("pincode must be a 6-digit Indian PIN code")
	}
	f := crmlead.ListFilter{
		City:           strings.TrimSpace(params.Get("city")),
		Pincode:        pincode,
		BusinessType:   strings.TrimSpace(params.Get("business_type")),
		MedicineSystem: strings.TrimSpace(params.Get("medicine_system")),
		NameQuery:      strings.TrimSpace(params.Get("name")),
	}
	if s := strings.TrimSpace(params.Get("stage")); s != "" {
		stage, err := crmlead.ParseStage(s)
		if err != nil {
			return crmlead.ListFilter{}, err
		}
		f.Stage = stage
	}
	if s := strings.TrimSpace(params.Get("temperature")); s != "" {
		temp, err := crmlead.ParseTemperature(s)
		if err != nil {
			return crmlead.ListFilter{}, err
		}
		f.Temperature = temp
	}
	if s := strings.TrimSpace(params.Get("assignee")); s != "" {
		if _, err := uuid.Parse(s); err != nil {
			return crmlead.ListFilter{}, errors.New("assignee must be a UUID")
		}
		f.AssigneeMembershipID = s
	}
	if pr := params["product_range"]; len(pr) > 0 {
		f.ProductRanges = filterNonEmpty(pr)
	}
	if dr := params["dosage_form"]; len(dr) > 0 {
		f.DosageForms = filterNonEmpty(dr)
	}
	return f, nil
}

func filterNonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if s := strings.TrimSpace(v); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func mapMutationErr(w http.ResponseWriter, log *slog.Logger, r *http.Request, err error, op string) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, command.ErrLeadNotFound):
		writeError(w, http.StatusNotFound, errCodeLeadNotFound, "")
	case errors.Is(err, command.ErrLeadTerminal):
		writeError(w, http.StatusConflict, errCodeLeadTerminal, "")
	case errors.Is(err, crmlead.ErrInvalid):
		writeError(w, http.StatusUnprocessableEntity, errCodeInvalidStage, err.Error())
	case errors.Is(err, crmlead.ErrTerminal):
		writeError(w, http.StatusConflict, errCodeLeadTerminal, "")
	default:
		log.ErrorContext(r.Context(), "crm: "+op, "err", err)
		writeError(w, http.StatusInternalServerError, errCodeInternalError, "")
	}
}

// leadViewToDto maps the app-layer read View to the wire DTO (1:1).
// Per STRICT CQRS the write aggregate never reaches the port — the
// projection lives in query.projectLead; this is a trivial copy.
func leadViewToDto(v query.LeadView) LeadDto {
	return LeadDto{
		ID:                      v.ID,
		TenantID:                v.TenantID,
		Stage:                   v.Stage,
		Temperature:             v.Temperature,
		ContactName:             v.ContactName,
		PhoneE164:               v.PhoneE164,
		City:                    v.City,
		District:                v.District,
		State:                   v.State,
		Pincode:                 v.Pincode,
		BusinessType:            v.BusinessType,
		MedicineSystem:          v.MedicineSystem,
		OrderValue:              v.OrderValue,
		BuyTimeline:             v.BuyTimeline,
		HasDrugLicence:          v.HasDrugLicence,
		HasGst:                  v.HasGst,
		GstVerified:             v.GstVerified,
		ProductRanges:           v.ProductRanges,
		DosageForms:             v.DosageForms,
		ExtraProfile:            v.ExtraProfile,
		AssigneeMembershipID:    v.AssigneeMembershipID,
		AssignedAt:              v.AssignedAt,
		SourcePurchaseID:        v.SourcePurchaseID,
		SourcePlatformLeadID:    v.SourcePlatformLeadID,
		ConvertedAt:             v.ConvertedAt,
		ConvertedByMembershipID: v.ConvertedByMembershipID,
		LostAt:                  v.LostAt,
		LostByMembershipID:      v.LostByMembershipID,
		LostReason:              v.LostReason,
		CreatedAt:               v.CreatedAt,
		CreatedByMembershipID:   v.CreatedByMembershipID,
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{
		Type:    "https://leadkart.api/errors/" + code,
		Title:   http.StatusText(status),
		Status:  status,
		Detail:  message,
		Error:   code,
		Message: message,
	})
}
