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
	"github.com/leadkart/leadkart-go/internal/crm/app"
	"github.com/leadkart/leadkart-go/internal/crm/app/command"
	"github.com/leadkart/leadkart-go/internal/crm/app/query"
	"github.com/leadkart/leadkart-go/internal/crm/domain/calllog"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

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
}

// ----- Handlers --------------------------------------------------------------

func handleListLeads(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
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
			Cursor: cursor, PageSize: pageSize, Filter: filter, SelfFilter: selfFilter,
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
		for _, l := range page.Items {
			out.Items = append(out.Items, leadToDto(l))
		}
		writeJSON(w, http.StatusOK, out)
	})
}

func handleGetLead(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseLeadID(w, r)
		if !ok {
			return
		}
		l, err := a.Queries.GetLead.Handle(r.Context(), query.GetLeadQuery{LeadID: id})
		switch {
		case errors.Is(err, query.ErrLeadNotFound):
			writeError(w, http.StatusNotFound, errCodeLeadNotFound, "")
			return
		case err != nil:
			log.ErrorContext(r.Context(), "crm: get lead", "err", err)
			writeError(w, http.StatusInternalServerError, errCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusOK, leadToDto(l))
	})
}

func handleAssignLead(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			LeadID: id, AssigneeMembershipID: req.AssigneeMembershipID,
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
			LeadID: id, NewStage: stage, ChangedByMembershipID: c.MembershipID, Reason: req.Reason,
		})
		mapMutationErr(w, log, r, err, "change stage")
	})
}

func handleChangeTemperature(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			LeadID: id, NewTemperature: temp, ChangedByMembershipID: c.MembershipID,
		})
		mapMutationErr(w, log, r, err, "change temperature")
	})
}

func handleLogCall(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			LeadID: id, Outcome: outcome, Notes: req.Notes, LoggedByMembershipID: c.MembershipID,
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
			LeadID: id, ConvertedByMembershipID: c.MembershipID,
		})
		mapMutationErr(w, log, r, err, "convert lead")
	})
}

func handleLoseLead(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
			LeadID: id, LostByMembershipID: c.MembershipID, Reason: req.Reason,
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

func leadToDto(l *crmlead.CrmLead) LeadDto {
	p := l.Profile()
	// extra_profile as a generic map keeps the wire shape forward-
	// compatible: the JSONB column can grow new keys without forcing
	// a wire-DTO field bump.
	extra := map[string]any{}
	if p.Extra.Street != "" {
		extra["street"] = p.Extra.Street
	}
	if p.Extra.GstNumber != "" {
		extra["gst_number"] = p.Extra.GstNumber
	}
	if p.Extra.PanNumber != "" {
		extra["pan_number"] = p.Extra.PanNumber
	}
	if p.Extra.HasPan {
		extra["has_pan"] = true
	}
	if p.Extra.Email != "" {
		extra["email"] = p.Extra.Email
	}
	if p.Extra.Notes != "" {
		extra["notes"] = p.Extra.Notes
	}
	return LeadDto{
		ID:                       l.ID().String(),
		TenantID:                 l.TenantID(),
		Stage:                    l.Stage().String(),
		Temperature:              l.Temperature().String(),
		ContactName:              p.ContactName,
		PhoneE164:                p.PhoneE164,
		City:                     p.City,
		District:                 p.District,
		State:                    p.State,
		Pincode:                  p.Pincode,
		BusinessType:             p.BusinessType,
		MedicineSystem:           p.MedicineSystem,
		OrderValue:               p.OrderValue,
		BuyTimeline:              p.BuyTimeline,
		HasDrugLicence:           p.HasDrugLicence,
		HasGst:                   p.HasGst,
		GstVerified:              p.GstVerified,
		ProductRanges:            p.ProductRanges,
		DosageForms:              p.DosageForms,
		ExtraProfile:             extra,
		AssigneeMembershipID:     l.AssigneeMembershipID(),
		AssignedAt:               l.AssignedAt(),
		SourcePurchaseID:         l.SourcePurchaseID(),
		SourcePlatformLeadID:     l.SourcePlatformLeadID(),
		ConvertedAt:              l.ConvertedAt(),
		ConvertedByMembershipID:  l.ConvertedByMembershipID(),
		LostAt:                   l.LostAt(),
		LostByMembershipID:       l.LostByMembershipID(),
		LostReason:               l.LostReason(),
		CreatedAt:                l.CreatedAt(),
		CreatedByMembershipID:    l.CreatedByMembershipID(),
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
