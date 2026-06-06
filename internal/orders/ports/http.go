package ports

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
	"github.com/leadkart/leadkart-go/internal/orders/app"
	"github.com/leadkart/leadkart-go/internal/orders/app/command"
	"github.com/leadkart/leadkart-go/internal/orders/app/query"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoice"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
	"github.com/leadkart/leadkart-go/internal/orders/domain/payment"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation"
)

func tenantFromContext(r *http.Request) (tenant.ID, bool) {
	tid, ok := tenancy.FromContext(r.Context())
	if !ok || tid == "" {
		return tenant.ID(""), false
	}
	return tenant.ID(tid), true
}

// AddRoutes registers Orders HTTP handlers on mux per Mat Ryer 2024 canon.
//
// Routes (all under /api/v1/orders/):
//
//	POST quotations                          create draft (quotations.manage)
//	GET  quotations/{quotationId}            read         (quotations.read)
//	POST quotations/{quotationId}/revise     revise       (quotations.manage)
//	POST quotations/{quotationId}/approve    approve+seed order (quotations.manage)
//	POST quotations/{quotationId}/reject     reject       (quotations.manage)
//	GET  orders/{orderId}                    read         (orders.read)
//	POST orders/{orderId}/token-payment      token + advance (orders.manage)
//	POST orders/{orderId}/confirm            confirm      (orders.manage)
//	POST orders/{orderId}/pack               pack         (orders.manage)
//	POST orders/{orderId}/invoice            invoice      (orders.manage)
//	POST orders/{orderId}/cancel             cancel       (orders.manage)
//	POST orders/{orderId}/complete           complete     (orders.manage)
//	GET  orders/{orderId}/invoice            read invoice (invoices.read)
//	GET  orders/{orderId}/payments           list payments (orders.read)
//	POST orders/{orderId}/payments           record payment (payments.record)
func AddRoutes(mux *http.ServeMux, log *slog.Logger, a app.Application, verifier authn.Verifier, stampValidator authn.StampValidator) {
	if verifier == nil || stampValidator == nil {
		return
	}
	p := permission.IdentityPermissions.Orders
	quoteRead := authn.RequirePermission(verifier, stampValidator, p.Quotations.Read)
	quoteManage := authn.RequirePermission(verifier, stampValidator, p.Quotations.Manage)
	orderRead := authn.RequirePermission(verifier, stampValidator, p.Orders.Read)
	orderManage := authn.RequirePermission(verifier, stampValidator, p.Orders.Manage)
	invoiceRead := authn.RequirePermission(verifier, stampValidator, p.Invoices.Read)
	paymentRecord := authn.RequirePermission(verifier, stampValidator, p.Payments.Record)

	mux.Handle("POST /api/v1/orders/quotations", quoteManage(handleCreateQuotation(log, a)))
	mux.Handle("GET /api/v1/orders/quotations/{quotationId}", quoteRead(handleGetQuotation(log, a)))
	mux.Handle("POST /api/v1/orders/quotations/{quotationId}/revise", quoteManage(handleReviseQuotation(log, a)))
	mux.Handle("POST /api/v1/orders/quotations/{quotationId}/approve", quoteManage(handleApproveQuotation(log, a)))
	mux.Handle("POST /api/v1/orders/quotations/{quotationId}/reject", quoteManage(handleRejectQuotation(log, a)))

	mux.Handle("GET /api/v1/orders/orders/{orderId}", orderRead(handleGetOrder(log, a)))
	mux.Handle("POST /api/v1/orders/orders/{orderId}/token-payment", orderManage(handleRecordTokenPayment(log, a)))
	mux.Handle("POST /api/v1/orders/orders/{orderId}/confirm", orderManage(handleConfirmOrder(log, a)))
	mux.Handle("POST /api/v1/orders/orders/{orderId}/pack", orderManage(handlePackOrder(log, a)))
	mux.Handle("POST /api/v1/orders/orders/{orderId}/invoice", orderManage(handleInvoiceOrder(log, a)))
	mux.Handle("POST /api/v1/orders/orders/{orderId}/cancel", orderManage(handleCancelOrder(log, a)))
	mux.Handle("POST /api/v1/orders/orders/{orderId}/complete", orderManage(handleCompleteOrder(log, a)))
	mux.Handle("GET /api/v1/orders/orders/{orderId}/invoice", invoiceRead(handleGetInvoice(log, a)))
	mux.Handle("GET /api/v1/orders/orders/{orderId}/payments", orderRead(handleListPayments(log, a)))
	mux.Handle("POST /api/v1/orders/orders/{orderId}/payments", paymentRecord(handleRecordPayment(log, a)))
}

// ----- Quotation handlers ---------------------------------------------------

