package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/parcel"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
	"github.com/leadkart/leadkart-go/internal/orders/domain/payment"
	"github.com/leadkart/leadkart-go/internal/orders/integrationevents"
)

// ----- RecordTokenPayment (→ token_paid) ------------------------------------

// RecordTokenPaymentCommand records the upfront token receipt + advances the
// order quotation_approved → token_paid in one tx.
type RecordTokenPaymentCommand struct {
	TenantID             tenant.ID
	OrderID              order.ID
	Method               string
	AmountPaise          int64
	ExternalReference    string
	Notes                string
	RecordedByMembership membership.ID
}

// RecordTokenPaymentHandler runs the token-payment flow.
type RecordTokenPaymentHandler struct {
	uow          pg.UnitOfWork
	orders       order.Repository
	payments     payment.Repository
	now          func() time.Time
	newPaymentID func() payment.ID
}

// NewRecordTokenPaymentHandler wires the handler.
func NewRecordTokenPaymentHandler(
	uow pg.UnitOfWork, orders order.Repository, payments payment.Repository,
	now func() time.Time, newPaymentID func() payment.ID,
) RecordTokenPaymentHandler {
	if now == nil {
		now = time.Now
	}
	return RecordTokenPaymentHandler{uow: uow, orders: orders, payments: payments, now: now, newPaymentID: newPaymentID}
}

// Handle records the token payment + advances the order atomically.
func (h RecordTokenPaymentHandler) Handle(ctx context.Context, cmd RecordTokenPaymentCommand) error {
	return h.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		now := h.now().UTC()
		// Advance the order FIRST: UpdateByID row-locks it, so a repeated or
		// concurrent token payment sees token_paid and is refused before any
		// payment row is written (the transition alone is a self no-op, which
		// used to let a second receipt through).
		if err := h.orders.UpdateByID(ctx, cmd.TenantID, cmd.OrderID, func(o *order.Order) (bool, error) {
			if o.State() == order.StateTokenPaid {
				return false, order.ErrTokenAlreadyRecorded
			}
			if err := o.RecordTokenPayment(cmd.RecordedByMembership, now); err != nil {
				return false, fmt.Errorf("advance: %w", err)
			}
			return true, nil
		}); err != nil {
			return err
		}
		p, err := payment.New(payment.NewInput{
			ID:                   h.newPaymentID(),
			TenantID:             cmd.TenantID,
			OrderID:              cmd.OrderID,
			Kind:                 payment.KindToken,
			Method:               payment.Method(cmd.Method),
			AmountPaise:          cmd.AmountPaise,
			ExternalReference:    cmd.ExternalReference,
			Notes:                cmd.Notes,
			ReceivedAt:           now,
			RecordedAt:           now,
			RecordedByMembership: cmd.RecordedByMembership,
		})
		if err != nil {
			return fmt.Errorf("orders record_token_payment: construct payment: %w", err)
		}
		if err := h.payments.Add(ctx, p); err != nil {
			return fmt.Errorf("orders record_token_payment: add payment: %w", err)
		}
		return nil
	})
}

// ----- ConfirmOrder (→ confirmed, emits OrderConfirmedV1) -------------------

// ConfirmOrderCommand advances token_paid → confirmed + publishes the enriched
// OrderConfirmedV1 (line snapshot) for Inventory stock reservation.
type ConfirmOrderCommand struct {
	TenantID              tenant.ID
	OrderID               order.ID
	ConfirmedByMembership membership.ID
}

// ConfirmOrderHandler runs the confirm flow.
type ConfirmOrderHandler struct {
	uow      pg.UnitOfWork
	orders   order.Repository
	enqueuer OutboxEnqueuer
	now      func() time.Time
}

// NewConfirmOrderHandler wires the handler.
func NewConfirmOrderHandler(uow pg.UnitOfWork, orders order.Repository, enqueuer OutboxEnqueuer, now func() time.Time) ConfirmOrderHandler {
	if now == nil {
		now = time.Now
	}
	return ConfirmOrderHandler{uow: uow, orders: orders, enqueuer: enqueuer, now: now}
}

// Handle advances to confirmed + enqueues OrderConfirmedV1 in one tx.
func (h ConfirmOrderHandler) Handle(ctx context.Context, cmd ConfirmOrderCommand) error {
	return h.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		var ev integrationevents.OrderConfirmedV1
		if err := h.orders.UpdateByID(ctx, cmd.TenantID, cmd.OrderID, func(o *order.Order) (bool, error) {
			if err := o.Confirm(cmd.ConfirmedByMembership, h.now().UTC()); err != nil {
				return false, fmt.Errorf("confirm: %w", err)
			}
			ev = buildOrderConfirmed(o, cmd.ConfirmedByMembership, h.now().UTC())
			return true, nil
		}); err != nil {
			return err
		}
		return h.enqueuer.EnqueueInTx(ctx, ev)
	})
}

