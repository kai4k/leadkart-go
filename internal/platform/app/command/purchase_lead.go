package command

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadcredit"
	"github.com/leadkart/leadkart-go/internal/platform/domain/platformlead"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
	"github.com/leadkart/leadkart-go/internal/platform/integrationevents"
)

// PurchaseLeadCommand carries the input for the tenant "buy this lead"
// use case. AmountPaisa is the price (in INR paise — NEVER float).
// Slice 1 hard-codes 1 credit per lead; the AmountPaisa field exists
// for forensic audit + future price-tier variation.
type PurchaseLeadCommand struct {
	PlatformLeadID         platformlead.ID
	PurchasingTenantID     platformlead.TenantID
	PurchasingMembershipID unverifiedcontact.MembershipID
	AmountPaisa            int64
}

// PurchaseLeadResult holds the wire-side purchase ID — the UUID that
// also appears on LeadPurchasedV1.PurchaseID for downstream
// correlation. CRM consumes this on the LeadPurchasedV1 → CrmLead
// projection.
type PurchaseLeadResult struct {
	PurchaseID string // UUIDv7
}

// ErrLeadNotFound is returned when cmd.PlatformLeadID doesn't exist.
var ErrLeadNotFound = errors.New("purchase lead: lead not found")

// ErrLeadAlreadySold is returned when the lead is sold to a different
// tenant. HTTP layer maps to 409.
var ErrLeadAlreadySold = errors.New("purchase lead: lead already sold")

// ErrInsufficientCredits is returned when the tenant's balance is
// below AmountPaisa. HTTP layer maps to 422.
var ErrInsufficientCredits = errors.New("purchase lead: insufficient credits")

// PurchaseLeadHandler runs the marketplace-purchase flow:
//
//  1. UoW tx (TxScopePlatform — write to platform_leads requires it
//     per migration RLS policy + LeadCredit row access requires
//     platform OR matching tenant).
//  2. UpdateByID(PlatformLead) → Purchase(...): marks the lead sold.
//  3. GetByTenant(LeadCredit) + Charge(amount). Persist via
//     UpsertWithVersion — ErrConflict → retry up to 3 times with
//     small jitter (per ADR 0059).
//  4. Enqueue LeadPurchasedV1 + LeadCreditAdjustedV1.
//
// The lead-charge amount is 1 credit per lead in Slice 1; AmountPaisa
// is the price the tenant paid (forensic field).
type PurchaseLeadHandler struct {
	uow       pg.UnitOfWork
	leads     platformlead.Repository
	credits   leadcredit.Repository
	outboxEnq OutboxEnqueuer
	now       func() time.Time
}

// NewPurchaseLeadHandler wires the handler.
func NewPurchaseLeadHandler(
	uow pg.UnitOfWork,
	leads platformlead.Repository,
	credits leadcredit.Repository,
	outboxEnq OutboxEnqueuer,
	now func() time.Time,
) PurchaseLeadHandler {
	if now == nil {
		now = time.Now
	}
	return PurchaseLeadHandler{
		uow: uow, leads: leads, credits: credits, outboxEnq: outboxEnq, now: now,
	}
}

// purchaseChargeCredits is the per-lead credit cost in Slice 1. Lifted
// from BRD §4.2 ("One lead credit = one lead purchase"). Future slices
// may vary by lead price band; the credit-charge amount becomes a
// derived value at that point.
const purchaseChargeCredits int64 = 1

// purchaseMaxRetries caps the optimistic-concurrency retry budget on
// the LeadCredit row per ADR 0059.
const purchaseMaxRetries = 3

// purchaseRetryJitterMax bounds the per-attempt random sleep on
// ErrConflict per ADR 0059 ("retries up to 3 times with a small jitter
// ~10ms"). Goal: spread the thundering-herd of contending writers so
// retry attempts don't immediately re-collide; without it the loop
// burns CPU + re-locks under load.
const purchaseRetryJitterMax = 10 * time.Millisecond

// Handle runs the purchase flow with optimistic-version retry on the
// lead-credit row.
func (h PurchaseLeadHandler) Handle(
	ctx context.Context,
	cmd PurchaseLeadCommand,
) (PurchaseLeadResult, error) {
	if cmd.AmountPaisa <= 0 {
		return PurchaseLeadResult{}, errors.New("purchase lead: amount must be positive")
	}
	purchaseID := ids.NewV7().String()

	var lastErr error
	for attempt := range purchaseMaxRetries {
		err := h.runOnce(ctx, cmd, purchaseID)
		if err == nil {
			return PurchaseLeadResult{PurchaseID: purchaseID}, nil
		}
		// ErrConflict is the only retryable shape — every other error
		// surfaces immediately.
		if !errors.Is(err, leadcredit.ErrConflict) {
			return PurchaseLeadResult{}, err
		}
		lastErr = err
		// Jittered sleep between retries (per ADR 0059). Skip the
		// final wait since the loop is about to exit anyway.
		if attempt+1 < purchaseMaxRetries {
			if waitErr := sleepJitter(ctx, purchaseRetryJitterMax); waitErr != nil {
				return PurchaseLeadResult{}, waitErr
			}
		}
	}
	return PurchaseLeadResult{}, fmt.Errorf("purchase lead: exhausted retries: %w", lastErr)
}

