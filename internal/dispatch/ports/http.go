package ports

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/dispatch/app"
	"github.com/leadkart/leadkart-go/internal/dispatch/app/command"
	"github.com/leadkart/leadkart-go/internal/dispatch/app/query"
	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// tenantFromContext extracts the caller's tenant ID from ctx (bound by
// authn middleware).
func tenantFromContext(r *http.Request) (tenant.ID, bool) {
	tid, ok := tenancy.FromContext(r.Context())
	if !ok || tid == "" {
		return tenant.ID(""), false
	}
	return tenant.ID(tid), true
}

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
		tid, ok := tenantFromContext(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		var req CreateConsignmentNoteRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidBody, "request body is not valid JSON")
			return
		}
		if _, err := uuid.Parse(strings.TrimSpace(req.OrderID)); err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidOrderID, "order_id must be a UUID")
			return
		}
		out, err := a.Commands.CreateConsignmentNote.Handle(r.Context(), command.CreateConsignmentNoteCommand{
			TenantID:              tid,
			OrderID:               consignmentnote.OrderID(req.OrderID),
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
			writeError(w, http.StatusUnprocessableEntity, errCodeInvalidBody, err.Error())
		default:
			log.ErrorContext(r.Context(), "dispatch: create consignment note", "err", err)
			writeError(w, http.StatusInternalServerError, errCodeInternalError, "")
		}
	})
}

func handleGet(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := tenantFromContext(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
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
		tid, ok := tenantFromContext(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		orderID := strings.TrimSpace(r.URL.Query().Get("order_id"))
		if orderID == "" {
			writeError(w, http.StatusBadRequest, errCodeOrderIDRequired, "order_id query parameter is required")
			return
		}
		if _, err := uuid.Parse(orderID); err != nil {
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
	tid, ok := tenantFromContext(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
		return "", "", "", false
	}
	id, ok := parseConsignmentNoteID(w, r)
	if !ok {
		return "", "", "", false
	}
	return membership.ID(c.MembershipID), tid, id, true
}

func parseConsignmentNoteID(w http.ResponseWriter, r *http.Request) (consignmentnote.ID, bool) {
	raw := r.PathValue("consignmentNoteId")
	if _, err := uuid.Parse(raw); err != nil {
		writeError(w, http.StatusBadRequest, errCodeInvalidConsignmentID, "consignmentNoteId must be a UUID")
		return "", false
	}
	return consignmentnote.ID(raw), true
}

func mapMutationErr(w http.ResponseWriter, log *slog.Logger, r *http.Request, err error, op string) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, consignmentnote.ErrNotFound):
		writeError(w, http.StatusNotFound, errCodeConsignmentNoteNotFound, "")
	case errors.Is(err, consignmentnote.ErrInvalidTransition):
		writeError(w, http.StatusConflict, errCodeInvalidStatusTransition, err.Error())
	case errors.Is(err, consignmentnote.ErrInvalid):
		writeError(w, http.StatusUnprocessableEntity, errCodeInvalidBody, err.Error())
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