func buildOrderConfirmed(o *order.Order, actor membership.ID, now time.Time) integrationevents.OrderConfirmedV1 {
	items := o.ConfirmedItems()
	wire := make([]integrationevents.OrderLineItem, len(items))
	for i, li := range items {
		wire[i] = integrationevents.OrderLineItem{
			ProductID: li.ProductID, SKU: li.SKU, Quantity: li.Quantity,
			UnitSalePaise: li.UnitSalePaise, GstRateBps: li.GstRateBps,
		}
	}
	return integrationevents.OrderConfirmedV1{
		OrderID:                 o.ID().String(),
		TenantID:                o.TenantID().String(),
		CustomerLeadID:          o.CustomerLeadID().String(),
		Items:                   wire,
		GrandTotalPaise:         o.GrandTotalPaise(),
		ConfirmedAtUTC:          now,
		ConfirmedByMembershipID: actor.String(),
	}
}

// ----- PackOrder (→ packed, emits OrderPackedV1) ----------------------------

// PackOrderCommand advances confirmed → packed + publishes the enriched
// OrderPackedV1 (carrier logistics) for the Dispatch consignment-note slot.
type PackOrderCommand struct {
	TenantID           tenant.ID
	OrderID            order.ID
	CarrierName        string
	BoxCount           int32
	WeightGrams        int64
	ExpectedDeliveryAt *time.Time
	PackedByMembership membership.ID
}

// PackOrderHandler runs the pack flow (confirmed → packed) + emits
// OrderPackedV1. Invoicing is a SEPARATE step: the auto-invoice subscriber on
// orders.order_packed.v1 (default path) or the manual InvoiceOrder route runs
// InvoiceOrder, which advances packed → invoiced. The AttachConsignment saga
// handler tolerates the not-yet-invoiced window via retry (ADR 0063 §4).
type PackOrderHandler struct {
	uow      pg.UnitOfWork
	orders   order.Repository
	enqueuer OutboxEnqueuer
	stock    StockExpiryChecker
	now      func() time.Time
}

// NewPackOrderHandler wires the handler. stock is required: it is how Orders
// learns, without importing Inventory, that the order holds expired stock.
func NewPackOrderHandler(uow pg.UnitOfWork, orders order.Repository, enqueuer OutboxEnqueuer, stock StockExpiryChecker, now func() time.Time) PackOrderHandler {
	if stock == nil {
		panic("command: NewPackOrderHandler stock guard required")
	}
	if now == nil {
		now = time.Now
	}
	return PackOrderHandler{uow: uow, orders: orders, enqueuer: enqueuer, stock: stock, now: now}
}

// Handle advances to packed + enqueues OrderPackedV1 in one tx.
func (h PackOrderHandler) Handle(ctx context.Context, cmd PackOrderCommand) error {
	// The consignment note's own rules (common/parcel), checked BEFORE any
	// state change: a pack Dispatch would refuse must never reach `packed`.
	// ONE clock reading: the moment the rules are judged at is the PackedAtUTC
	// the event carries, and Dispatch judges the consignment note at that same
	// moment, so the two can never disagree about the delivery date.
	now := h.now().UTC()
	if err := parcel.Validate(parcel.Details{
		CarrierName: cmd.CarrierName, BoxCount: cmd.BoxCount,
		WeightGrams: cmd.WeightGrams, ExpectedDeliveryAt: cmd.ExpectedDeliveryAt,
	}, now); err != nil {
		return fmt.Errorf("orders pack_order: %w", err)
	}
	// Expired stock is never sold. The pack is what starts the invoice, the
	// consignment note and the stock consume, so it is refused here, before any
	// state change, when a batch the order holds is past its printed expiry on the
	// company's business day. Asked outside the transaction: the answer comes from
	// another module's read, and a second pooled connection taken inside a
	// transaction is the pool-deadlock shape.
	expired, err := h.stock.HoldsExpiredStock(ctx, cmd.TenantID, cmd.OrderID)
	if err != nil {
		return fmt.Errorf("orders pack_order: check held stock expiry: %w", err)
	}
	if expired {
		return fmt.Errorf("orders pack_order: %w", order.ErrStockExpired)
	}
	return h.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		if err := h.orders.UpdateByID(ctx, cmd.TenantID, cmd.OrderID, func(o *order.Order) (bool, error) {
			if err := o.MarkPacked(cmd.PackedByMembership, now); err != nil {
				return false, fmt.Errorf("pack: %w", err)
			}
			return true, nil
		}); err != nil {
			return err
		}
		var eta *time.Time
		if cmd.ExpectedDeliveryAt != nil {
			t := cmd.ExpectedDeliveryAt.UTC()
			eta = &t
		}
		return h.enqueuer.EnqueueInTx(ctx, integrationevents.OrderPackedV1{
			OrderID:               cmd.OrderID.String(),
			TenantID:              cmd.TenantID.String(),
			BoxCount:              cmd.BoxCount,
			WeightGrams:           cmd.WeightGrams,
			CarrierName:           cmd.CarrierName,
			ExpectedDeliveryAtUTC: eta,
			PackedAtUTC:           now,
			PackedByMembershipID:  cmd.PackedByMembership.String(),
		})
	})
}

