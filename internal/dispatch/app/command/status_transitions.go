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

// ----- MarkDispatched -------------------------------------------------------

// MarkDispatchedCommand drives the pending → dispatched transition.
// DocketNumber is required (the transition exists to capture it).
type MarkDispatchedCommand struct {
	TenantID                 tenant.ID
	ConsignmentNoteID        consignmentnote.ID
	DocketNumber             string
	TransitionedByMembership membership.ID
}

// MarkDispatchedHandler runs the pending → dispatched transition.
type MarkDispatchedHandler struct {
	uow   pg.UnitOfWork
	notes consignmentnote.Repository
	now   func() time.Time
}

// NewMarkDispatchedHandler wires the handler.
func NewMarkDispatchedHandler(uow pg.UnitOfWork, notes consignmentnote.Repository, now func() time.Time) MarkDispatchedHandler {
	if now == nil {
		now = time.Now
	}
	return MarkDispatchedHandler{uow: uow, notes: notes, now: now}
}

// Handle executes the transition inside a UoW tx.
func (h MarkDispatchedHandler) Handle(ctx context.Context, cmd MarkDispatchedCommand) error {
	return h.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		return h.notes.UpdateByID(ctx, cmd.TenantID, cmd.ConsignmentNoteID, func(cn *consignmentnote.ConsignmentNote) (bool, error) {
			priorStatus := cn.Status()
			if err := cn.MarkDispatched(cmd.DocketNumber, cmd.TransitionedByMembership, h.now()); err != nil {
				return false, fmt.Errorf("mark dispatched: %w", err)
			}
			// No-op when already-dispatched-with-same-docket (mutator
			// returns nil but doesn't change state).
			return cn.Status() != priorStatus, nil
		})
	})
}

// ----- MarkInTransit --------------------------------------------------------

// MarkInTransitCommand drives the dispatched → in_transit transition.
type MarkInTransitCommand struct {
	TenantID                 tenant.ID
	ConsignmentNoteID        consignmentnote.ID
	TransitionedByMembership membership.ID
}

// MarkInTransitHandler runs the dispatched → in_transit transition.
type MarkInTransitHandler struct {
	uow   pg.UnitOfWork
	notes consignmentnote.Repository
	now   func() time.Time
}

// NewMarkInTransitHandler wires the handler.
func NewMarkInTransitHandler(uow pg.UnitOfWork, notes consignmentnote.Repository, now func() time.Time) MarkInTransitHandler {
	if now == nil {
		now = time.Now
	}
	return MarkInTransitHandler{uow: uow, notes: notes, now: now}
}

// Handle executes the transition.
func (h MarkInTransitHandler) Handle(ctx context.Context, cmd MarkInTransitCommand) error {
	return h.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		return h.notes.UpdateByID(ctx, cmd.TenantID, cmd.ConsignmentNoteID, func(cn *consignmentnote.ConsignmentNote) (bool, error) {
			priorStatus := cn.Status()
			if err := cn.MarkInTransit(cmd.TransitionedByMembership, h.now()); err != nil {
				return false, err
			}
			return cn.Status() != priorStatus, nil
		})
	})
}

// ----- MarkDelivered --------------------------------------------------------

// MarkDeliveredCommand drives the terminal-success transition. The
// emitted ConsignmentDeliveredV1 is the saga input the Orders module's
// subscriber consumes per ADR 0063 §4.
type MarkDeliveredCommand struct {
	TenantID                 tenant.ID
	ConsignmentNoteID        consignmentnote.ID
	TransitionedByMembership membership.ID
}

// MarkDeliveredHandler runs the terminal-success transition.
type MarkDeliveredHandler struct {
	uow   pg.UnitOfWork
	notes consignmentnote.Repository
	now   func() time.Time
}

// NewMarkDeliveredHandler wires the handler.
func NewMarkDeliveredHandler(uow pg.UnitOfWork, notes consignmentnote.Repository, now func() time.Time) MarkDeliveredHandler {
	if now == nil {
		now = time.Now
	}
	return MarkDeliveredHandler{uow: uow, notes: notes, now: now}
}

// Handle executes the transition.
func (h MarkDeliveredHandler) Handle(ctx context.Context, cmd MarkDeliveredCommand) error {
	return h.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		return h.notes.UpdateByID(ctx, cmd.TenantID, cmd.ConsignmentNoteID, func(cn *consignmentnote.ConsignmentNote) (bool, error) {
			priorStatus := cn.Status()
			if err := cn.MarkDelivered(cmd.TransitionedByMembership, h.now()); err != nil {
				return false, err
			}
			return cn.Status() != priorStatus, nil
		})
	})
}

// ----- MarkFailed -----------------------------------------------------------

// MarkFailedCommand drives the terminal-failure transition.
type MarkFailedCommand struct {
	TenantID                 tenant.ID
	ConsignmentNoteID        consignmentnote.ID
	Reason                   string
	TransitionedByMembership membership.ID
}

// MarkFailedHandler runs the terminal-failure transition.
type MarkFailedHandler struct {
	uow   pg.UnitOfWork
	notes consignmentnote.Repository
	now   func() time.Time
}

// NewMarkFailedHandler wires the handler.
func NewMarkFailedHandler(uow pg.UnitOfWork, notes consignmentnote.Repository, now func() time.Time) MarkFailedHandler {
	if now == nil {
		now = time.Now
	}
	return MarkFailedHandler{uow: uow, notes: notes, now: now}
}

// ErrTerminal is the sentinel surfaced when an upstream caller tries
// to fail/transition an already-terminal ConsignmentNote.
var ErrTerminal = errors.New("dispatch: consignment note is terminal")

// Handle executes the transition.
func (h MarkFailedHandler) Handle(ctx context.Context, cmd MarkFailedCommand) error {
	return h.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		return h.notes.UpdateByID(ctx, cmd.TenantID, cmd.ConsignmentNoteID, func(cn *consignmentnote.ConsignmentNote) (bool, error) {
			priorStatus := cn.Status()
			if err := cn.MarkFailed(cmd.Reason, cmd.TransitionedByMembership, h.now()); err != nil {
				return false, err
			}
			return cn.Status() != priorStatus, nil
		})
	})
}