func handleCreateQuotation(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, tid, ok := actorTenant(w, r)
		if !ok {
			return
		}
		var req CreateQuotationRequest
		if !decode(w, r, &req) {
			return
		}
		id, err := a.Commands.CreateQuotation.Handle(r.Context(), command.CreateQuotationCommand{
			TenantID: tid, CustomerLeadID: req.CustomerLeadID,
			Items: toCmdItems(req.Items), Note: req.Note, CreatedByMembershipID: actor,
		})
		switch {
		case err == nil:
			writeJSON(w, http.StatusCreated, CreateQuotationResponse{QuotationID: id.String()})
		case errors.Is(err, quotation.ErrInvalid):
			writeError(w, http.StatusUnprocessableEntity, errCodeValidation, err.Error())
		default:
			internal(w, log, r, "create quotation", err)
		}
	})
}

func handleGetQuotation(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := tenantFromContext(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		id, ok := parseQuotationID(w, r)
		if !ok {
			return
		}
		v, err := a.Queries.GetQuotation.Handle(r.Context(), query.GetQuotationQuery{TenantID: tid, QuotationID: id})
		switch {
		case errors.Is(err, query.ErrQuotationNotFound):
			writeError(w, http.StatusNotFound, errCodeQuotationNotFound, "")
		case err != nil:
			internal(w, log, r, "get quotation", err)
		default:
			writeJSON(w, http.StatusOK, quotationToDto(*v))
		}
	})
}

func handleReviseQuotation(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, tid, id, ok := actorTenantQuotation(w, r)
		if !ok {
			return
		}
		var req ReviseQuotationRequest
		if !decode(w, r, &req) {
			return
		}
		err := a.Commands.ReviseQuotation.Handle(r.Context(), command.ReviseQuotationCommand{
			TenantID: tid, QuotationID: id, Items: toCmdItems(req.Items), Note: req.Note, RevisedByMembership: actor,
		})
		mapQuotationMutation(w, log, r, err, "revise quotation")
	})
}

func handleApproveQuotation(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, tid, id, ok := actorTenantQuotation(w, r)
		if !ok {
			return
		}
		res, err := a.Commands.ApproveQuotation.Handle(r.Context(), command.ApproveQuotationCommand{
			TenantID: tid, QuotationID: id, ApprovedByMembership: actor,
		})
		switch {
		case err == nil:
			writeJSON(w, http.StatusOK, ApproveQuotationResponse{OrderID: res.OrderID.String()})
		case errors.Is(err, quotation.ErrNotFound):
			writeError(w, http.StatusNotFound, errCodeQuotationNotFound, "")
		case errors.Is(err, quotation.ErrInvalidTransition):
			writeError(w, http.StatusConflict, errCodeInvalidTransition, err.Error())
		case errors.Is(err, quotation.ErrInvalid):
			writeError(w, http.StatusUnprocessableEntity, errCodeValidation, err.Error())
		default:
			internal(w, log, r, "approve quotation", err)
		}
	})
}

func handleRejectQuotation(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, tid, id, ok := actorTenantQuotation(w, r)
		if !ok {
			return
		}
		var req RejectQuotationRequest
		if !decode(w, r, &req) {
			return
		}
		if strings.TrimSpace(req.Reason) == "" {
			writeError(w, http.StatusBadRequest, errCodeReasonRequired, "reason is required")
			return
		}
		err := a.Commands.RejectQuotation.Handle(r.Context(), command.RejectQuotationCommand{
			TenantID: tid, QuotationID: id, Reason: req.Reason, RejectedByMembership: actor,
		})
		mapQuotationMutation(w, log, r, err, "reject quotation")
	})
}

// ----- Order handlers -------------------------------------------------------

func handleGetOrder(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := tenantFromContext(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		id, ok := parseOrderID(w, r)
		if !ok {
			return
		}
		v, err := a.Queries.GetOrder.Handle(r.Context(), query.GetOrderQuery{TenantID: tid, OrderID: id})
		switch {
		case errors.Is(err, query.ErrOrderNotFound):
			writeError(w, http.StatusNotFound, errCodeOrderNotFound, "")
		case err != nil:
			internal(w, log, r, "get order", err)
		default:
			writeJSON(w, http.StatusOK, orderToDto(*v))
		}
	})
}

func handleRecordTokenPayment(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, tid, id, ok := actorTenantOrder(w, r)
		if !ok {
			return
		}
		var req RecordTokenPaymentRequest
		if !decode(w, r, &req) {
			return
		}
		err := a.Commands.RecordTokenPayment.Handle(r.Context(), command.RecordTokenPaymentCommand{
			TenantID: tid, OrderID: id, Method: req.Method, AmountPaise: req.AmountPaise,
			ExternalReference: req.ExternalReference, Notes: req.Notes, RecordedByMembership: actor,
		})
		mapOrderMutation(w, log, r, err, "record token payment")
	})
}

func handleConfirmOrder(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, tid, id, ok := actorTenantOrder(w, r)
		if !ok {
			return
		}
		err := a.Commands.ConfirmOrder.Handle(r.Context(), command.ConfirmOrderCommand{
			TenantID: tid, OrderID: id, ConfirmedByMembership: actor,
		})
		mapOrderMutation(w, log, r, err, "confirm order")
	})
}