// ----- AttachConsignment (links the note) -----------------------------------

// AttachConsignmentCommand links the Dispatch consignment-note id to the order. Saga-driven:
// the Dispatch consignment-note-created subscriber calls this once the slot exists (ADR 0063
// §4). It changes no state; the order is dispatched by [DispatchOrderHandler] when the goods
// leave.
type AttachConsignmentCommand struct {
	TenantID          tenant.ID
	OrderID           order.ID
	ConsignmentNoteID string
}

// AttachConsignmentHandler links the note.
type AttachConsignmentHandler struct {
	orders order.Repository
}

// NewAttachConsignmentHandler wires the handler.
func NewAttachConsignmentHandler(orders order.Repository) AttachConsignmentHandler {
	return AttachConsignmentHandler{orders: orders}
}

// Handle links the consignment; re-linking the same note persists nothing.
func (h AttachConsignmentHandler) Handle(ctx context.Context, cmd AttachConsignmentCommand) error {
	return h.orders.UpdateByID(ctx, cmd.TenantID, cmd.OrderID, func(o *order.Order) (bool, error) {
		prior := o.ConsignmentNoteID()
		if err := o.AttachConsignment(cmd.ConsignmentNoteID); err != nil {
			return false, fmt.Errorf("orders attach_consignment: %w", err)
		}
		return o.ConsignmentNoteID() != prior, nil
	})
}

// ----- DispatchOrder (invoiced → dispatched) --------------------------------

// ErrDispatchNotInvoiced is returned when the order has no tax invoice, or there is no such
// order for this company. Goods do not leave without one (review G2d B1).
var ErrDispatchNotInvoiced = errors.New("orders dispatch_order: the order has no tax invoice")

// ErrDispatchWrongState is returned when the order has an invoice but is in a state goods do
// not leave from: a cancelled order keeps its invoice and is never shipped (review G2e E2),
// and an order already dispatched or delivered is not dispatched again.
var ErrDispatchWrongState = errors.New("orders dispatch_order: the order is not in a state goods can leave from")

// DispatchOrderCommand moves an invoiced order to dispatched.
type DispatchOrderCommand struct {
	TenantID                 tenant.ID
	OrderID                  order.ID
	ConsignmentNoteID        string
	TransitionedByMembership membership.ID
}

// ConfirmedQuantity is the quantity of one product an order confirmed (its lines of the product
// summed).
type ConfirmedQuantity struct {
	ProductID string
	Quantity  int64
}

// DispatchOrderHandler runs the invoiced → dispatched transition as ONE STEP of Dispatch's
// dispatch transaction: it neither opens nor joins a transaction of its own, it uses the
// repository, which joins the one in ctx. The caller (the composition-root bridge) refuses a
// ctx that carries none, so the order cannot move without the stock and the note moving with
// it.
type DispatchOrderHandler struct {
	orders order.Repository
	now    func() time.Time
}

// NewDispatchOrderHandler wires the handler.
func NewDispatchOrderHandler(orders order.Repository, now func() time.Time) DispatchOrderHandler {
	if now == nil {
		now = time.Now
	}
	return DispatchOrderHandler{orders: orders, now: now}
}

// Handle locks the order, requires it to be invoiced and advances it to dispatched, the
// event riding the outbox of the same transaction. An order that is not found answers
// [ErrDispatchNotInvoiced] (another company's order looks the same as none).
func (h DispatchOrderHandler) Handle(ctx context.Context, cmd DispatchOrderCommand) error {
	_, err := h.Dispatch(ctx, cmd)
	return err
}