// sleepJitter blocks for a uniformly-random duration in (0, max].
// Cancels early on ctx done. Test seam: production rng is
// [math/rand/v2.N]; tests swap [jitterDuration] when timing-sensitive.
//
//nolint:gochecknoglobals // test seam — swappable in tests.
var jitterDuration = func(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	// Weak randomness is fine here — this is BACKOFF jitter, not a
	// security primitive. The goal is "spread retries across a small
	// window so they don't immediately re-collide" — even a constant
	// integer would be marginally useful. crypto/rand would be a
	// 100x perf regression for zero security gain.
	//
	//nolint:gosec // G404: backoff jitter is not security-sensitive.
	return rand.N(max)
}

func sleepJitter(ctx context.Context, max time.Duration) error {
	d := jitterDuration(max)
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// runOnce is one attempt at the purchase. Pulled out so the retry loop
// stays readable.
func (h PurchaseLeadHandler) runOnce(
	ctx context.Context,
	cmd PurchaseLeadCommand,
	purchaseID string,
) error {
	now := h.now()
	return h.uow.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context) error {
		// Step 1: charge the tenant's lead-credit balance FIRST. If we
		// can't pay, the lead stays available — never mark the lead
		// sold before the debit succeeds.
		credit, err := h.credits.GetByTenant(ctx, leadcredit.TenantID(cmd.PurchasingTenantID.String()))
		if err != nil {
			if errors.Is(err, leadcredit.ErrNotFound) {
				// No row = no balance = can't pay. Same error shape
				// as "row exists but balance < 1".
				return ErrInsufficientCredits
			}
			return fmt.Errorf("load credits: %w", err)
		}
		err = credit.Charge(
			purchaseChargeCredits,
			"Marketplace purchase: "+cmd.PlatformLeadID.String(),
			leadcredit.MembershipID(cmd.PurchasingMembershipID.String()),
			now,
		)
		if err != nil {
			if errors.Is(err, leadcredit.ErrInsufficientBalance) {
				return ErrInsufficientCredits
			}
			return fmt.Errorf("charge credits: %w", err)
		}
		if err := h.credits.UpsertWithVersion(ctx, credit); err != nil {
			return err // bubble ErrConflict so the outer loop retries
		}

		// Step 2: mark the lead sold. UpdateByID drains the
		// PurchasedEvent (suppressed by the mechanical mapper — we
		// emit LeadPurchasedV1 directly below).
		err = h.leads.UpdateByID(ctx, cmd.PlatformLeadID, func(l *platformlead.PlatformLead) (bool, error) {
			return true, l.Purchase(
				cmd.PurchasingTenantID,
				cmd.PurchasingMembershipID,
				cmd.AmountPaisa,
				now,
			)
		})
		if err != nil {
			if errors.Is(err, platformlead.ErrNotFound) {
				return ErrLeadNotFound
			}
			if errors.Is(err, platformlead.ErrAlreadySold) {
				return ErrLeadAlreadySold
			}
			return fmt.Errorf("mark lead sold: %w", err)
		}

		// Step 3: re-load the lead to grab the LeadSnapshot for the
		// wire event.
		lead, err := h.leads.GetByID(ctx, cmd.PlatformLeadID)
		if err != nil {
			return fmt.Errorf("reload lead: %w", err)
		}

		// Step 4: emit LeadPurchasedV1 (TenantScoped — to the
		// purchaser) — full snapshot for CRM autonomy. UUIDs travel
		// the wire as strings per ADR 0059 frozen brief.
		purchasedEv := integrationevents.LeadPurchasedV1{
			PurchaseID:              purchaseID,
			TenantID:                cmd.PurchasingTenantID.String(),
			PlatformLeadID:          cmd.PlatformLeadID.String(),
			PurchasedAt:             now.UTC(),
			PurchasedByMembershipID: cmd.PurchasingMembershipID.String(),
			AmountPaisa:             cmd.AmountPaisa,
			LeadSnapshot:            integrationevents.SnapshotFromForm(lead.Form()),
		}
		if err := h.outboxEnq.EnqueueInTx(ctx, purchasedEv); err != nil {
			return fmt.Errorf("enqueue lead-purchased: %w", err)
		}
		return nil
	})
}
