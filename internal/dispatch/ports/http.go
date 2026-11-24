package ports

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/leadkart/leadkart-go/internal/common/parcel"
	"github.com/leadkart/leadkart-go/internal/common/textlimit"
	"github.com/leadkart/leadkart-go/internal/dispatch/app"
	"github.com/leadkart/leadkart-go/internal/dispatch/app/command"
	"github.com/leadkart/leadkart-go/internal/dispatch/app/query"
	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// AddRoutes registers Dispatch HTTP handlers on mux per Mat Ryer 2024
// canon.
//
// Routes registered here (all under /api/v1/dispatch/):
//
//	POST /api/v1/dispatch/consignment-notes                    create slot (manual)
//	GET  /api/v1/dispatch/consignment-notes?order_id={uuid}    by-order lookup
//	GET  /api/v1/dispatch/consignment-notes/{consignmentNoteId} single read
//	POST /api/v1/dispatch/consignment-notes/{id}/dispatch      pending → dispatched
//	POST /api/v1/dispatch/consignment-notes/{id}/in-transit    dispatched → in_transit
//	POST /api/v1/dispatch/consignment-notes/{id}/delivered     → delivered (terminal)
//	POST /api/v1/dispatch/consignment-notes/{id}/failed        → failed (terminal)
//
// Per-handler permission gates (ADR 0036 closed-set catalog):
//
//	read:   dispatch.consignment_notes.read
//	manage: dispatch.consignment_notes.manage (create + all transitions)
func AddRoutes(mux *http.ServeMux, log *slog.Logger, a app.Application, verifier authn.Verifier, stampValidator authn.StampValidator) {
	if verifier == nil || stampValidator == nil {
		return
	}
	read := authn.RequirePermission(verifier, stampValidator, permission.IdentityPermissions.Dispatch.ConsignmentNotes.Read)
	manage := authn.RequirePermission(verifier, stampValidator, permission.IdentityPermissions.Dispatch.ConsignmentNotes.Manage)

	mux.Handle("POST /api/v1/dispatch/consignment-notes", manage(handleCreate(log, a)))
	mux.Handle("GET /api/v1/dispatch/consignment-notes", read(handleGetByOrder(log, a)))
	mux.Handle("GET /api/v1/dispatch/consignment-notes/{consignmentNoteId}", read(handleGet(log, a)))
	mux.Handle("POST /api/v1/dispatch/consignment-notes/{consignmentNoteId}/dispatch", manage(handleMarkDispatched(log, a)))
	mux.Handle("POST /api/v1/dispatch/consignment-notes/{consignmentNoteId}/in-transit", manage(handleMarkInTransit(log, a)))
	mux.Handle("POST /api/v1/dispatch/consignment-notes/{consignmentNoteId}/delivered", manage(handleMarkDelivered(log, a)))
	mux.Handle("POST /api/v1/dispatch/consignment-notes/{consignmentNoteId}/failed", manage(handleMarkFailed(log, a)))
}

// ----- Handlers -------------------------------------------------------------

func handleCreate(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		effTenant, ok := authn.EffectiveTenantID(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		tid := tenant.ID(effTenant)
		var req CreateConsignmentNoteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidBody, "request body is not valid JSON")
			return
		}
		orderID, ok := authn.CanonicalID(req.OrderID)
		if !ok {
			writeError(w, http.StatusBadRequest, errCodeInvalidOrderID, "order_id must be a UUID")
			return
		}
		out, err := a.Commands.CreateConsignmentNote.Handle(r.Context(), command.CreateConsignmentNoteCommand{
			TenantID:              tid,
			OrderID:               consignmentnote.OrderID(orderID),
			CarrierName:           req.CarrierName,
			BoxCount:              req.BoxCount,
			WeightGrams:           req.WeightGrams,
			ExpectedDeliveryAt:    req.ExpectedDeliveryAt,
			CreatedByMembershipID: membership.ID(c.MembershipID),
		})
		switch {
		case err == nil:
			status := http.StatusCreated
			if out.AlreadyExisted {
				status = http.StatusOK
			}
			writeJSON(w, status, CreateConsignmentNoteResponse{
				ConsignmentNoteID: out.ConsignmentNoteID.String(),
				AlreadyExisted:    out.AlreadyExisted,
			})
		case errors.Is(err, consignmentnote.ErrInvalid):
			writeInvalid(w, err)
		default:
			log.ErrorContext(r.Context(), "dispatch: create consignment note", "err", err)
			writeError(w, http.StatusInternalServerError, errCodeInternalError, "")
		}
	})
}

