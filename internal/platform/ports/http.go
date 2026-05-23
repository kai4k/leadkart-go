package ports

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
	"github.com/leadkart/leadkart-go/internal/platform/app"
	"github.com/leadkart/leadkart-go/internal/platform/app/command"
	"github.com/leadkart/leadkart-go/internal/platform/app/query"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadcredit"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadform"
	"github.com/leadkart/leadkart-go/internal/platform/domain/platformlead"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
	"github.com/leadkart/leadkart-go/internal/platform/domain/verificationcall"
)

// AddRoutes registers Platform HTTP handlers on mux. Mat Ryer 2024
// canon: ports own request/response translation, not the routing
// scheme — the composition root chooses the URL space.
//
// verifier + stampValidator gate authenticated routes. Both MUST be
// non-nil for the auth-route block to register; pass (nil, nil) only
// in test fixtures that don't exercise the auth surface (none exist
// in this slice — every route is gated).
//
// Routes registered (per ADR 0059 brief):
//
//	POST /api/v1/platform/unverified-contacts                    Lead Agent creates raw contact
//	POST /api/v1/platform/unverified-contacts/{id}/calls         log a verification call outcome
//	POST /api/v1/platform/unverified-contacts/{id}/verify        promote to PlatformLead
//	POST /api/v1/platform/unverified-contacts/{id}/reject        terminal reject
//	GET  /api/v1/platform/unverified-contacts                    Platform-only list (paginated)
//	GET  /api/v1/platform/marketplace/leads                      tenant-facing browse (paginated)
//	POST /api/v1/platform/marketplace/leads/{id}/purchase        tenant purchases with credits
//	POST /api/v1/platform/lead-credits/topup                     Platform-only top-up
//	GET  /api/v1/platform/lead-credits/balance                   per-caller balance
func AddRoutes(
	mux *http.ServeMux,
	log *slog.Logger,
	a app.Application,
	verifier authn.Verifier,
	stampValidator authn.StampValidator,
) {
	if verifier == nil || stampValidator == nil {
		// Platform module routes are 100% auth-gated — no anonymous
		// surface. Without the verifier we register nothing. Test
		// fixtures that DON'T exercise platform routes pass nil/nil;
		// production passes the real verifier.
		return
	}

	// UnverifiedContacts — Platform.UnverifiedContacts.Manage gate.
	manageContacts := authn.RequirePermission(verifier, stampValidator,
		permission.IdentityPermissions.PlatformUnverifiedContacts.Manage)

	mux.Handle("POST /api/v1/platform/unverified-contacts",
		manageContacts(handleCreateUnverifiedContact(log, a)))
	mux.Handle("POST /api/v1/platform/unverified-contacts/{id}/calls",
		manageContacts(handleLogVerificationCall(log, a)))
	mux.Handle("POST /api/v1/platform/unverified-contacts/{id}/verify",
		manageContacts(handleVerifyUnverifiedContact(log, a)))
	mux.Handle("POST /api/v1/platform/unverified-contacts/{id}/reject",
		manageContacts(handleRejectUnverifiedContact(log, a)))
	mux.Handle("GET /api/v1/platform/unverified-contacts",
		manageContacts(handleListUnverifiedContacts(log, a)))

	// Marketplace browse — held by every tenant role.
	browse := authn.RequirePermission(verifier, stampValidator,
		permission.IdentityPermissions.PlatformMarketplace.Browse)
	mux.Handle("GET /api/v1/platform/marketplace/leads",
		browse(handleBrowseMarketplace(log, a)))

	// Marketplace purchase — purchase permission.
	purchase := authn.RequirePermission(verifier, stampValidator,
		permission.IdentityPermissions.PlatformMarketplace.Purchase)
	mux.Handle("POST /api/v1/platform/marketplace/leads/{id}/purchase",
		purchase(handlePurchaseLead(log, a)))

	// LeadCredits topup — Platform-tier only.
	topup := authn.RequirePermission(verifier, stampValidator,
		permission.IdentityPermissions.PlatformLeadCredits.Topup)
	mux.Handle("POST /api/v1/platform/lead-credits/topup",
		topup(handleTopupLeadCredits(log, a)))

	// LeadCredits read — held by every tenant role + Platform.
	read := authn.RequirePermission(verifier, stampValidator,
		permission.IdentityPermissions.PlatformLeadCredits.Read)
	mux.Handle("GET /api/v1/platform/lead-credits/balance",
		read(handleGetLeadCreditBalance(log, a)))
}

