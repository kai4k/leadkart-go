package ports

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
	"github.com/leadkart/leadkart-go/internal/orders/app/command"
	"github.com/leadkart/leadkart-go/internal/orders/app/query"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
	"github.com/leadkart/leadkart-go/internal/orders/domain/payment"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation"
)

// ----- actor / tenant / id extraction ---------------------------------------

// actorTenant resolves the caller's membership + tenant, writing the
// appropriate error + returning ok=false on failure. Returns membership.ID
// (not the JWT claims struct) so this port does not import identity/app/jwt
// (TestArch_NoCrossModuleImports).
func actorTenant(w http.ResponseWriter, r *http.Request) (membership.ID, tenant.ID, bool) {
	c, ok := authn.ClaimsFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
		return "", "", false
	}
	tid, ok := tenantFromContext(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
		return "", "", false
	}
	return membership.ID(c.MembershipID), tid, true
}

func actorTenantQuotation(w http.ResponseWriter, r *http.Request) (membership.ID, tenant.ID, quotation.ID, bool) {
	actor, tid, ok := actorTenant(w, r)
	if !ok {
		return "", "", "", false
	}
	id, ok := parseQuotationID(w, r)
	if !ok {
		return "", "", "", false
	}
	return actor, tid, id, true
}

func actorTenantOrder(w http.ResponseWriter, r *http.Request) (membership.ID, tenant.ID, order.ID, bool) {
	actor, tid, ok := actorTenant(w, r)
	if !ok {
		return "", "", "", false
	}
	id, ok := parseOrderID(w, r)
	if !ok {
		return "", "", "", false
	}
	return actor, tid, id, true
}

func parseQuotationID(w http.ResponseWriter, r *http.Request) (quotation.ID, bool) {
	raw := r.PathValue("quotationId")
	if _, err := uuid.Parse(raw); err != nil {
		writeError(w, http.StatusBadRequest, errCodeInvalidQuotationID, "quotationId must be a UUID")
		return "", false
	}
	return quotation.ID(raw), true
}

func parseOrderID(w http.ResponseWriter, r *http.Request) (order.ID, bool) {
	raw := r.PathValue("orderId")
	if _, err := uuid.Parse(raw); err != nil {
		writeError(w, http.StatusBadRequest, errCodeInvalidOrderID, "orderId must be a UUID")
		return "", false
	}
	return order.ID(raw), true
}

// decode reads a JSON body, writing a 400 + returning false on malformed input.
func decode(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, errCodeInvalidBody, "request body is not valid JSON")
		return false
	}
	return true
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// ----- error mapping --------------------------------------------------------

func mapQuotationMutation(w http.ResponseWriter, log *slog.Logger, r *http.Request, err error, op string) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, quotation.ErrNotFound):
		writeError(w, http.StatusNotFound, errCodeQuotationNotFound, "")
	case errors.Is(err, quotation.ErrInvalidTransition):
		writeError(w, http.StatusConflict, errCodeInvalidTransition, err.Error())
	case errors.Is(err, quotation.ErrInvalid):
		writeError(w, http.StatusUnprocessableEntity, errCodeValidation, err.Error())
	default:
		internal(w, log, r, op, err)
	}
}

func mapOrderMutation(w http.ResponseWriter, log *slog.Logger, r *http.Request, err error, op string) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, order.ErrNotFound):
		writeError(w, http.StatusNotFound, errCodeOrderNotFound, "")
	case errors.Is(err, order.ErrInvalidTransition):
		writeError(w, http.StatusConflict, errCodeInvalidTransition, err.Error())
	case errors.Is(err, payment.ErrAlreadyExistsForExternalReference):
		writeError(w, http.StatusConflict, errCodePaymentConflict, "")
	case errors.Is(err, order.ErrInvalid), errors.Is(err, payment.ErrInvalid):
		writeError(w, http.StatusUnprocessableEntity, errCodeValidation, err.Error())
	default:
		internal(w, log, r, op, err)
	}
}

func internal(w http.ResponseWriter, log *slog.Logger, r *http.Request, op string, err error) {
	log.ErrorContext(r.Context(), "orders: "+op, "err", err)
	writeError(w, http.StatusInternalServerError, errCodeInternalError, "")
}

// ----- request/response mapping ---------------------------------------------

func toCmdItems(in []LineItemDto) []command.CreateQuotationLineItem {
	out := make([]command.CreateQuotationLineItem, len(in))
	for i, li := range in {
		out[i] = command.CreateQuotationLineItem{
			ProductID: li.ProductID, SKU: li.SKU, Description: li.Description,
			Quantity: li.Quantity, UnitMrpPaise: li.UnitMrpPaise,
			UnitSalePaise: li.UnitSalePaise, GstRateBps: li.GstRateBps,
		}
	}
	return out
}

func viewItemsToDto(in []query.LineItemView) []LineItemDto {
	out := make([]LineItemDto, len(in))
	for i, li := range in {
		out[i] = LineItemDto{
			ProductID: li.ProductID, SKU: li.SKU, Description: li.Description,
			Quantity: li.Quantity, UnitMrpPaise: li.UnitMrpPaise,
			UnitSalePaise: li.UnitSalePaise, GstRateBps: li.GstRateBps,
		}
	}
	return out
}

func quotationToDto(v query.QuotationView) QuotationDto {
	return QuotationDto{
		ID: v.ID, TenantID: v.TenantID, CustomerLeadID: v.CustomerLeadID, State: v.State,
		RevisionNumber: v.RevisionNumber, Items: viewItemsToDto(v.Items),
		RejectionReason: v.RejectionReason, CreatedAt: v.CreatedAt,
	}
}

func orderToDto(v query.OrderView) OrderDto {
	return OrderDto{
		ID: v.ID, TenantID: v.TenantID, ApprovedQuotationID: v.ApprovedQuotationID,
		CustomerLeadID: v.CustomerLeadID, State: v.State, Items: viewItemsToDto(v.Items),
		SubtotalPaise: v.SubtotalPaise, TaxPaise: v.TaxPaise, GrandTotalPaise: v.GrandTotalPaise,
		InvoiceID: v.InvoiceID, ConsignmentNoteID: v.ConsignmentNoteID,
		ConfirmedAt: v.ConfirmedAt, PackedAt: v.PackedAt, InvoicedAt: v.InvoicedAt,
		DispatchedAt: v.DispatchedAt, DeliveredAt: v.DeliveredAt, CompletedAt: v.CompletedAt,
		CancelledAt: v.CancelledAt, CancellationReason: v.CancellationReason, CreatedAt: v.CreatedAt,
	}
}

func invoiceToDto(v query.InvoiceView) InvoiceDto {
	return InvoiceDto{
		ID: v.ID, OrderID: v.OrderID, NumberDisplay: v.NumberDisplay, Items: viewItemsToDto(v.Items),
		SubtotalPaise: v.SubtotalPaise, TaxPaise: v.TaxPaise, GrandTotalPaise: v.GrandTotalPaise, IssuedAt: v.IssuedAt,
	}
}

func paymentToDto(v query.PaymentView) PaymentDto {
	return PaymentDto{
		ID: v.ID, OrderID: v.OrderID, Kind: v.Kind, Method: v.Method,
		AmountPaise: v.AmountPaise, ExternalReference: v.ExternalReference, ReceivedAt: v.ReceivedAt,
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
