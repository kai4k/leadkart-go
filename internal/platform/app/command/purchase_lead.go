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

// PurchaseLeadCommand is the tenant "buy this lead" input. AmountPaisa
// is the price in INR paise (never float); Slice 1 charges 1 credit per
// lead, so AmountPaisa is forensic + future price-tier headroom.
type PurchaseLeadCommand struct {
	PlatformLeadID         platformlead.ID
	PurchasingTenantID     platformlead.TenantID
	PurchasingMembershipID unverifiedcontact.MembershipID
	AmountPaisa            int64
}

// PurchaseLeadResult holds the purchase ID — the UUID that also rides
// LeadPurchasedV1.PurchaseID for downstream correlation (CRM's
// LeadPurchasedV1 → CrmLead projection).
type PurchaseLeadResult struct {
	PurchaseID string // UUIDv7
}

// ErrLeadNotFound signals an unknown cmd.PlatformLeadID.
var ErrLeadNotFound = errors.New("purchase lead: lead not found")

// ErrLeadAlreadySold signals the lead is sold to another tenant; HTTP maps to 409.
var ErrLeadAlreadySold = errors.New("purchase lead: lead already sold")

// ErrInsufficientCredits signals a balance below the charge; HTTP maps to 422.
var ErrInsufficientCredits = errors.New("purchase lead: insufficient credits")

// PurchaseLeadHandler runs the marketplace-purchase flow in one
// TxScopePlatform tx: charge the buyer's LeadCredit row, mark the
// PlatformLead sold, then enqueue LeadPurchasedV1. ErrConflict on the
// credit row retries up to 3 times with jitter (ADR 0059).
type PurchaseLeadHandler struct {
	uow           pg.UnitOfWork
	leads         platformlead.Repository
	credits       leadcredit.Repository
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
	outboxEnq OutboxEnqueuer,
	now func() time.Time,
	newPurchaseID func() string,
) PurchaseLeadHandler {
	if newPurchaseID == nil {
		panic("command: NewPurchaseLeadHandler newPurchaseID required")
	}
	if now == nil {
		now = time.Now
	}
	return PurchaseLeadHandler{
		uow: uow, leads: leads, credits: credits, outboxEnq: outboxEnq,
		now: now, newPurchaseID: newPurchaseID,
	}
}

// purchaseChargeCredits is the per-lead cost in Slice 1 (BRD §4.2: one
// credit per purchase). Future slices may vary it by price band.
const purchaseChargeCredits int64 = 1

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
	if cmd.AmountPaisa <= 0 {
		return PurchaseLeadResult{}, errors.New("purchase lead: amount must be positive")
	}
	purchaseID := h.newPurchaseID()

	var lastErr error
	for attempt := range purchaseMaxRetries {
		err := h.runOnce(ctx, cmd, purchaseID)
		if err == nil {
			return PurchaseLeadResult{PurchaseID: purchaseID}, nil
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

// runOnce is one purchase attempt, split out to keep Handle's retry
// loop readable.
func (h PurchaseLeadHandler) runOnce(
	ctx context.Context,
	cmd PurchaseLeadCommand,
	purchaseID string,
) error {
	now := h.now()
	return h.uow.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context) error {
		// Charge first: never mark the lead sold before the debit
		// succeeds, so a failed payment leaves the lead available.
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

		// Mark the lead sold. The mechanical mapper suppresses the
		// PurchasedEvent so we emit LeadPurchasedV1 directly below.
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

		// Re-load to grab the LeadSnapshot for the wire event.
		lead, err := h.leads.GetByID(ctx, cmd.PlatformLeadID)
		if err != nil {
			return fmt.Errorf("reload lead: %w", err)
		}

		// Emit LeadPurchasedV1 (TenantScoped to the purchaser) with a
		// full snapshot for CRM autonomy. UUIDs ride as strings (ADR 0059).
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