func handleGet(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effTenant, ok := authn.EffectiveTenantID(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		tid := tenant.ID(effTenant)
		id, ok := parseConsignmentNoteID(w, r)
		if !ok {
			return
		}
		got, err := a.Queries.GetConsignmentNote.Handle(r.Context(), query.GetConsignmentNoteQuery{
			TenantID: tid, ConsignmentNoteID: id,
		})
		switch {
		case errors.Is(err, query.ErrConsignmentNoteNotFound):
			writeError(w, http.StatusNotFound, errCodeConsignmentNoteNotFound, "")
		case err != nil:
			log.ErrorContext(r.Context(), "dispatch: get consignment note", "err", err)
			writeError(w, http.StatusInternalServerError, errCodeInternalError, "")
		default:
			writeJSON(w, http.StatusOK, consignmentNoteToDto(*got))
		}
	})
}

func handleGetByOrder(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		effTenant, ok := authn.EffectiveTenantID(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		tid := tenant.ID(effTenant)
		rawOrderID := r.URL.Query().Get("order_id")
		if rawOrderID == "" {
			writeError(w, http.StatusBadRequest, errCodeOrderIDRequired, "order_id query parameter is required")
			return
		}
		orderID, ok := authn.CanonicalID(rawOrderID)
		if !ok {
			writeError(w, http.StatusBadRequest, errCodeInvalidOrderID, "order_id must be a UUID")
			return
		}
		got, err := a.Queries.GetConsignmentNoteByOrder.Handle(r.Context(), query.GetConsignmentNoteByOrderQuery{
			TenantID: tid, OrderID: consignmentnote.OrderID(orderID),
		})
		switch {
		case errors.Is(err, query.ErrConsignmentNoteNotFound):
			writeError(w, http.StatusNotFound, errCodeConsignmentNoteNotFound, "")
		case err != nil:
			log.ErrorContext(r.Context(), "dispatch: get consignment note by order", "err", err)
			writeError(w, http.StatusInternalServerError, errCodeInternalError, "")
		default:
			writeJSON(w, http.StatusOK, consignmentNoteToDto(*got))
		}
	})
}

func handleMarkDispatched(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, tid, id, ok := actorTenantID(w, r)
		if !ok {
			return
		}
		var req MarkDispatchedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidBody, "request body is not valid JSON")
			return
		}
		if strings.TrimSpace(req.DocketNumber) == "" {
			writeError(w, http.StatusBadRequest, errCodeDocketRequired, "docket_number is required")
			return
		}
		err := a.Commands.MarkDispatched.Handle(r.Context(), command.MarkDispatchedCommand{
			TenantID: tid, ConsignmentNoteID: id,
			DocketNumber:             req.DocketNumber,
			TransitionedByMembership: actor,
		})
		mapMutationErr(w, log, r, err, "mark dispatched")
	})
}

func handleMarkInTransit(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, tid, id, ok := actorTenantID(w, r)
		if !ok {
			return
		}
		err := a.Commands.MarkInTransit.Handle(r.Context(), command.MarkInTransitCommand{
			TenantID: tid, ConsignmentNoteID: id,
			TransitionedByMembership: actor,
		})
		mapMutationErr(w, log, r, err, "mark in transit")
	})
}

func handleMarkDelivered(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, tid, id, ok := actorTenantID(w, r)
		if !ok {
			return
		}
		err := a.Commands.MarkDelivered.Handle(r.Context(), command.MarkDeliveredCommand{
			TenantID: tid, ConsignmentNoteID: id,
			TransitionedByMembership: actor,
		})
		mapMutationErr(w, log, r, err, "mark delivered")
	})
}

func handleMarkFailed(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, tid, id, ok := actorTenantID(w, r)
		if !ok {
			return
		}
		var req MarkFailedRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidBody, "request body is not valid JSON")
			return
		}
		if strings.TrimSpace(req.Reason) == "" {
			writeError(w, http.StatusBadRequest, errCodeReasonRequired, "reason is required")
			return
		}
		err := a.Commands.MarkFailed.Handle(r.Context(), command.MarkFailedCommand{
			TenantID: tid, ConsignmentNoteID: id,
			Reason:                   req.Reason,
			TransitionedByMembership: actor,
		})
		mapMutationErr(w, log, r, err, "mark failed")
	})
}

// ----- Helpers --------------------------------------------------------------

