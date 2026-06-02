package command

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadcredit"
	"github.com/leadkart/leadkart-go/internal/platform/domain/platformlead"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
	"github.com/leadkart/leadkart-go/internal/platform/integrationevents"
)

// PurchaseLeadCommand is the tenant "buy this lead" input. The price is
// computed server-side at purchase time (ADR 0065 dynamic pricing) — the
// client does not propose an amount.
type PurchaseLeadCommand struct {
	PlatformLeadID         platformlead.ID
	PurchasingTenantID     platformlead.TenantID
	PurchasingMembershipID unverifiedcontact.MembershipID
}

// PurchaseLeadResult holds the purchase ID — the UUID that also rides
// LeadPurchasedV1.PurchaseID for downstream correlation (CRM's
// LeadPurchasedV1 → CrmLead projection) — plus the amount the buyer was
// charged (computed price, snapshotted on the lead_purchases row).
type PurchaseLeadResult struct {
	PurchaseID  string // UUIDv7
	AmountPaisa int64  // computed price the buyer paid
}

// ErrLeadNotFound signals an unknown cmd.PlatformLeadID.
var ErrLeadNotFound = errors.New("purchase lead: lead not found")

// ErrLeadSoldOut signals the lead has reached its sale limit; HTTP maps to 409.
var ErrLeadSoldOut = errors.New("purchase lead: lead sold out")

// ErrLeadAlreadyPurchased signals the buying tenant already owns this lead;
// HTTP maps to 409.
var ErrLeadAlreadyPurchased = errors.New("purchase lead: already purchased by tenant")

// ErrInsufficientCredits signals a balance below the charge; HTTP maps to 422.
var ErrInsufficientCredits = errors.New("purchase lead: insufficient credits")

// PurchaseLeadHandler runs the marketplace-purchase flow in one
// TxScopePlatform tx: charge the buyer's LeadCredit row, record a
// PlatformLead purchase (under a row lock so the sale limit is race-free),
// then enqueue LeadPurchasedV1. ErrConflict on the credit row retries up to
// 3 times with jitter (ADR 0059 + 0065).
type PurchaseLeadHandler struct {
	uow           pg.UnitOfWork
	leads         platformlead.Repository
	credits       leadcredit.Repository
	tiers         TierReader
	outboxEnq     OutboxEnqueuer
	now           func() time.Time
	newPurchaseID func() string
}

// NewPurchaseLeadHandler wires the handler.
//
// newPurchaseID is injected per TestArch_HandlersInjectIDFactory; tests
// inject a deterministic counter for pinnable IDs.
func NewPurchaseLeadHandler(
	uow pg.UnitOfWork,
	leads platformlead.Repository,
	credits leadcredit.Repository,
	tiers TierReader,
	outboxEnq OutboxEnqueuer,
	now func() time.Time,
	newPurchaseID func() string,
) PurchaseLeadHandler {
	if tiers == nil {
		panic("command: NewPurchaseLeadHandler tiers reader required")
	}
	if newPurchaseID == nil {
		panic("command: NewPurchaseLeadHandler newPurchaseID required")
	}
	if now == nil {
		now = time.Now
	}
	return PurchaseLeadHandler{
		uow: uow, leads: leads, credits: credits, tiers: tiers, outboxEnq: outboxEnq,
		now: now, newPurchaseID: newPurchaseID,
	}
}

// purchaseChargeCredits is the per-lead cost (BRD §4.2: one credit per
// purchase). The credit debit is flat; the money price (amount_paisa) is the
// dynamic, tier-based value recorded on the purchase row.
const purchaseChargeCredits int64 = 1

// purchasePackageDiscountBps is the buyer's subscription-package discount in
// basis points. Zero until the tenant subscription/package concept lands
// (ADR 0065 — the pricing hook is wired, the input defaults to 0).
const purchasePackageDiscountBps = 0

// purchaseMaxRetries caps the optimistic-concurrency retries on the
// LeadCredit row (ADR 0059).
const purchaseMaxRetries = 3

// purchaseRetryJitterMax bounds the per-attempt jitter on ErrConflict
// (ADR 0059) to spread contending writers so retries don't re-collide.
const purchaseRetryJitterMax = 10 * time.Millisecond

