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
)

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