// actorTenantID resolves the actor membership + tenant + path id common
// to every transition handler, writing the appropriate error response +
// returning ok=false when any precondition fails. It deliberately returns
// the membership.ID (not the JWT claims struct) so this port does not
// import identity's private app/jwt layer (TestArch_NoCrossModuleImports).
func actorTenantID(w http.ResponseWriter, r *http.Request) (membership.ID, tenant.ID, consignmentnote.ID, bool) {
	c, ok := authn.ClaimsFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
		return "", "", "", false
	}
	effTenant, ok := authn.EffectiveTenantID(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
		return "", "", "", false
	}
	tid := tenant.ID(effTenant)
	id, ok := parseConsignmentNoteID(w, r)
	if !ok {
		return "", "", "", false
	}
	return membership.ID(c.MembershipID), tid, id, true
}

func parseConsignmentNoteID(w http.ResponseWriter, r *http.Request) (consignmentnote.ID, bool) {
	id, ok := authn.CanonicalID(r.PathValue("consignmentNoteId"))
	if !ok {
		writeError(w, http.StatusBadRequest, errCodeInvalidConsignmentID, "consignmentNoteId must be a UUID")
		return "", false
	}
	return consignmentnote.ID(id), true
}

// writeInvalid answers 422 for a domain refusal: the stable code of the rule
// broken when it has one (a pack rule from common/parcel, a text over its
// limit), else the generic invalid_body.
func writeInvalid(w http.ResponseWriter, err error) {
	var pe *parcel.Error
	if errors.As(err, &pe) {
		writeError(w, http.StatusUnprocessableEntity, pe.Code, pe.Detail)
		return
	}
	if te, ok := textlimit.Of(err); ok {
		writeError(w, http.StatusUnprocessableEntity, te.Code(), te.Error())
		return
	}
	writeError(w, http.StatusUnprocessableEntity, errCodeInvalidBody, err.Error())
}

func mapMutationErr(w http.ResponseWriter, log *slog.Logger, r *http.Request, err error, op string) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, consignmentnote.ErrNotFound):
		writeError(w, http.StatusNotFound, errCodeConsignmentNoteNotFound, "")
	case errors.Is(err, consignmentnote.ErrInvalidTransition):
		writeError(w, http.StatusConflict, errCodeInvalidStatusTransition, err.Error())
	case errors.Is(err, command.ErrOrderNotDispatchable):
		writeError(w, http.StatusConflict, errCodeOrderNotDispatchable, "the order is not in a state goods can leave from (it is cancelled or already past dispatch)")
	case errors.Is(err, command.ErrStockExpired):
		writeError(w, http.StatusConflict, errCodeOrderStockExpired, "the goods of the order are past their printed expiry; expired stock is never sold, cancel the order instead")
	case errors.Is(err, command.ErrStockMismatch):
		writeError(w, http.StatusConflict, errCodeOrderStockMismatch, "the stock held for the order does not equal the quantity it confirmed (less or more); goods leave only for what was ordered")
	case errors.Is(err, command.ErrStockNotHeld):
		writeError(w, http.StatusConflict, errCodeOrderStockNotHeld, "the order holds no stock to ship; goods never leave without a deduction")
	case errors.Is(err, command.ErrOrderNotInvoiced):
		writeError(w, http.StatusConflict, errCodeOrderNotInvoiced, "the order has no tax invoice yet; goods are not dispatched without one")
	case errors.Is(err, consignmentnote.ErrInvalid):
		writeInvalid(w, err)
	default:
		log.ErrorContext(r.Context(), "dispatch: "+op, "err", err)
		writeError(w, http.StatusInternalServerError, errCodeInternalError, "")
	}
}

func consignmentNoteToDto(v query.ConsignmentNoteView) ConsignmentNoteDto {
	return ConsignmentNoteDto{
		ID:                    v.ID,
		TenantID:              v.TenantID,
		OrderID:               v.OrderID,
		Status:                v.Status,
		CarrierName:           v.CarrierName,
		DocketNumber:          v.DocketNumber,
		BoxCount:              v.BoxCount,
		WeightGrams:           v.WeightGrams,
		ExpectedDeliveryAt:    v.ExpectedDeliveryAt,
		DispatchedAt:          v.DispatchedAt,
		InTransitAt:           v.InTransitAt,
		DeliveredAt:           v.DeliveredAt,
		FailedAt:              v.FailedAt,
		FailureReason:         v.FailureReason,
		CreatedAt:             v.CreatedAt,
		CreatedByMembershipID: v.CreatedByMembershipID,
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