// ----- Handlers ------------------------------------------------------------

func handleCreateUnverifiedContact(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req CreateUnverifiedContactRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		form, err := leadform.New(leadform.Input{
			ContactName:    req.ContactName,
			MobileE164:     req.MobileE164,
			Email:          req.Email,
			Pincode:        req.Pincode,
			City:           req.City,
			District:       req.District,
			State:          req.State,
			Street:         req.Street,
			HasDrugLicence: req.HasDrugLicence,
			HasGst:         req.HasGst,
			GstNumber:      req.GstNumber,
			HasPan:         req.HasPan,
			PanNumber:      req.PanNumber,
			BusinessType:   leadform.BusinessType(req.BusinessType),
			MedicineSystem: leadform.MedicineSystem(req.MedicineSystem),
			ProductRanges:  req.ProductRanges,
			DosageForms:    req.DosageForms,
			OrderValue:     leadform.OrderValue(req.OrderValue),
			BuyTimeline:    leadform.BuyTimeline(req.BuyTimeline),
		})
		if err != nil {
			writeError(w, http.StatusUnprocessableEntity, ErrCodeInvalidLeadForm, err.Error())
			return
		}

		membershipID, ok := membershipIDFromCtx(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeMembershipContextRequired, "")
			return
		}

		out, err := a.Commands.CreateUnverifiedContact.Handle(r.Context(), command.CreateUnverifiedContactCommand{
			Form:      form,
			CreatedBy: unverifiedcontact.MembershipID(membershipID),
		})
		if err != nil {
			log.ErrorContext(r.Context(), "create unverified contact failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusCreated, CreateUnverifiedContactResponse{
			ContactID: out.ContactID.String(),
		})
	})
}

func handleLogVerificationCall(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contactID, ok := parsePathUUID(w, r, "id", ErrCodeInvalidContactID)
		if !ok {
			return
		}
		var req LogVerificationCallRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		outcome := verificationcall.OutcomeCode(req.Outcome)
		if !outcome.IsValid() {
			writeError(w, http.StatusUnprocessableEntity, ErrCodeInvalidCallOutcome,
				"outcome must be one of: verified, rejected, busy, no_answer, wrong_number")
			return
		}
		membershipID, ok := membershipIDFromCtx(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeMembershipContextRequired, "")
			return
		}

		out, err := a.Commands.LogVerificationCall.Handle(r.Context(), command.LogVerificationCallCommand{
			ContactID:             unverifiedcontact.ID(contactID),
			Outcome:               outcome,
			Notes:                 req.Notes,
			CallbackWindowStartAt: req.CallbackWindowStartAt,
			CallbackWindowEndAt:   req.CallbackWindowEndAt,
			LoggedBy:              unverifiedcontact.MembershipID(membershipID),
		})
		switch {
		case errors.Is(err, command.ErrContactNotFound):
			writeError(w, http.StatusNotFound, ErrCodeContactNotFound, "")
			return
		case errors.Is(err, unverifiedcontact.ErrInvalid),
			errors.Is(err, verificationcall.ErrInvalid):
			writeError(w, http.StatusUnprocessableEntity, ErrCodeInvalidCallOutcome, err.Error())
			return
		case err != nil:
			log.ErrorContext(r.Context(), "log verification call failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusCreated, LogVerificationCallResponse{
			CallID: out.CallID.String(),
		})
	})
}