// Handle runs the purchase with optimistic-version retry on the credit row.
func (h PurchaseLeadHandler) Handle(
	ctx context.Context,
	cmd PurchaseLeadCommand,
) (PurchaseLeadResult, error) {
	purchaseID := h.newPurchaseID()

	var lastErr error
	for attempt := range purchaseMaxRetries {
		amountPaisa, err := h.runOnce(ctx, cmd, purchaseID)
		if err == nil {
			return PurchaseLeadResult{PurchaseID: purchaseID, AmountPaisa: amountPaisa}, nil
		}
		// ErrConflict is the only retryable shape.
		if !errors.Is(err, leadcredit.ErrConflict) {
			return PurchaseLeadResult{}, err
		}
		lastErr = err
		// Skip the final wait; the loop is about to exit (ADR 0059).
		if attempt+1 < purchaseMaxRetries {
			if waitErr := sleepJitter(ctx, purchaseRetryJitterMax); waitErr != nil {
				return PurchaseLeadResult{}, waitErr
			}
		}
	}
	return PurchaseLeadResult{}, fmt.Errorf("purchase lead: exhausted retries: %w", lastErr)
}

// jitterDuration returns a uniform random duration in (0, max].
// Swappable test seam; production uses [math/rand/v2.N].
//
//nolint:gochecknoglobals // test seam — swappable in tests.
var jitterDuration = func(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	// Weak randomness is fine: this is backoff jitter, not security.
	// crypto/rand would be a 100x perf hit for zero security gain.
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

// runOnce is one purchase attempt, split out to keep Handle's retry loop
// readable. Returns the computed price charged on success.
func (h PurchaseLeadHandler) runOnce(
	ctx context.Context,
	cmd PurchaseLeadCommand,
	purchaseID string,
) (int64, error) {
	now := h.now()
	var chargedPaisa int64
	err := h.uow.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context) error {
		// Charge first: never record the purchase before the debit succeeds,
		// so a failed payment leaves the lead available. The credit cost is a
		// flat 1 (BRD §4.2); the money price is computed below.
		credit, err := h.credits.GetByTenant(ctx, leadcredit.TenantID(cmd.PurchasingTenantID.String()))
		if err != nil {
			if errors.Is(err, leadcredit.ErrNotFound) {
				// No row = no balance; same shape as balance < 1.
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

		// Resolve the lead's tier config (base price + default sale limit)
		// for pricing + the limit invariant.
		lead0, err := h.leads.GetByID(ctx, cmd.PlatformLeadID)
		if err != nil {
			if errors.Is(err, platformlead.ErrNotFound) {
				return ErrLeadNotFound
			}
			return fmt.Errorf("load lead: %w", err)
		}
		tierCfg, err := h.tiers.GetTier(ctx, lead0.Tier())
		if err != nil {
			return fmt.Errorf("resolve tier %q: %w", lead0.Tier(), err)
		}

		// Record the purchase under a row lock (the repo's UpdateByID locks the
		// lead + hydrates the buyer set) so the count-based limit is race-free.
		// Price is computed from the locked purchase count. The mapper
		// suppresses PurchasedEvent — we emit LeadPurchasedV1 directly below.
		err = h.leads.UpdateByID(ctx, cmd.PlatformLeadID, func(l *platformlead.PlatformLead) (bool, error) {
			price := platformlead.ComputePurchasePricePaisa(
				tierCfg.BasePricePaisa, l.PurchaseCount(), purchasePackageDiscountBps)
			chargedPaisa = price
			return true, l.RecordPurchase(
				purchaseID,
				cmd.PurchasingTenantID,
				cmd.PurchasingMembershipID,
				price,
				tierCfg.DefaultSaleLimit,
				now,
			)
		})
		if err != nil {
			switch {
			case errors.Is(err, platformlead.ErrNotFound):
				return ErrLeadNotFound
			case errors.Is(err, platformlead.ErrSoldOut):
				return ErrLeadSoldOut
			case errors.Is(err, platformlead.ErrAlreadyPurchased):
				return ErrLeadAlreadyPurchased
			default:
				return fmt.Errorf("record purchase: %w", err)
			}
		}

		// Re-load to grab the LeadSnapshot for the wire event.
		lead, err := h.leads.GetByID(ctx, cmd.PlatformLeadID)
		if err != nil {
			return fmt.Errorf("reload lead: %w", err)
		}

		// Emit LeadPurchasedV1 (TenantScoped to the purchaser) with a full
		// snapshot for CRM autonomy. Per-purchase event (ADR 0065). UUIDs ride
		// as strings (ADR 0059).
		purchasedEv := integrationevents.LeadPurchasedV1{
			PurchaseID:              purchaseID,
			TenantID:                cmd.PurchasingTenantID.String(),
			PlatformLeadID:          cmd.PlatformLeadID.String(),
			PurchasedAt:             now.UTC(),
			PurchasedByMembershipID: cmd.PurchasingMembershipID.String(),
			AmountPaisa:             chargedPaisa,
			LeadSnapshot:            integrationevents.SnapshotFromForm(lead.Form()),
		}
		if err := h.outboxEnq.EnqueueInTx(ctx, purchasedEv); err != nil {
			return fmt.Errorf("enqueue lead-purchased: %w", err)
		}
		return nil
	})
	return chargedPaisa, err
}
