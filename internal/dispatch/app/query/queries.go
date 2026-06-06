// Package query holds Dispatch read-side handlers per TDL canon. Read
// only — no state mutation. Strict CQRS (ADR 0067): handlers return flat
// *View read models, never the ConsignmentNote aggregate; the port maps a
// View straight to its wire DTO.
package query

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ErrConsignmentNoteNotFound surfaces when no note exists in the
// caller's tenant scope for the requested id / order id.
var ErrConsignmentNoteNotFound = errors.New("dispatch query: consignment note not found")

// ConsignmentNoteView is the read-side projection of a ConsignmentNote
// aggregate.
type ConsignmentNoteView struct {
	ID                    string
	TenantID              string
	OrderID               string
	Status                string
	CarrierName           string
	DocketNumber          string
	BoxCount              int32
	WeightGrams           int64
	ExpectedDeliveryAt    *time.Time
	DispatchedAt          *time.Time
	InTransitAt           *time.Time
	DeliveredAt           *time.Time
	FailedAt              *time.Time
	FailureReason         string
	CreatedAt             time.Time
	CreatedByMembershipID string
}

// newConsignmentNoteView projects an aggregate into its read model.
func newConsignmentNoteView(cn *consignmentnote.ConsignmentNote) ConsignmentNoteView {
	return ConsignmentNoteView{
		ID:                    cn.ID().String(),
		TenantID:              cn.TenantID().String(),
		OrderID:               cn.OrderID().String(),
		Status:                cn.Status().String(),
		CarrierName:           cn.CarrierName(),
		DocketNumber:          cn.DocketNumber(),
		BoxCount:              cn.BoxCount(),
		WeightGrams:           cn.WeightGrams(),
		ExpectedDeliveryAt:    cn.ExpectedDeliveryAt(),
		DispatchedAt:          cn.DispatchedAt(),
		InTransitAt:           cn.InTransitAt(),
		DeliveredAt:           cn.DeliveredAt(),
		FailedAt:              cn.FailedAt(),
		FailureReason:         cn.FailureReason(),
		CreatedAt:             cn.CreatedAt(),
		CreatedByMembershipID: cn.CreatedByMembershipID().String(),
	}
}

// ----- GetConsignmentNote --------------------------------------------------

// GetConsignmentNoteQuery selects a single note by ID under tenant scope.
type GetConsignmentNoteQuery struct {
	TenantID          tenant.ID
	ConsignmentNoteID consignmentnote.ID
}

// GetConsignmentNoteHandler runs the by-id read.
type GetConsignmentNoteHandler struct {
	notes consignmentnote.Repository
}

// NewGetConsignmentNoteHandler wires the handler.
func NewGetConsignmentNoteHandler(notes consignmentnote.Repository) GetConsignmentNoteHandler {
	if notes == nil {
		panic("query: NewGetConsignmentNoteHandler notes required")
	}
	return GetConsignmentNoteHandler{notes: notes}
}

// Handle returns the read model or [ErrConsignmentNoteNotFound].
func (h GetConsignmentNoteHandler) Handle(ctx context.Context, q GetConsignmentNoteQuery) (*ConsignmentNoteView, error) {
	if q.TenantID == "" {
		return nil, errors.New("dispatch get_consignment_note: tenant id required")
	}
	if q.ConsignmentNoteID.IsZero() {
		return nil, errors.New("dispatch get_consignment_note: consignment note id required")
	}
	cn, err := h.notes.GetByID(ctx, q.TenantID, q.ConsignmentNoteID)
	if err != nil {
		if errors.Is(err, consignmentnote.ErrNotFound) {
			return nil, ErrConsignmentNoteNotFound
		}
		return nil, fmt.Errorf("dispatch get_consignment_note: %w", err)
	}
	view := newConsignmentNoteView(cn)
	return &view, nil
}

// ----- GetConsignmentNoteByOrder -------------------------------------------

// GetConsignmentNoteByOrderQuery selects the (zero or one) note attached
// to an order under tenant scope.
type GetConsignmentNoteByOrderQuery struct {
	TenantID tenant.ID
	OrderID  consignmentnote.OrderID
}

// GetConsignmentNoteByOrderHandler runs the by-order read.
type GetConsignmentNoteByOrderHandler struct {
	notes consignmentnote.Repository
}

// NewGetConsignmentNoteByOrderHandler wires the handler.
func NewGetConsignmentNoteByOrderHandler(notes consignmentnote.Repository) GetConsignmentNoteByOrderHandler {
	if notes == nil {
		panic("query: NewGetConsignmentNoteByOrderHandler notes required")
	}
	return GetConsignmentNoteByOrderHandler{notes: notes}
}

// Handle returns the read model or [ErrConsignmentNoteNotFound].
func (h GetConsignmentNoteByOrderHandler) Handle(ctx context.Context, q GetConsignmentNoteByOrderQuery) (*ConsignmentNoteView, error) {
	if q.TenantID == "" {
		return nil, errors.New("dispatch get_consignment_note_by_order: tenant id required")
	}
	if q.OrderID.IsZero() {
		return nil, errors.New("dispatch get_consignment_note_by_order: order id required")
	}
	cn, err := h.notes.GetByOrderID(ctx, q.TenantID, q.OrderID)
	if err != nil {
		if errors.Is(err, consignmentnote.ErrNotFound) {
			return nil, ErrConsignmentNoteNotFound
		}
		return nil, fmt.Errorf("dispatch get_consignment_note_by_order: %w", err)
	}
	view := newConsignmentNoteView(cn)
	return &view, nil
}
