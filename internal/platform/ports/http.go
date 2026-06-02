package ports

import (
	"encoding/json"
	"errors"
	"io"
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

// AddRoutes registers Platform HTTP handlers on mux (ADR 0059). Mat Ryer
// 2024 canon: ports own request/response translation, not the URL space.
//
// verifier and stampValidator gate every route — both MUST be non-nil to
// register anything. Pass (nil, nil) only from fixtures that skip the auth
// surface; every platform route is gated, so that registers nothing.
func AddRoutes(
	mux *http.ServeMux,
	log *slog.Logger,
	a app.Application,
	verifier authn.Verifier,
	stampValidator authn.StampValidator,
) {
	if verifier == nil || stampValidator == nil {
		// No anonymous surface — without a verifier, register nothing.
		return
	}

	// chain wraps outer around an inner-wrapped handler — used to layer
	// RequirePlatform atop RequirePermission on Platform-only routes.
	chain := func(outer, inner func(http.Handler) http.Handler) func(http.Handler) http.Handler {
		return func(h http.Handler) http.Handler {
			return outer(inner(h))
		}
	}

	// requirePlatform enforces both is_platform=true AND tenant_slug=="platform"
	// on the JWT (multi-tenancy.md "Platform admin endpoints"). A tenant role
	// mis-granted Platform.* permissions still can't reach these routes — its
	// JWT carries is_platform=false. Defense-in-depth atop the permission gate.
	requirePlatform := authn.RequirePlatform(verifier, stampValidator)

	// UnverifiedContacts writes + the Platform-only LIST: permission gate plus
	// requirePlatform. LIST is the most sensitive — it exposes the whole
	// pipeline including PII.
	manageContacts := authn.RequirePermission(verifier, stampValidator,
		permission.IdentityPermissions.PlatformUnverifiedContacts.Manage)

	platformContactsGate := chain(requirePlatform, manageContacts)
	mux.Handle("POST /api/v1/platform/unverified-contacts",
		platformContactsGate(handleCreateUnverifiedContact(log, a)))
	mux.Handle("POST /api/v1/platform/unverified-contacts/{id}/calls",
		platformContactsGate(handleLogVerificationCall(log, a)))
	mux.Handle("POST /api/v1/platform/unverified-contacts/{id}/verify",
		platformContactsGate(handleVerifyUnverifiedContact(log, a)))
	mux.Handle("POST /api/v1/platform/unverified-contacts/{id}/reject",
		platformContactsGate(handleRejectUnverifiedContact(log, a)))
	mux.Handle("GET /api/v1/platform/unverified-contacts",
		platformContactsGate(handleListUnverifiedContacts(log, a)))

	// Marketplace browse — held by every tenant role.
	browse := authn.RequirePermission(verifier, stampValidator,
		permission.IdentityPermissions.PlatformMarketplace.Browse)
	mux.Handle("GET /api/v1/platform/marketplace/leads",
		browse(handleBrowseMarketplace(log, a)))

	// Marketplace purchase — tenant-facing, so NO requirePlatform.
	purchase := authn.RequirePermission(verifier, stampValidator,
		permission.IdentityPermissions.PlatformMarketplace.Purchase)
	mux.Handle("POST /api/v1/platform/marketplace/leads/{id}/purchase",
		purchase(handlePurchaseLead(log, a)))

	// LeadCredits topup — Platform-tier only; requirePlatform atop the
	// permission so a mis-granted tenant role still can't credit balances.
	topup := authn.RequirePermission(verifier, stampValidator,
		permission.IdentityPermissions.PlatformLeadCredits.Topup)
	mux.Handle("POST /api/v1/platform/lead-credits/topup",
		chain(requirePlatform, topup)(handleTopupLeadCredits(log, a)))

	// LeadCredits read — held by every tenant role and Platform.
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
		case errors.Is(err, command.ErrContactAlreadyTerminal):
			// H11 — refuse duplicate verify: avoids a second
			// LeadVerifiedV1 and a phantom PlatformLead.
			writeError(w, http.StatusConflict, ErrCodeContactAlreadyTerminal,
				"contact is already in a terminal state")
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
		// Body is optional/empty — price is computed server-side (ADR 0065).
		var req PurchaseLeadRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidBody, "request body is not valid JSON")
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

		_ = req // body currently carries no fields; price is server-computed
		out, err := a.Commands.PurchaseLead.Handle(r.Context(), command.PurchaseLeadCommand{
			PlatformLeadID:         platformlead.ID(leadID),
			PurchasingTenantID:     platformlead.TenantID(c.TenantID),
			PurchasingMembershipID: unverifiedcontact.MembershipID(membershipID),
		})
		switch {
		case errors.Is(err, command.ErrLeadNotFound):
			writeError(w, http.StatusNotFound, ErrCodeLeadNotFound, "")
			return
		case errors.Is(err, command.ErrLeadSoldOut):
			writeError(w, http.StatusConflict, ErrCodeLeadSoldOut, "lead has reached its sale limit")
			return
		case errors.Is(err, command.ErrLeadAlreadyPurchased):
			writeError(w, http.StatusConflict, ErrCodeLeadAlreadyPurchased, "tenant already purchased this lead")
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
			AmountPaisa:    out.AmountPaisa,
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
		// Balance is for the JWT's tenant. A platform user without
		// impersonation sees the platform tenant's (usually zero) row;
		// per-tenant operator views await a future GET .../tenants/{id}/balance.
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
			// No row yet → 200 + zero balance; row is INSERTed lazily on first topup.
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

func writeJSON(w http.ResponseWriter, status int, body any) {
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

// parsePathUUID returns the named path param if it's a valid UUID, else
// writes a 400 and returns ok=false.
func parsePathUUID(w http.ResponseWriter, r *http.Request, name, errCode string) (string, bool) {
	raw := strings.TrimSpace(r.PathValue(name))
	if _, err := uuid.Parse(raw); err != nil {
		writeError(w, http.StatusBadRequest, errCode, name+" must be a UUID")
		return "", false
	}
	return raw, true
}

// parsePageSize parses and clamps ?page_size=N; default when absent,
// error on non-numeric.
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

// parseCSV splits a comma-separated query param, dropping empties.
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

// parseBoolPtr parses a tri-state query bool: true/1/yes → &true,
// false/0/no → &false, anything else (incl. empty) → nil = no filter.
func parseBoolPtr(s string) *bool {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return nil
	}
	switch s {
	case "true", "1", "yes":
		return new(true)
	case "false", "0", "no":
		return new(false)
	}
	return nil
}

// membershipIDFromCtx returns the JWT's membership_id (for "who did this"
// stamps) and ok=false when absent or non-UUID; callers surface 401.
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
