package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/dispatch/domain/consignmentnote"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Status-transition handlers are single-aggregate UpdateFn commands (TDL
// canon): each loads → mutates → persists ONE ConsignmentNote via
// notes.UpdateByID, which already owns its transaction (repository-owns-tx,
// ADR 0004). No pg.UnitOfWork wrapper — a WithinTx around a single
// UpdateByID is redundant ceremony (ADR 0067 UoW audit). The multi-write
// CreateConsignmentNote command keeps the UoW.

// ----- MarkDispatched -------------------------------------------------------

// MarkDispatchedCommand drives the pending → dispatched transition.
// DocketNumber is required (the transition exists to capture it).
type MarkDispatchedCommand struct {
	TenantID                 tenant.ID
	ConsignmentNoteID        consignmentnote.ID
	DocketNumber             string
	TransitionedByMembership membership.ID
}

// ErrOrderNotInvoiced is returned when a note is dispatched for an order that has no tax
// invoice. Goods do not leave without one, and an invoice is refused on expired stock, so
// this also stops expired stock leaving by hand (review G2d B1).
var ErrOrderNotInvoiced = errors.New("dispatch: the order has no tax invoice")

// OrderInvoiceChecker asks Orders whether an order has a tax invoice. Dispatch owns the
// question; the composition root binds it to an Orders read, because modules never import
// each other. An invoice, once issued, is never removed.
type OrderInvoiceChecker interface {
	OrderHasInvoice(ctx context.Context, tenantID tenant.ID, orderID string) (bool, error)
}

// MarkDispatchedHandler runs the pending → dispatched transition.
type MarkDispatchedHandler struct {
	notes    consignmentnote.Repository
	invoiced OrderInvoiceChecker
	now      func() time.Time
}

// NewMarkDispatchedHandler wires the handler. invoiced is required.
func NewMarkDispatchedHandler(notes consignmentnote.Repository, invoiced OrderInvoiceChecker, now func() time.Time) MarkDispatchedHandler {
	if invoiced == nil {
		panic("command: NewMarkDispatchedHandler invoice check required")
	}
	if now == nil {
		now = time.Now
	}
	return MarkDispatchedHandler{notes: notes, invoiced: invoiced, now: now}
}

// Handle executes the transition via the UpdateFn pattern, after Orders confirms the
// note's order has an invoice. The question is asked first and outside the note's
// transaction (another module's read); an invoice is never removed, so the answer cannot
// go stale. A read that fails fails closed.
func (h MarkDispatchedHandler) Handle(ctx context.Context, cmd MarkDispatchedCommand) error {
	cn, err := h.notes.GetByID(ctx, cmd.TenantID, cmd.ConsignmentNoteID)
	if err != nil {
		return err
	}
	has, err := h.invoiced.OrderHasInvoice(ctx, cmd.TenantID, string(cn.OrderID()))
	if err != nil {
		return fmt.Errorf("mark dispatched: check the order's invoice: %w", err)
	}
	if !has {
		return fmt.Errorf("mark dispatched: %w", ErrOrderNotInvoiced)
	}
	return h.notes.UpdateByID(ctx, cmd.TenantID, cmd.ConsignmentNoteID, func(cn *consignmentnote.ConsignmentNote) (bool, error) {
		priorStatus := cn.Status()
		if err := cn.MarkDispatched(cmd.DocketNumber, cmd.TransitionedByMembership, h.now()); err != nil {
			return false, fmt.Errorf("mark dispatched: %w", err)
		}
		// No-op when already-dispatched-with-same-docket (mutator
		// returns nil but doesn't change state).
		return cn.Status() != priorStatus, nil
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
	notes consignmentnote.Repository
	now   func() time.Time
}

// NewMarkInTransitHandler wires the handler.
func NewMarkInTransitHandler(notes consignmentnote.Repository, now func() time.Time) MarkInTransitHandler {
	if now == nil {
		now = time.Now
	}
	return MarkInTransitHandler{notes: notes, now: now}
}

// Handle executes the transition.
func (h MarkInTransitHandler) Handle(ctx context.Context, cmd MarkInTransitCommand) error {
	return h.notes.UpdateByID(ctx, cmd.TenantID, cmd.ConsignmentNoteID, func(cn *consignmentnote.ConsignmentNote) (bool, error) {
		priorStatus := cn.Status()
		if err := cn.MarkInTransit(cmd.TransitionedByMembership, h.now()); err != nil {
			return false, err
		}
		return cn.Status() != priorStatus, nil
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
	notes consignmentnote.Repository
	now   func() time.Time
}

// NewMarkDeliveredHandler wires the handler.
func NewMarkDeliveredHandler(notes consignmentnote.Repository, now func() time.Time) MarkDeliveredHandler {
	if now == nil {
		now = time.Now
	}
	return MarkDeliveredHandler{notes: notes, now: now}
}

// Handle executes the transition.
func (h MarkDeliveredHandler) Handle(ctx context.Context, cmd MarkDeliveredCommand) error {
	return h.notes.UpdateByID(ctx, cmd.TenantID, cmd.ConsignmentNoteID, func(cn *consignmentnote.ConsignmentNote) (bool, error) {
		priorStatus := cn.Status()
		if err := cn.MarkDelivered(cmd.TransitionedByMembership, h.now()); err != nil {
			return false, err
		}
		return cn.Status() != priorStatus, nil
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
	notes consignmentnote.Repository
	now   func() time.Time
}

// NewMarkFailedHandler wires the handler.
func NewMarkFailedHandler(notes consignmentnote.Repository, now func() time.Time) MarkFailedHandler {
	if now == nil {
		now = time.Now
	}
	return MarkFailedHandler{notes: notes, now: now}
}

// ErrTerminal is the sentinel surfaced when an upstream caller tries
// to fail/transition an already-terminal ConsignmentNote.
var ErrTerminal = errors.New("dispatch: consignment note is terminal")

// Handle executes the transition.
func (h MarkFailedHandler) Handle(ctx context.Context, cmd MarkFailedCommand) error {
	return h.notes.UpdateByID(ctx, cmd.TenantID, cmd.ConsignmentNoteID, func(cn *consignmentnote.ConsignmentNote) (bool, error) {
		priorStatus := cn.Status()
		if err := cn.MarkFailed(cmd.Reason, cmd.TransitionedByMembership, h.now()); err != nil {
			return false, err
		}
		return cn.Status() != priorStatus, nil
	})
}
