package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/domain/creditnote"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoice"
	"github.com/leadkart/leadkart-go/internal/orders/domain/invoicenumber"
	"github.com/leadkart/leadkart-go/internal/orders/domain/order"
)

// CompensateOrderCancellationCommand is the saga compensation for an order
// cancelled at/after invoicing (ADR 0063 §4): mint a cancellation note that
// financially reverses the invoice. Driven by the OrderCancelled subscriber.
type CompensateOrderCancellationCommand struct {
	TenantID           tenant.ID
	OrderID            order.ID
	PriorState         order.State
	Reason             string
	IssuedByMembership membership.ID
}

// CompensateOrderCancellationHandler looks up the order's invoice and mints a
// cancellation note for its grand total. Pre-invoice cancellations are a
// financial no-op (nothing issued yet). Post-delivery cancellations are NOT
// auto-reversed: per BRD §A-014 a post-delivery return is an operator-driven
// credit note (amount may be partial) — the subscriber logs and acks.
type CompensateOrderCancellationHandler struct {
	invoices invoice.Repository
	mint     MintCreditNoteHandler
}

// NewCompensateOrderCancellationHandler wires the handler.
func NewCompensateOrderCancellationHandler(invoices invoice.Repository, mint MintCreditNoteHandler) CompensateOrderCancellationHandler {
	return CompensateOrderCancellationHandler{invoices: invoices, mint: mint}
}

// CompensateOrderCancellationResult reports whether a note was minted this
// call (false on replays and on prior states needing no reversal).
type CompensateOrderCancellationResult struct {
	CreditNoteID creditnote.ID
	Minted       bool
}

// Handle mints the cancellation note. Idempotent: a replay surfaces
// [creditnote.ErrCancellationAlreadyExists] from the partial unique index and
// is treated as success (Minted=false).
func (h CompensateOrderCancellationHandler) Handle(ctx context.Context, cmd CompensateOrderCancellationCommand) (CompensateOrderCancellationResult, error) {
	if cmd.PriorState != order.StateInvoiced && cmd.PriorState != order.StateDispatched {
		// Pre-invoice cancel (nothing issued) or post-delivery cancel
		// (operator-driven return) — no automatic reversal.
		return CompensateOrderCancellationResult{}, nil
	}
	inv, err := h.invoices.GetByOrderID(ctx, cmd.TenantID, cmd.OrderID)
	if err != nil {
		// invoiced/dispatched implies the invoice row exists; retry until
		// visible (at-least-once redelivery handles transient lag).
		return CompensateOrderCancellationResult{}, fmt.Errorf("orders compensate_cancellation: load invoice: %w", err)
	}
	res, err := h.mint.Handle(ctx, MintCreditNoteCommand{
		TenantID:           cmd.TenantID,
		InvoiceID:          inv.ID(),
		Kind:               invoicenumber.KindCancellationNote.String(),
		Reason:             "order cancelled: " + cmd.Reason,
		AmountPaise:        inv.GrandTotalPaise(),
		IssuedByMembership: cmd.IssuedByMembership,
	})
	if err != nil {
		if errors.Is(err, creditnote.ErrCancellationAlreadyExists) {
			return CompensateOrderCancellationResult{}, nil // replay — note already minted
		}
		return CompensateOrderCancellationResult{}, fmt.Errorf("orders compensate_cancellation: %w", err)
	}
	return CompensateOrderCancellationResult{CreditNoteID: res.CreditNoteID, Minted: true}, nil
}

// MintCreditNoteCommand allocates a gapless credit-note / cancellation-note
// number and mints the CreditNote against an invoice — one UoW tx (number +
// row commit together). Cancellation notes are minted by the order-cancel
// compensation subscriber (pre-delivery); credit notes by post-delivery
// returns (ADR 0063 §4, BRD §A-014).
type MintCreditNoteCommand struct {
	TenantID           tenant.ID
	InvoiceID          invoice.ID
	Kind               string // "credit_note" | "cancellation_note"
	Reason             string
	AmountPaise        int64
	IssuedByMembership membership.ID
}

// MintCreditNoteResult returns the new credit-note ID + display number.
type MintCreditNoteResult struct {
	CreditNoteID  creditnote.ID
	NumberDisplay string
}

// MintCreditNoteHandler runs the credit-note minting flow.
type MintCreditNoteHandler struct {
	uow             pg.UnitOfWork
	creditNotes     creditnote.Repository
	allocator       invoicenumber.Allocator
	now             func() time.Time
	newCreditNoteID func() creditnote.ID
}

// NewMintCreditNoteHandler wires the handler.
func NewMintCreditNoteHandler(
	uow pg.UnitOfWork, creditNotes creditnote.Repository, allocator invoicenumber.Allocator,
	now func() time.Time, newCreditNoteID func() creditnote.ID,
) MintCreditNoteHandler {
	if now == nil {
		now = time.Now
	}
	return MintCreditNoteHandler{uow: uow, creditNotes: creditNotes, allocator: allocator, now: now, newCreditNoteID: newCreditNoteID}
}

// Handle mints the credit note atomically. Returns
// [creditnote.ErrCancellationAlreadyExists] when a cancellation note already
// exists for the invoice (idempotent compensation replay).
func (h MintCreditNoteHandler) Handle(ctx context.Context, cmd MintCreditNoteCommand) (MintCreditNoteResult, error) {
	kind := invoicenumber.Kind(cmd.Kind)
	if kind != invoicenumber.KindCreditNote && kind != invoicenumber.KindCancellationNote {
		return MintCreditNoteResult{}, fmt.Errorf("orders mint_credit_note: kind %q must be credit_note or cancellation_note", cmd.Kind)
	}
	if cmd.TenantID == "" {
		return MintCreditNoteResult{}, errors.New("orders mint_credit_note: tenant id required")
	}
	var result MintCreditNoteResult
	err := h.uow.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context) error {
		now := h.now().UTC()
		num, err := h.allocator.Allocate(ctx, cmd.TenantID, invoicenumber.FromDate(now), kind)
		if err != nil {
			return fmt.Errorf("allocate number: %w", err)
		}
		cn, err := creditnote.New(creditnote.NewInput{
			ID:                 h.newCreditNoteID(),
			TenantID:           cmd.TenantID,
			InvoiceID:          cmd.InvoiceID,
			Number:             num,
			Kind:               kind,
			Reason:             cmd.Reason,
			AmountPaise:        cmd.AmountPaise,
			IssuedAt:           now,
			IssuedByMembership: cmd.IssuedByMembership,
		})
		if err != nil {
			return fmt.Errorf("construct credit note: %w", err)
		}
		if err := h.creditNotes.Add(ctx, cn); err != nil {
			return fmt.Errorf("add credit note: %w", err)
		}
		result = MintCreditNoteResult{CreditNoteID: cn.ID(), NumberDisplay: num.String()}
		return nil
	})
	if err != nil {
		return MintCreditNoteResult{}, fmt.Errorf("orders mint_credit_note: %w", err)
	}
	return result, nil
}