// Dispatch is [DispatchOrderHandler.Handle] and also returns what the order confirmed, per
// product, read from the same locked aggregate: Dispatch's stock step ships only a hold that
// equals it.
func (h DispatchOrderHandler) Dispatch(ctx context.Context, cmd DispatchOrderCommand) ([]ConfirmedQuantity, error) {
	var confirmed []ConfirmedQuantity
	err := h.orders.UpdateByID(ctx, cmd.TenantID, cmd.OrderID, func(o *order.Order) (bool, error) {
		switch {
		case o.State() == order.StateInvoiced:
		case o.State() == order.StateCancelled:
			return false, ErrDispatchWrongState
		case o.InvoiceID() == "":
			return false, ErrDispatchNotInvoiced
		default:
			return false, ErrDispatchWrongState
		}
		if err := o.MarkDispatched(cmd.TransitionedByMembership, h.now().UTC()); err != nil {
			return false, fmt.Errorf("orders dispatch_order: %w", err)
		}
		// The note is linked in the same step (a no-op when the async attach already did).
		if err := o.AttachConsignment(cmd.ConsignmentNoteID); err != nil {
			return false, fmt.Errorf("orders dispatch_order: %w", err)
		}
		byProduct := map[string]int64{}
		for _, it := range o.ConfirmedItems() {
			if _, seen := byProduct[it.ProductID]; !seen {
				confirmed = append(confirmed, ConfirmedQuantity{ProductID: it.ProductID})
			}
			byProduct[it.ProductID] += int64(it.Quantity)
		}
		for i := range confirmed {
			confirmed[i].Quantity = byProduct[confirmed[i].ProductID]
		}
		return true, nil
	})
	if errors.Is(err, order.ErrNotFound) {
		return nil, ErrDispatchNotInvoiced
	}
	if err != nil {
		return nil, err
	}
	return confirmed, nil
}

// ----- MarkDelivered / CompleteOrder / CancelOrder --------------------------

// MarkOrderDeliveredCommand advances dispatched → delivered. Saga-driven (the
// Dispatch consignment-delivered subscriber) but also operator-reachable.
type MarkOrderDeliveredCommand struct {
	TenantID                 tenant.ID
	OrderID                  order.ID
	TransitionedByMembership membership.ID
}

// MarkOrderDeliveredHandler runs the delivered transition.
type MarkOrderDeliveredHandler struct {
	orders order.Repository
	now    func() time.Time
}

// NewMarkOrderDeliveredHandler wires the handler.
func NewMarkOrderDeliveredHandler(orders order.Repository, now func() time.Time) MarkOrderDeliveredHandler {
	if now == nil {
		now = time.Now
	}
	return MarkOrderDeliveredHandler{orders: orders, now: now}
}

// Handle runs the delivered transition.
func (h MarkOrderDeliveredHandler) Handle(ctx context.Context, cmd MarkOrderDeliveredCommand) error {
	return h.orders.UpdateByID(ctx, cmd.TenantID, cmd.OrderID, func(o *order.Order) (bool, error) {
		prior := o.State()
		if err := o.MarkDelivered(cmd.TransitionedByMembership, h.now().UTC()); err != nil {
			return false, fmt.Errorf("orders mark_delivered: %w", err)
		}
		return o.State() != prior, nil
	})
}

// CompleteOrderCommand advances delivered → complete.
type CompleteOrderCommand struct {
	TenantID                 tenant.ID
	OrderID                  order.ID
	TransitionedByMembership membership.ID
}

// CompleteOrderHandler runs the complete transition.
type CompleteOrderHandler struct {
	orders order.Repository
	now    func() time.Time
}

// NewCompleteOrderHandler wires the handler.
func NewCompleteOrderHandler(orders order.Repository, now func() time.Time) CompleteOrderHandler {
	if now == nil {
		now = time.Now
	}
	return CompleteOrderHandler{orders: orders, now: now}
}

// Handle runs the complete transition.
func (h CompleteOrderHandler) Handle(ctx context.Context, cmd CompleteOrderCommand) error {
	return h.orders.UpdateByID(ctx, cmd.TenantID, cmd.OrderID, func(o *order.Order) (bool, error) {
		prior := o.State()
		if err := o.MarkComplete(cmd.TransitionedByMembership, h.now().UTC()); err != nil {
			return false, fmt.Errorf("orders complete_order: %w", err)
		}
		return o.State() != prior, nil
	})
}

// CancelOrderCommand cancels a non-terminal order; the drained
// OrderCancelledV1 fires the compensation subscribers (ADR 0063 §4).
type CancelOrderCommand struct {
	TenantID              tenant.ID
	OrderID               order.ID
	Reason                string
	CancelledByMembership membership.ID
}

// CancelOrderHandler runs the cancel transition.
type CancelOrderHandler struct {
	orders order.Repository
	now    func() time.Time
}

// NewCancelOrderHandler wires the handler.
func NewCancelOrderHandler(orders order.Repository, now func() time.Time) CancelOrderHandler {
	if now == nil {
		now = time.Now
	}
	return CancelOrderHandler{orders: orders, now: now}
}

// Handle runs the cancel transition.
func (h CancelOrderHandler) Handle(ctx context.Context, cmd CancelOrderCommand) error {
	return h.orders.UpdateByID(ctx, cmd.TenantID, cmd.OrderID, func(o *order.Order) (bool, error) {
		prior := o.State()
		if err := o.Cancel(cmd.Reason, cmd.CancelledByMembership, h.now().UTC()); err != nil {
			return false, fmt.Errorf("orders cancel_order: %w", err)
		}
		return o.State() != prior, nil
	})
}
