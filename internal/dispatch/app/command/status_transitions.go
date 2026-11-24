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

// Status-transition handlers are single-aggregate UpdateFn commands (TDL
// canon): each loads → mutates → persists ONE ConsignmentNote via
// notes.UpdateByID, which already owns its transaction (repository-owns-tx,
// ADR 0004). No pg.UnitOfWork wrapper — a WithinTx around a single
// UpdateByID is redundant ceremony (ADR 0067 UoW audit). The multi-write
// CreateConsignmentNote command keeps the UoW, and so does MarkDispatched,
// which moves stock, order and note in one transaction (ADR 0072).

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
// invoice. Goods do not leave without one (review G2d B1).
var ErrOrderNotInvoiced = errors.New("dispatch: the order has no tax invoice")

// ErrOrderNotDispatchable is returned when the order has an invoice but is in a state goods
// do not leave from: a cancelled order keeps its invoice and is never shipped (review G2e E2).
var ErrOrderNotDispatchable = errors.New("dispatch: the order is not in a state goods can leave from")

// ErrStockExpired is returned when a batch the order holds is past its printed expiry: the
// goods are still in the warehouse and expired stock is never sold (review G2e E1).
var ErrStockExpired = errors.New("dispatch: the goods of the order are past their printed expiry")

// ErrStockNotHeld is returned when the order holds no stock at the moment goods leave for it:
// goods never leave without a deduction (ADR 0072).
var ErrStockNotHeld = errors.New("dispatch: the order holds no stock to ship")

// OrderShipper is Dispatch's port to Orders for the moment goods leave: it moves the order
// from invoiced to dispatched. The composition root binds it to an Orders command, because
// modules never import each other.
//
// It runs INSIDE the dispatch transaction and must use the one in ctx (it is called with the
// ctx [TenantUnitOfWork.WithinTenantTx] hands the closure). An implementation that cannot
// find a transaction in ctx must fail closed instead of opening its own: a second
// transaction would not roll back with the stock and the note. It answers
// [ErrOrderNotInvoiced] for an order with no invoice (or none for this company) and
// [ErrOrderNotDispatchable] for one in a state goods do not leave from.
type OrderShipper interface {
	MarkOrderDispatched(ctx context.Context, tenantID tenant.ID, orderID, consignmentNoteID string, actor membership.ID) error
}

// StockShipper is Dispatch's port to Inventory for the moment goods leave: it locks the
// batches the order holds, reads the hold under the locks and ships it (release + outbound,
// both movement rows). Same transaction rule as [OrderShipper]. It answers
// [ErrStockNotHeld] when the order holds nothing and [ErrStockExpired] when a held batch is
// past its printed expiry on the company business day; one expiry authority (Inventory's
// batch rule), not a second check here.
type StockShipper interface {
	ShipOrderStock(ctx context.Context, tenantID tenant.ID, orderID string, actor membership.ID) error
}

// MarkDispatchedHandler runs the pending → dispatched transition.
type MarkDispatchedHandler struct {
	uow    pg.TenantUnitOfWork
	notes  consignmentnote.Repository
	orders OrderShipper
	stock  StockShipper
	now    func() time.Time
}

// NewMarkDispatchedHandler wires the handler. uow, orders and stock are required.
func NewMarkDispatchedHandler(uow pg.TenantUnitOfWork, notes consignmentnote.Repository, orders OrderShipper, stock StockShipper, now func() time.Time) MarkDispatchedHandler {
	if uow == nil || orders == nil || stock == nil {
		panic("command: NewMarkDispatchedHandler uow, order and stock steps required")
	}
	if now == nil {
		now = time.Now
	}
	return MarkDispatchedHandler{uow: uow, notes: notes, orders: orders, stock: stock, now: now}
}

// Handle dispatches the note in ONE tenant-scoped transaction (ADR 0072), in this lock order:
//
//  1. lock the note (a note already past pending touches nothing: the same docket is a
//     no-op, another docket is the domain's refusal);
//  2. lock the order and move it invoiced → dispatched, its event in the outbox (not
//     invoiced / wrong state: 409);
//  3. lock the order's batches in batch-id order, read the hold under the locks, refuse when
//     nothing is held or a held batch is past its printed expiry, ship every hold (release +
//     outbound, both movement rows);
//  4. note pending → dispatched, its event in the outbox.
//
// It commits once. Any error rolls back all of it: stock, order and note change together or
// not at all. The steps run inside the note's UpdateByID closure so the note's row lock is the
// first lock taken; every repository joins the transaction in ctx, so the whole flow runs on
// one connection.
func (h MarkDispatchedHandler) Handle(ctx context.Context, cmd MarkDispatchedCommand) error {
	return h.uow.WithinTenantTx(ctx, cmd.TenantID.String(), func(ctx context.Context) error {
		return h.notes.UpdateByID(ctx, cmd.TenantID, cmd.ConsignmentNoteID, func(cn *consignmentnote.ConsignmentNote) (bool, error) {
			priorStatus := cn.Status()
			if priorStatus == consignmentnote.StatusPending {
				// A docket the note will refuse is refused before the order or the stock move.
				if err := consignmentnote.ValidateDocketNumber(cmd.DocketNumber); err != nil {
					return false, fmt.Errorf("mark dispatched: %w", err)
				}
				orderID := string(cn.OrderID())
				if err := h.orders.MarkOrderDispatched(ctx, cmd.TenantID, orderID, cmd.ConsignmentNoteID.String(), cmd.TransitionedByMembership); err != nil {
					return false, fmt.Errorf("mark dispatched: %w", err)
				}
				if err := h.stock.ShipOrderStock(ctx, cmd.TenantID, orderID, cmd.TransitionedByMembership); err != nil {
					return false, fmt.Errorf("mark dispatched: %w", err)
				}
			}
			// The note moves last: a refusal by the note's own rules (no docket, a note that
			// cannot be dispatched) after the order and the stock moved rolls them back too.
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
