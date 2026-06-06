package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
	"github.com/leadkart/leadkart-go/internal/orders/domain/quotation"
)

// ----- CreateQuotation ------------------------------------------------------

// CreateQuotationLineItem is the app-level input for one quotation line.
type CreateQuotationLineItem struct {
	ProductID     string
	SKU           string
	Description   string
	Quantity      int32
	UnitMrpPaise  int64
	UnitSalePaise int64
	GstRateBps    int32
}

// CreateQuotationCommand drives a fresh draft quotation (revision 1).
type CreateQuotationCommand struct {
	TenantID              tenant.ID
	CustomerLeadID        string
	Items                 []CreateQuotationLineItem
	Note                  string
	CreatedByMembershipID membership.ID
}

// CreateQuotationHandler creates the draft.
type CreateQuotationHandler struct {
	quotations quotation.Repository
	now        func() time.Time
	newID      func() quotation.ID
}

// NewCreateQuotationHandler wires the handler.
func NewCreateQuotationHandler(quotations quotation.Repository, now func() time.Time, newID func() quotation.ID) CreateQuotationHandler {
	if now == nil {
		now = time.Now
	}
	return CreateQuotationHandler{quotations: quotations, now: now, newID: newID}
}

// Handle creates + persists the quotation, returning its new ID.
func (h CreateQuotationHandler) Handle(ctx context.Context, cmd CreateQuotationCommand) (quotation.ID, error) {
	if cmd.TenantID == "" {
		return "", errors.New("orders create_quotation: tenant id required")
	}
	q, err := quotation.New(quotation.NewInput{
		ID:                    h.newID(),
		TenantID:              cmd.TenantID,
		CustomerLeadID:        quotation.CustomerLeadID(cmd.CustomerLeadID),
		InitialItems:          toDomainLineItems(cmd.Items),
		InitialNote:           cmd.Note,
		CreatedByMembershipID: cmd.CreatedByMembershipID,
		Now:                   h.now().UTC(),
	})
	if err != nil {
		return "", fmt.Errorf("orders create_quotation: %w", err)
	}
	if err := h.quotations.Add(ctx, q); err != nil {
		return "", fmt.Errorf("orders create_quotation: %w", err)
	}
	return q.ID(), nil
}

// ----- ReviseQuotation ------------------------------------------------------

// ReviseQuotationCommand appends a new revision.
type ReviseQuotationCommand struct {
	TenantID            tenant.ID
	QuotationID         quotation.ID
	Items               []CreateQuotationLineItem
	Note                string
	RevisedByMembership membership.ID
}

// ReviseQuotationHandler runs the revise transition.
type ReviseQuotationHandler struct {
	quotations quotation.Repository
	now        func() time.Time
}

// NewReviseQuotationHandler wires the handler.
func NewReviseQuotationHandler(quotations quotation.Repository, now func() time.Time) ReviseQuotationHandler {
	if now == nil {
		now = time.Now
	}
	return ReviseQuotationHandler{quotations: quotations, now: now}
}

// Handle appends the revision via the UpdateFn.
func (h ReviseQuotationHandler) Handle(ctx context.Context, cmd ReviseQuotationCommand) error {
	return h.quotations.UpdateByID(ctx, cmd.TenantID, cmd.QuotationID, func(q *quotation.Quotation) (bool, error) {
		if err := q.Revise(quotation.ReviseInput{
			Items:               toDomainLineItems(cmd.Items),
			Note:                cmd.Note,
			RevisedByMembership: cmd.RevisedByMembership,
			Now:                 h.now().UTC(),
		}); err != nil {
			return false, fmt.Errorf("orders revise_quotation: %w", err)
		}
		return true, nil
	})
}

// ----- RejectQuotation ------------------------------------------------------

// RejectQuotationCommand drives the terminal-reject transition.
type RejectQuotationCommand struct {
	TenantID             tenant.ID
	QuotationID          quotation.ID
	Reason               string
	RejectedByMembership membership.ID
}

// RejectQuotationHandler runs the reject transition.
type RejectQuotationHandler struct {
	quotations quotation.Repository
	now        func() time.Time
}

