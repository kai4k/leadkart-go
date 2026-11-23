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
	invoices    invoice.Repository
	creditNotes creditnote.Repository
	mint        MintCreditNoteHandler
}

// NewCompensateOrderCancellationHandler wires the handler.
func NewCompensateOrderCancellationHandler(invoices invoice.Repository, creditNotes creditnote.Repository, mint MintCreditNoteHandler) CompensateOrderCancellationHandler {
	return CompensateOrderCancellationHandler{invoices: invoices, creditNotes: creditNotes, mint: mint}
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
	// The note reverses what the invoice still stands for: its total less the
	// credit notes already issued (an operator return before the cancel). A
	// replay finds nothing left. A note minted between this read and the mint
	// below fails the mint's own cap check and the subscriber retries.
	existing, err := h.creditNotes.ListByInvoice(ctx, cmd.TenantID, inv.ID())
	if err != nil {
		return CompensateOrderCancellationResult{}, fmt.Errorf("orders compensate_cancellation: list notes: %w", err)
	}
	remaining := creditnote.RemainingPaise(inv.GrandTotalPaise(), existing)
	if remaining == 0 {
		return CompensateOrderCancellationResult{}, nil // already fully reversed (replay)
	}
	res, err := h.mint.Handle(ctx, MintCreditNoteCommand{
		TenantID:           cmd.TenantID,
		InvoiceID:          inv.ID(),
		Kind:               invoicenumber.KindCancellationNote.String(),
		Reason:             cancellationNoteReason(cmd.Reason),
		AmountPaise:        remaining,
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

// cancellationNoteReason is the reason a cancellation note carries: a fixed
// prefix, then the cancel reason. The cancel route accepts a reason as long as
// the note's whole limit, so the composed text is clipped to
// [creditnote.MaxReasonChars] characters (runes, the unit of the column CHECK):
// an accepted cancel must always be able to reverse its invoice. The full
// reason stays on the order (orders.cancellation_reason).
func cancellationNoteReason(cancelReason string) string {
	r := []rune("order cancelled: " + cancelReason)
	if len(r) > creditnote.MaxReasonChars {
		r = r[:creditnote.MaxReasonChars]
	}
	return string(r)
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
	invoices        invoice.Repository
	orders          order.Repository
	allocator       invoicenumber.Allocator
	now             func() time.Time
	newCreditNoteID func() creditnote.ID
}

// NewMintCreditNoteHandler wires the handler.
func NewMintCreditNoteHandler(
	uow pg.UnitOfWork, creditNotes creditnote.Repository, invoices invoice.Repository, orders order.Repository,
	allocator invoicenumber.Allocator, now func() time.Time, newCreditNoteID func() creditnote.ID,
) MintCreditNoteHandler {
	if now == nil {
		now = time.Now
	}
	return MintCreditNoteHandler{uow: uow, creditNotes: creditNotes, invoices: invoices, orders: orders, allocator: allocator, now: now, newCreditNoteID: newCreditNoteID}
}

// Handle mints the credit note atomically. Returns
// [creditnote.ErrCancellationAlreadyExists] when a cancellation note already
// exists for the invoice (idempotent compensation replay), and
// [creditnote.ErrExceedsInvoice] when the notes on the invoice, this one
// included, would pass its grand total. The invoice's order row is locked first
// (the invoice table is append-only, with no UPDATE policy, so it cannot be
// locked itself), so two concurrent notes on one invoice queue and the second
// counts the first.
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
		inv, err := h.invoices.GetByID(ctx, cmd.TenantID, cmd.InvoiceID)
		if err != nil {
			return fmt.Errorf("load invoice: %w", err)
		}
		// Row-lock the order that owns the invoice, persisting nothing: every
		// note on this invoice passes through here, so they serialise, and the
		// notes listed below include every one committed before this lock.
		if err := h.orders.UpdateByID(ctx, cmd.TenantID, inv.OrderID(), func(*order.Order) (bool, error) {
			return false, nil
		}); err != nil {
			return fmt.Errorf("lock invoice order: %w", err)
		}
		existing, err := h.creditNotes.ListByInvoice(ctx, cmd.TenantID, inv.ID())
		if err != nil {
			return fmt.Errorf("list notes: %w", err)
		}
		if err := creditnote.CheckWithinInvoice(inv.GrandTotalPaise(), existing, cmd.AmountPaise); err != nil {
			return err
		}
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
