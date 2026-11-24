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
// invoice. Goods do not leave without one (review G2d B1).
var ErrOrderNotInvoiced = errors.New("dispatch: the order has no tax invoice")

// ErrOrderNotDispatchable is returned when the order has an invoice but is in a state goods
// do not leave from: a cancelled order keeps its invoice and is never shipped (review G2e E2).
var ErrOrderNotDispatchable = errors.New("dispatch: the order is not in a state goods can leave from")

// ErrStockExpired is returned when a batch the order reserved is past its printed expiry:
// the goods are still in the warehouse and expired stock is never sold (review G2e E1).
var ErrStockExpired = errors.New("dispatch: the goods of the order are past their printed expiry")

// OrderDispatchStatus is Orders' answer to "may goods leave for this order?".
type OrderDispatchStatus int

const (
	// OrderDispatchable means the order is invoiced or dispatched.
	OrderDispatchable OrderDispatchStatus = iota
	// OrderNotInvoiced means the order has no tax invoice (or there is no such order for this company).
	OrderNotInvoiced
	// OrderWrongState means the order is cancelled or past dispatch.
	OrderWrongState
)

// OrderDispatchChecker asks Orders whether goods may leave for an order. Dispatch owns the
// question; the composition root binds it to an Orders read, because modules never import
// each other.
type OrderDispatchChecker interface {
	OrderDispatchStatus(ctx context.Context, tenantID tenant.ID, orderID string) (OrderDispatchStatus, error)
}

// StockExpiryChecker asks Inventory whether any batch the order ever reserved is past its
// printed expiry on the company business day. The reservation counts even after the consume
// drained it: the goods stay in the warehouse until the carrier takes them.
type StockExpiryChecker interface {
	ReservedStockExpired(ctx context.Context, tenantID tenant.ID, orderID string) (bool, error)
}

// MarkDispatchedHandler runs the pending → dispatched transition.
type MarkDispatchedHandler struct {
	notes  consignmentnote.Repository
	orders OrderDispatchChecker
	stock  StockExpiryChecker
	now    func() time.Time
}

// NewMarkDispatchedHandler wires the handler. orders and stock are required.
func NewMarkDispatchedHandler(notes consignmentnote.Repository, orders OrderDispatchChecker, stock StockExpiryChecker, now func() time.Time) MarkDispatchedHandler {
	if orders == nil || stock == nil {
		panic("command: NewMarkDispatchedHandler order and stock checks required")
	}
	if now == nil {
		now = time.Now
	}
	return MarkDispatchedHandler{notes: notes, orders: orders, stock: stock, now: now}
}

// Handle executes the transition via the UpdateFn pattern. While the note is pending it
// first asks Orders whether goods may leave for the order (invoiced, and in a state goods
// leave from) and Inventory whether a batch the order reserved is now past its printed
// expiry; either refusal leaves the note pending, and a read that fails fails closed. The
// questions are asked outside the note's transaction (other modules' reads). A note already
// past pending is not re-judged: its goods have left.
func (h MarkDispatchedHandler) Handle(ctx context.Context, cmd MarkDispatchedCommand) error {
	cn, err := h.notes.GetByID(ctx, cmd.TenantID, cmd.ConsignmentNoteID)
	if err != nil {
		return err
	}
	if cn.Status() == consignmentnote.StatusPending {
		orderID := string(cn.OrderID())
		status, err := h.orders.OrderDispatchStatus(ctx, cmd.TenantID, orderID)
		if err != nil {
			return fmt.Errorf("mark dispatched: check the order: %w", err)
		}
		switch status {
		case OrderDispatchable:
		case OrderNotInvoiced:
			return fmt.Errorf("mark dispatched: %w", ErrOrderNotInvoiced)
		default:
			return fmt.Errorf("mark dispatched: %w", ErrOrderNotDispatchable)
		}
		expired, err := h.stock.ReservedStockExpired(ctx, cmd.TenantID, orderID)
		if err != nil {
			return fmt.Errorf("mark dispatched: check the stock expiry: %w", err)
		}
		if expired {
			return fmt.Errorf("mark dispatched: %w", ErrStockExpired)
		}
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