func handleVerifyUnverifiedContact(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contactID, ok := parsePathUUID(w, r, "id", ErrCodeInvalidContactID)
		if !ok {
			return
		}
		membershipID, ok := membershipIDFromCtx(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeMembershipContextRequired, "")
			return
		}
		out, err := a.Commands.VerifyUnverifiedContact.Handle(r.Context(), command.VerifyUnverifiedContactCommand{
			ContactID:  unverifiedcontact.ID(contactID),
			VerifiedBy: unverifiedcontact.MembershipID(membershipID),
		})
		switch {
		case errors.Is(err, command.ErrContactNotFound):
			writeError(w, http.StatusNotFound, ErrCodeContactNotFound, "")
			return
		case errors.Is(err, unverifiedcontact.ErrInvalid):
			writeError(w, http.StatusUnprocessableEntity, ErrCodeInvalidCallOutcome, err.Error())
			return
		case err != nil:
			log.ErrorContext(r.Context(), "verify unverified contact failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusCreated, VerifyUnverifiedContactResponse{
			PlatformLeadID: out.PlatformLeadID.String(),
		})
	})
}

func handleRejectUnverifiedContact(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		contactID, ok := parsePathUUID(w, r, "id", ErrCodeInvalidContactID)
		if !ok {
			return
		}
		var req RejectUnverifiedContactRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		if strings.TrimSpace(req.Reason) == "" {
			writeError(w, http.StatusUnprocessableEntity, ErrCodeInvalidBody, "reason required")
			return
		}
		membershipID, ok := membershipIDFromCtx(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeMembershipContextRequired, "")
			return
		}
		err := a.Commands.RejectUnverifiedContact.Handle(r.Context(), command.RejectUnverifiedContactCommand{
			ContactID:  unverifiedcontact.ID(contactID),
			Reason:     req.Reason,
			RejectedBy: unverifiedcontact.MembershipID(membershipID),
		})
		switch {
		case errors.Is(err, command.ErrContactNotFound):
			writeError(w, http.StatusNotFound, ErrCodeContactNotFound, "")
			return
		case errors.Is(err, unverifiedcontact.ErrInvalid):
			writeError(w, http.StatusUnprocessableEntity, ErrCodeInvalidBody, err.Error())
			return
		case err != nil:
			log.ErrorContext(r.Context(), "reject unverified contact failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func handleListUnverifiedContacts(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state := strings.TrimSpace(r.URL.Query().Get("status"))
		cursor, err := pagination.Decode(r.URL.Query().Get("cursor"))
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidCursor, "")
			return
		}
		pageSize, err := parsePageSize(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidPageSize, err.Error())
			return
		}
		page, err := a.Queries.ListUnverifiedContacts.Handle(r.Context(), query.ListUnverifiedContactsQuery{
			State:    state,
			Cursor:   cursor,
			PageSize: pageSize,
		})
		if err != nil {
			log.ErrorContext(r.Context(), "list unverified contacts failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		out := ListUnverifiedContactsResponse{
			Items:      make([]UnverifiedContactDto, 0, len(page.Items)),
			HasMore:    page.HasMore,
			NextCursor: page.NextCursor,
		}
		for _, v := range page.Items {
			out.Items = append(out.Items, UnverifiedContactDto{
				ID:                    v.ID,
				State:                 v.State,
				ContactName:           v.ContactName,
				MobileE164:            v.MobileE164,
				City:                  v.City,
				StateGeo:              v.StateGeo,
				CreatedAt:             v.CreatedAt,
				CreatedByMembershipID: v.CreatedByMembershipID,
			})
		}
		writeJSON(w, http.StatusOK, out)
	})
}