// NewRejectQuotationHandler wires the handler.
func NewRejectQuotationHandler(quotations quotation.Repository, now func() time.Time) RejectQuotationHandler {
	if now == nil {
		now = time.Now
	}
	return RejectQuotationHandler{quotations: quotations, now: now}
}

// Handle runs the reject via the UpdateFn.
func (h RejectQuotationHandler) Handle(ctx context.Context, cmd RejectQuotationCommand) error {
	return h.quotations.UpdateByID(ctx, cmd.TenantID, cmd.QuotationID, func(q *quotation.Quotation) (bool, error) {
		if err := q.Reject(cmd.RejectedByMembership, cmd.Reason, h.now().UTC()); err != nil {
			return false, fmt.Errorf("orders reject_quotation: %w", err)
		}
		return true, nil
	})
}

// ----- ApproveQuotation (+ create Order) ------------------------------------

// ApproveQuotationCommand approves the quotation AND seeds the Order from the
// frozen revision — one atomic UoW tx (the Order exists iff the Quotation is
// approved, BRD §6.4).
type ApproveQuotationCommand struct {
	TenantID             tenant.ID
	QuotationID          quotation.ID
	ApprovedByMembership membership.ID
}

// ApproveQuotationResult returns the freshly-created Order's ID.
type ApproveQuotationResult struct {
	OrderID order.ID
}

// ApproveQuotationHandler approves + creates the order in one tx.
type ApproveQuotationHandler struct {
	uow        pg.UnitOfWork
	quotations quotation.Repository
	orders     order.Repository
	now        func() time.Time
	newOrderID func() order.ID
}

// NewApproveQuotationHandler wires the handler.
func NewApproveQuotationHandler(
	uow pg.UnitOfWork, quotations quotation.Repository, orders order.Repository,
	now func() time.Time, newOrderID func() order.ID,
) ApproveQuotationHandler {
	if now == nil {
		now = time.Now
	}
	return ApproveQuotationHandler{uow: uow, quotations: quotations, orders: orders, now: now, newOrderID: newOrderID}
}

// Handle approves the quotation + creates the Order atomically.
func (h ApproveQuotationHandler) Handle(ctx context.Context, cmd ApproveQuotationCommand) (ApproveQuotationResult, error) {
	if cmd.TenantID == "" {
		return ApproveQuotationResult{}, errors.New("orders approve_quotation: tenant id required")
	}
	var result ApproveQuotationResult
	err := h.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		var (
			items  []quotation.LineItem
			leadID quotation.CustomerLeadID
		)
		if err := h.quotations.UpdateByID(ctx, cmd.TenantID, cmd.QuotationID, func(q *quotation.Quotation) (bool, error) {
			if err := q.Approve(cmd.ApprovedByMembership, h.now().UTC()); err != nil {
				return false, fmt.Errorf("approve: %w", err)
			}
			items = q.CurrentRevision().Items
			leadID = q.CustomerLeadID()
			return true, nil
		}); err != nil {
			return err
		}
		o, err := order.New(order.NewInput{
			ID:                    h.newOrderID(),
			TenantID:              cmd.TenantID,
			ApprovedQuotationID:   cmd.QuotationID,
			CustomerLeadID:        leadID,
			ConfirmedItems:        items,
			CreatedByMembershipID: cmd.ApprovedByMembership,
			Now:                   h.now().UTC(),
		})
		if err != nil {
			return fmt.Errorf("construct order: %w", err)
		}
		if err := h.orders.Add(ctx, o); err != nil {
			return fmt.Errorf("add order: %w", err)
		}
		result.OrderID = o.ID()
		return nil
	})
	if err != nil {
		return ApproveQuotationResult{}, fmt.Errorf("orders approve_quotation: %w", err)
	}
	return result, nil
}

// toDomainLineItems maps app line-item inputs to domain LineItems.
func toDomainLineItems(in []CreateQuotationLineItem) []quotation.LineItem {
	out := make([]quotation.LineItem, len(in))
	for i, li := range in {
		out[i] = quotation.LineItem{
			ProductID: li.ProductID, SKU: li.SKU, Description: li.Description,
			Quantity: li.Quantity, UnitMrpPaise: li.UnitMrpPaise,
			UnitSalePaise: li.UnitSalePaise, GstRateBps: li.GstRateBps,
		}
	}
	return out
}
