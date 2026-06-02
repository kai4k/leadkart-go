package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadcredit"
)

// InitialiseLeadCreditsCommand carries the "tenant just registered;
// ensure a zero-balance LeadCredit row exists" use case input. Driven
// by the [identity.TenantRegisteredV1] subscriber in
// internal/platform/ports/subscribers — never reached from the
// request path.
//
// Idempotent by design: the natural-key check (GetByTenant →
// ErrNotFound) short-circuits to AlreadyExisted=true on broker replay.
// Per BRD §6.2 "Consumed: TenantRegistered → initialise lead credits".
type InitialiseLeadCreditsCommand struct {
	TenantID leadcredit.TenantID
}

// InitialiseLeadCreditsResult tells the subscriber whether the row
// already existed (replay) or was freshly inserted. Subscribers log
// the distinction; the wire return is the same (Watermill ACK).
type InitialiseLeadCreditsResult struct {
	AlreadyExisted bool
}

// InitialiseLeadCreditsHandler boots a zero-balance LeadCredit row on
// tenant registration. Sister-command of [TopupLeadCreditsHandler]; the
// two CAN both create the row (whichever message lands first wins +
// the loser short-circuits via the natural-key check).
type InitialiseLeadCreditsHandler struct {
	uow     pg.UnitOfWork
	credits leadcredit.Repository
	now     func() time.Time
}

// NewInitialiseLeadCreditsHandler wires the handler.
func NewInitialiseLeadCreditsHandler(
	uow pg.UnitOfWork,
	credits leadcredit.Repository,
	now func() time.Time,
) InitialiseLeadCreditsHandler {
	if now == nil {
		now = time.Now
	}
	return InitialiseLeadCreditsHandler{uow: uow, credits: credits, now: now}
}

// Handle runs the row-init inside a UoW tx. Idempotent: existing row →
// AlreadyExisted=true + no write. Missing row → INSERT a fresh
// zero-balance row.
func (h InitialiseLeadCreditsHandler) Handle(
	ctx context.Context,
	cmd InitialiseLeadCreditsCommand,
) (InitialiseLeadCreditsResult, error) {
	if cmd.TenantID.IsZero() {
		return InitialiseLeadCreditsResult{}, errors.New("initialise lead credits: tenant_id required")
	}

	var result InitialiseLeadCreditsResult
	err := h.uow.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context) error {
		_, err := h.credits.GetByTenant(ctx, cmd.TenantID)
		switch {
		case err == nil:
			result.AlreadyExisted = true
			return nil
		case errors.Is(err, leadcredit.ErrNotFound):
			// fall through to insert
		default:
			return fmt.Errorf("load credit: %w", err)
		}

		credit, err := leadcredit.NewForTenant(cmd.TenantID, h.now())
		if err != nil {
			return fmt.Errorf("construct credit row: %w", err)
		}
		if err := h.credits.UpsertWithVersion(ctx, credit); err != nil {
			// A concurrent insert won the race — re-load + treat as
			// "already existed" so the subscriber ACKs cleanly. Per
			// at-least-once delivery doctrine: a competing Initialise
			// (or Topup → first-row) is an expected concurrent path.
			if errors.Is(err, leadcredit.ErrConflict) {
				result.AlreadyExisted = true
				return nil
			}
			return fmt.Errorf("insert credit row: %w", err)
		}
		return nil
	})
	if err != nil {
		return InitialiseLeadCreditsResult{}, err
	}
	return result, nil
}
