package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadcredit"
)

// TopupLeadCreditsCommand carries the operator-driven "credit this
// tenant" use case input. AdjustedBy is the operator's membership;
// gated by Platform.LeadCredits.Topup permission at the HTTP layer.
type TopupLeadCreditsCommand struct {
	TenantID    leadcredit.TenantID
	Delta       int64
	Reason      string
	AdjustedBy  leadcredit.MembershipID
}

// TopupLeadCreditsResult holds the post-topup balance — surfaced to
// the operator so the UI can refresh without a separate GET.
type TopupLeadCreditsResult struct {
	NewBalance int64
}

// TopupLeadCreditsHandler is the platform-tier topup flow. Creates the
// LeadCredit row on first topup; uses optimistic-version retry on
// subsequent updates (per ADR 0059).
type TopupLeadCreditsHandler struct {
	uow     pg.UnitOfWork
	credits leadcredit.Repository
	now     func() time.Time
}

// NewTopupLeadCreditsHandler wires the handler.
func NewTopupLeadCreditsHandler(
	uow pg.UnitOfWork,
	credits leadcredit.Repository,
	now func() time.Time,
) TopupLeadCreditsHandler {
	if now == nil {
		now = time.Now
	}
	return TopupLeadCreditsHandler{uow: uow, credits: credits, now: now}
}

const topupMaxRetries = 3

// topupRetryJitterMax bounds the per-attempt random sleep on
// ErrConflict per ADR 0059 ("retries up to 3 times with a small jitter
// ~10ms"). Same shape as purchaseRetryJitterMax — keeps the platform's
// optimistic-concurrency retries thundering-herd-safe.
const topupRetryJitterMax = 10 * time.Millisecond

// Handle runs the topup with optimistic-version retry. Either INSERTs
// a fresh row (when GetByTenant returns ErrNotFound) or UPDATEs the
// existing row.
func (h TopupLeadCreditsHandler) Handle(
	ctx context.Context,
	cmd TopupLeadCreditsCommand,
) (TopupLeadCreditsResult, error) {
	if cmd.Delta <= 0 {
		return TopupLeadCreditsResult{}, fmt.Errorf("topup: delta must be positive (got %d)", cmd.Delta)
	}

	var lastErr error
	for attempt := range topupMaxRetries {
		r, err := h.runOnce(ctx, cmd)
		if err == nil {
			return r, nil
		}
		if !errors.Is(err, leadcredit.ErrConflict) {
			return TopupLeadCreditsResult{}, err
		}
		lastErr = err
		// Jittered sleep between retries (per ADR 0059). Skip the
		// final wait since the loop is about to exit anyway.
		if attempt+1 < topupMaxRetries {
			if waitErr := sleepJitter(ctx, topupRetryJitterMax); waitErr != nil {
				return TopupLeadCreditsResult{}, waitErr
			}
		}
	}
	return TopupLeadCreditsResult{}, fmt.Errorf("topup: exhausted retries: %w", lastErr)
}

func (h TopupLeadCreditsHandler) runOnce(
	ctx context.Context,
	cmd TopupLeadCreditsCommand,
) (TopupLeadCreditsResult, error) {
	now := h.now()
	var result TopupLeadCreditsResult
	err := h.uow.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context) error {
		credit, err := h.credits.GetByTenant(ctx, cmd.TenantID)
		switch {
		case errors.Is(err, leadcredit.ErrNotFound):
			credit, err = leadcredit.NewForTenant(cmd.TenantID, now)
			if err != nil {
				return fmt.Errorf("construct credit row: %w", err)
			}
		case err != nil:
			return fmt.Errorf("load credit: %w", err)
		}
		if err := credit.Topup(cmd.Delta, cmd.Reason, cmd.AdjustedBy, now); err != nil {
			return fmt.Errorf("topup: %w", err)
		}
		if err := h.credits.UpsertWithVersion(ctx, credit); err != nil {
			return err
		}
		result = TopupLeadCreditsResult{NewBalance: credit.Balance()}
		return nil
	})
	return result, err
}