func handleBrowseMarketplace(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		filter := platformlead.MarketplaceFilter{
			State:          strings.TrimSpace(q.Get("state")),
			City:           strings.TrimSpace(q.Get("city")),
			District:       strings.TrimSpace(q.Get("district")),
			Pincode:        strings.TrimSpace(q.Get("pincode")),
			BusinessType:   strings.TrimSpace(q.Get("business_type")),
			MedicineSystem: strings.TrimSpace(q.Get("medicine_system")),
			OrderValue:     strings.TrimSpace(q.Get("order_value")),
			BuyTimeline:    strings.TrimSpace(q.Get("buy_timeline")),
			ProductRanges:  parseCSV(q.Get("product_ranges")),
			DosageForms:    parseCSV(q.Get("dosage_forms")),
			HasDrugLicence: parseBoolPtr(q.Get("has_drug_licence")),
			HasGst:         parseBoolPtr(q.Get("has_gst")),
			GstVerified:    parseBoolPtr(q.Get("gst_verified")),
		}
		cursor, err := pagination.Decode(q.Get("cursor"))
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidCursor, "")
			return
		}
		pageSize, err := parsePageSize(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidPageSize, err.Error())
			return
		}

		page, err := a.Queries.BrowseMarketplace.Handle(r.Context(), query.BrowseMarketplaceQuery{
			Filter:   filter,
			Cursor:   cursor,
			PageSize: pageSize,
		})
		if err != nil {
			log.ErrorContext(r.Context(), "browse marketplace failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		out := BrowseMarketplaceResponse{
			Items:      make([]MarketplaceLeadDto, 0, len(page.Items)),
			HasMore:    page.HasMore,
			NextCursor: page.NextCursor,
		}
		for _, v := range page.Items {
			out.Items = append(out.Items, MarketplaceLeadDto{
				ID:             v.ID,
				ContactName:    v.ContactName,
				City:           v.City,
				District:       v.District,
				State:          v.State,
				Pincode:        v.PinCode,
				HasDrugLicence: v.HasDrugLicence,
				HasGst:         v.HasGst,
				GstVerified:    v.GstVerified,
				HasPan:         v.HasPan,
				BusinessType:   v.BusinessType,
				MedicineSystem: v.MedicineSystem,
				ProductRanges:  v.ProductRanges,
				DosageForms:    v.DosageForms,
				OrderValue:     v.OrderValue,
				BuyTimeline:    v.BuyTimeline,
				VerifiedAt:     v.VerifiedAt,
			})
		}
		writeJSON(w, http.StatusOK, out)
	})
}

func handlePurchaseLead(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leadID, ok := parsePathUUID(w, r, "id", ErrCodeInvalidLeadID)
		if !ok {
			return
		}
		var req PurchaseLeadRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		if req.AmountPaisa <= 0 {
			writeError(w, http.StatusUnprocessableEntity, ErrCodeInvalidPurchaseAmount,
				"amount_paisa must be positive")
			return
		}
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeMembershipContextRequired, "")
			return
		}
		membershipID, ok := membershipIDFromCtx(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeMembershipContextRequired, "")
			return
		}

		out, err := a.Commands.PurchaseLead.Handle(r.Context(), command.PurchaseLeadCommand{
			PlatformLeadID:         platformlead.ID(leadID),
			PurchasingTenantID:     platformlead.TenantID(c.TenantID),
			PurchasingMembershipID: unverifiedcontact.MembershipID(membershipID),
			AmountPaisa:            req.AmountPaisa,
		})
		switch {
		case errors.Is(err, command.ErrLeadNotFound):
			writeError(w, http.StatusNotFound, ErrCodeLeadNotFound, "")
			return
		case errors.Is(err, command.ErrLeadAlreadySold):
			writeError(w, http.StatusConflict, ErrCodeLeadAlreadySold, "")
			return
		case errors.Is(err, command.ErrInsufficientCredits):
			writeError(w, http.StatusUnprocessableEntity, ErrCodeInsufficientCredits,
				"tenant has insufficient lead credits")
			return
		case errors.Is(err, leadcredit.ErrConflict):
			writeError(w, http.StatusConflict, ErrCodeCreditConflict,
				"credit row update conflict; retry")
			return
		case err != nil:
			log.ErrorContext(r.Context(), "purchase lead failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusCreated, PurchaseLeadResponse{
			PurchaseID:     out.PurchaseID,
			PlatformLeadID: leadID,
		})
	})
}