func handlePackOrder(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, tid, id, ok := actorTenantOrder(w, r)
		if !ok {
			return
		}
		var req PackOrderRequest
		if !decode(w, r, &req) {
			return
		}
		err := a.Commands.PackOrder.Handle(r.Context(), command.PackOrderCommand{
			TenantID: tid, OrderID: id, CarrierName: req.CarrierName, BoxCount: req.BoxCount,
			WeightGrams: req.WeightGrams, ExpectedDeliveryAt: req.ExpectedDeliveryAt, PackedByMembership: actor,
		})
		mapOrderMutation(w, log, r, err, "pack order")
	})
}

func handleInvoiceOrder(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, tid, id, ok := actorTenantOrder(w, r)
		if !ok {
			return
		}
		res, err := a.Commands.InvoiceOrder.Handle(r.Context(), command.InvoiceOrderCommand{
			TenantID: tid, OrderID: id, IssuedByMembership: actor,
		})
		switch {
		case err == nil:
			writeJSON(w, http.StatusOK, InvoiceOrderResponse{InvoiceID: res.InvoiceID.String(), NumberDisplay: res.NumberDisplay})
		case errors.Is(err, order.ErrNotFound):
			writeError(w, http.StatusNotFound, errCodeOrderNotFound, "")
		case errors.Is(err, invoice.ErrAlreadyExistsForOrder):
			writeError(w, http.StatusConflict, errCodeInvoiceConflict, "")
		case errors.Is(err, order.ErrInvalidTransition):
			writeError(w, http.StatusConflict, errCodeInvalidTransition, err.Error())
		default:
			internal(w, log, r, "invoice order", err)
		}
	})
}

func handleCancelOrder(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, tid, id, ok := actorTenantOrder(w, r)
		if !ok {
			return
		}
		var req CancelOrderRequest
		if !decode(w, r, &req) {
			return
		}
		if strings.TrimSpace(req.Reason) == "" {
			writeError(w, http.StatusBadRequest, errCodeReasonRequired, "reason is required")
			return
		}
		err := a.Commands.CancelOrder.Handle(r.Context(), command.CancelOrderCommand{
			TenantID: tid, OrderID: id, Reason: req.Reason, CancelledByMembership: actor,
		})
		mapOrderMutation(w, log, r, err, "cancel order")
	})
}

func handleCompleteOrder(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, tid, id, ok := actorTenantOrder(w, r)
		if !ok {
			return
		}
		err := a.Commands.CompleteOrder.Handle(r.Context(), command.CompleteOrderCommand{
			TenantID: tid, OrderID: id, TransitionedByMembership: actor,
		})
		mapOrderMutation(w, log, r, err, "complete order")
	})
}

func handleGetInvoice(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := tenantFromContext(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		id, ok := parseOrderID(w, r)
		if !ok {
			return
		}
		v, err := a.Queries.GetInvoiceByOrder.Handle(r.Context(), query.GetInvoiceByOrderQuery{TenantID: tid, OrderID: id})
		switch {
		case errors.Is(err, query.ErrInvoiceNotFound):
			writeError(w, http.StatusNotFound, errCodeInvoiceNotFound, "")
		case err != nil:
			internal(w, log, r, "get invoice", err)
		default:
			writeJSON(w, http.StatusOK, invoiceToDto(*v))
		}
	})
}

func handleListPayments(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := tenantFromContext(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		id, ok := parseOrderID(w, r)
		if !ok {
			return
		}
		vs, err := a.Queries.ListPaymentsByOrder.Handle(r.Context(), query.ListPaymentsByOrderQuery{TenantID: tid, OrderID: id})
		if err != nil {
			internal(w, log, r, "list payments", err)
			return
		}
		out := ListPaymentsResponse{Payments: make([]PaymentDto, 0, len(vs))}
		for _, v := range vs {
			out.Payments = append(out.Payments, paymentToDto(v))
		}
		writeJSON(w, http.StatusOK, out)
	})
}

func handleRecordPayment(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, tid, id, ok := actorTenantOrder(w, r)
		if !ok {
			return
		}
		var req RecordPaymentRequest
		if !decode(w, r, &req) {
			return
		}
		res, err := a.Commands.RecordPayment.Handle(r.Context(), command.RecordPaymentCommand{
			TenantID: tid, OrderID: id, Kind: req.Kind, Method: req.Method, AmountPaise: req.AmountPaise,
			ExternalReference: req.ExternalReference, Notes: req.Notes, ReceivedAt: derefTime(req.ReceivedAt), RecordedByMembership: actor,
		})
		switch {
		case err == nil:
			writeJSON(w, http.StatusCreated, RecordPaymentResponse{PaymentID: res.PaymentID.String()})
		case errors.Is(err, payment.ErrAlreadyExistsForExternalReference):
			writeError(w, http.StatusConflict, errCodePaymentConflict, "")
		case errors.Is(err, payment.ErrInvalid):
			writeError(w, http.StatusUnprocessableEntity, errCodeValidation, err.Error())
		default:
			internal(w, log, r, "record payment", err)
		}
	})
}
