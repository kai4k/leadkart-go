// Package command holds Dispatch CQRS command handlers.
//
// Per TDL Wild Workouts canonical layout: each handler is a concrete
// struct with a single Handle method. Handlers aggregate as fields on
// the [app.Application] facade; ports + subscribers call
// `app.Commands.X.Handle(...)`.
//
// Boundary discipline (ADR 0047): handlers depend on domain repository
// INTERFACES + a handful of cross-cutting infra interfaces
// (pg.UnitOfWork). No pgx, no pgxpool, no concrete adapter struct.
package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// CreateConsignmentNoteCommand drives a fresh pending ConsignmentNote
// slot creation. Typically called from the OrderPacked subscriber
// (one slot per packed order) but also reachable via a manual HTTP
// route for operator-driven creation.
type CreateConsignmentNoteCommand struct {
	TenantID              tenant.ID
	OrderID               consignmentnote.OrderID
	CarrierName           string
	BoxCount              int32
	WeightGrams           int64
	ExpectedDeliveryAt    *time.Time
	CreatedByMembershipID membership.ID
}

// CreateConsignmentNoteResult tells the subscriber whether the row
// already existed (idempotency hit on retry) or was freshly inserted.
type CreateConsignmentNoteResult struct {
	ConsignmentNoteID consignmentnote.ID
	AlreadyExisted    bool
}

// CreateConsignmentNoteHandler runs the create flow inside a UoW tx.
// Idempotent via the partial-unique invariant on (tenant_id, order_id)
// — replay returns AlreadyExisted=true.
type CreateConsignmentNoteHandler struct {
	uow   pg.UnitOfWork
	notes consignmentnote.Repository
	now   func() time.Time
	newID func() consignmentnote.ID
}

// NewCreateConsignmentNoteHandler wires the handler.
func NewCreateConsignmentNoteHandler(
	uow pg.UnitOfWork,
	notes consignmentnote.Repository,
	now func() time.Time,
	newID func() consignmentnote.ID,
) CreateConsignmentNoteHandler {
	if now == nil {
		now = time.Now
	}
	return CreateConsignmentNoteHandler{uow: uow, notes: notes, now: now, newID: newID}
}

// Handle executes the create flow.
func (h CreateConsignmentNoteHandler) Handle(
	ctx context.Context, cmd CreateConsignmentNoteCommand,
) (CreateConsignmentNoteResult, error) {
	if cmd.TenantID == "" {
		return CreateConsignmentNoteResult{}, errors.New("create consignment note: tenant_id required")
	}
	if cmd.OrderID.IsZero() {
		return CreateConsignmentNoteResult{}, errors.New("create consignment note: order_id required")
	}

	var result CreateConsignmentNoteResult
	err := h.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		// Natural-key precheck: a ConsignmentNote already exists for
		// this order → AlreadyExisted=true, no-op. Subscriber ACKs the
		// duplicate.
		existing, err := h.notes.GetByOrderID(ctx, cmd.TenantID, cmd.OrderID)
		switch {
		case err == nil:
			result.ConsignmentNoteID = existing.ID()
			result.AlreadyExisted = true
			return nil
		case errors.Is(err, consignmentnote.ErrNotFound):
			// fall through to insert
		default:
			return fmt.Errorf("load consignment note: %w", err)
		}

		cn, err := consignmentnote.New(consignmentnote.NewInput{
			ID:                    h.newID(),
			TenantID:              cmd.TenantID,
			OrderID:               cmd.OrderID,
			CarrierName:           cmd.CarrierName,
			BoxCount:              cmd.BoxCount,
			WeightGrams:           cmd.WeightGrams,
			ExpectedDeliveryAt:    cmd.ExpectedDeliveryAt,
			CreatedByMembershipID: cmd.CreatedByMembershipID,
			Now:                   h.now(),
		})
		if err != nil {
			return fmt.Errorf("construct: %w", err)
		}
		if err := h.notes.Add(ctx, cn); err != nil {
			// Concurrent insert won the race — re-load + treat as
			// "already existed" so the subscriber ACKs cleanly.
			if errors.Is(err, consignmentnote.ErrAlreadyExistsForOrder) {
				if existing, gerr := h.notes.GetByOrderID(ctx, cmd.TenantID, cmd.OrderID); gerr == nil {
					result.ConsignmentNoteID = existing.ID()
					result.AlreadyExisted = true
					return nil
				}
			}
			return fmt.Errorf("add: %w", err)
		}
		result.ConsignmentNoteID = cn.ID()
		return nil
	})
	if err != nil {
		return CreateConsignmentNoteResult{}, err
	}
	return result, nil
}