func handleTopupLeadCredits(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req TopupLeadCreditsRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
			return
		}
		if _, err := uuid.Parse(req.TenantID); err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidTenantID, "tenant_id must be a UUID")
			return
		}
		if req.Delta <= 0 {
			writeError(w, http.StatusUnprocessableEntity, ErrCodeInvalidPurchaseAmount,
				"delta must be positive")
			return
		}
		if strings.TrimSpace(req.Reason) == "" {
			writeError(w, http.StatusUnprocessableEntity, ErrCodeInvalidBody, "reason required")
			return
		}
		membershipID, ok := membershipIDFromCtx(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeMembershipContextRequired, "")
			return
		}

		out, err := a.Commands.TopupLeadCredits.Handle(r.Context(), command.TopupLeadCreditsCommand{
			TenantID:   leadcredit.TenantID(req.TenantID),
			Delta:      req.Delta,
			Reason:     req.Reason,
			AdjustedBy: leadcredit.MembershipID(membershipID),
		})
		if err != nil {
			log.ErrorContext(r.Context(), "topup lead credits failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusOK, TopupLeadCreditsResponse{
			TenantID:   req.TenantID,
			NewBalance: out.NewBalance,
		})
	})
}

func handleGetLeadCreditBalance(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Caller's tenant from the JWT — tenant users see their own
		// balance; platform users get whatever tenant the JWT context
		// carries (in v0.2 platform user without impersonation sees
		// the platform tenant's balance row, which is typically zero
		// — they should use a future GET .../tenants/{id}/balance for
		// per-tenant operator views).
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, ErrCodeMembershipContextRequired, "")
			return
		}
		out, err := a.Queries.GetLeadCreditBalance.Handle(r.Context(), query.GetLeadCreditBalanceQuery{
			TenantID: leadcredit.TenantID(c.TenantID),
		})
		switch {
		case errors.Is(err, query.ErrCreditRowNotFound):
			// No row yet → 200 + zero balance (canonical zero-or-row
			// shape; the row gets INSERTed lazily on first topup).
			writeJSON(w, http.StatusOK, LeadCreditBalanceResponse{
				TenantID: c.TenantID,
				Balance:  0,
			})
			return
		case err != nil:
			log.ErrorContext(r.Context(), "get lead credit balance failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusOK, LeadCreditBalanceResponse{
			TenantID: out.TenantID,
			Balance:  out.Balance,
		})
	})
}

// ----- Helpers --------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, body interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

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

func problemType(code string) string {
	if code == "" {
		return ""
	}
	return "https://leadkart.api/errors/" + code
}

// parsePathUUID extracts a path parameter, validates UUID shape, +
// writes a 400 on failure. Returns (string, true) on success.
func parsePathUUID(w http.ResponseWriter, r *http.Request, name, errCode string) (string, bool) {
	raw := strings.TrimSpace(r.PathValue(name))
	if _, err := uuid.Parse(raw); err != nil {
		writeError(w, http.StatusBadRequest, errCode, name+" must be a UUID")
		return "", false
	}
	return raw, true
}

// parsePageSize parses ?page_size=N and clamps via pagination.
// Returns the default when absent; rejects non-numeric.
func parsePageSize(r *http.Request) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("page_size"))
	if raw == "" {
		return pagination.DefaultPageSize, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.New("page_size must be an integer")
	}
	return pagination.ClampPageSize(n), nil
}

// parseCSV splits a comma-separated query parameter; empties dropped.
func parseCSV(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		t := strings.TrimSpace(p)
		if t != "" {
			out = append(out, t)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// parseBoolPtr parses a tri-state query bool. Empty string returns
// nil (no filter); "true"/"1"/"yes" returns &true; "false"/"0"/"no"
// returns &false; everything else returns nil (silently).
func parseBoolPtr(s string) *bool {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return nil
	}
	switch s {
	case "true", "1", "yes":
		t := true
		return &t
	case "false", "0", "no":
		f := false
		return &f
	}
	return nil
}

// membershipIDFromCtx extracts the verified JWT's membership_id claim
// so handlers can stamp "who did this" on aggregate methods. Falls
// back to an empty string when the claim is missing — the caller
// branches on the boolean to surface 401.
func membershipIDFromCtx(r *http.Request) (string, bool) {
	c, ok := authn.ClaimsFromContext(r.Context())
	if !ok {
		return "", false
	}
	m := strings.TrimSpace(c.MembershipID)
	if m == "" {
		return "", false
	}
	if _, err := uuid.Parse(m); err != nil {
		return "", false
	}
	return m, true
}
