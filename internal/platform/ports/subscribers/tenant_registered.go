package subscribers

import (
	"context"
	"fmt"
	"log/slog"

	identityevents "github.com/leadkart/leadkart-go/internal/identity/integrationevents"
	"github.com/leadkart/leadkart-go/internal/platform/app/command"
	"github.com/leadkart/leadkart-go/internal/platform/domain/leadcredit"
)

// HandlerName constants — CI-stable per messaging.md "stable handler
// names". Changing one of these makes every previously-processed
// message "fresh" against the inbox dedup table.
const (
	HandlerInitialiseLeadCredits = "platform.subscribers.InitialiseLeadCredits"
)

// arch-test:idempotency-via-natural-key-precheck — dedup happens one
// call-frame down: [command.InitialiseLeadCreditsHandler.Handle] runs
// GetByTenant inside the same tx and short-circuits with
// AlreadyExisted=true on replay. The handler returns nil on that
// branch so Watermill ACKs the duplicate.

// TenantRegisteredIngestor is the Platform-side subscriber that turns
// [identity.TenantRegisteredV1] envelopes into a zero-balance LeadCredit
// row via [command.InitialiseLeadCreditsHandler]. Idempotent — the
// natural-key precheck inside the command short-circuits replays.
type TenantRegisteredIngestor struct {
	cmd command.InitialiseLeadCreditsHandler
	log *slog.Logger
}

// NewTenantRegisteredIngestor wires the subscriber. log is mandatory —
// pass slog.New(slog.NewTextHandler(io.Discard, nil)) in tests that
// don't want output. Mat Ryer canon (NewServer takes the logger
// explicitly); no nil-fallback.
func NewTenantRegisteredIngestor(
	cmd command.InitialiseLeadCreditsHandler,
	log *slog.Logger,
) *TenantRegisteredIngestor {
	if log == nil {
		panic("subscribers: NewTenantRegisteredIngestor log required")
	}
	return &TenantRegisteredIngestor{cmd: cmd, log: log}
}

// Handle is the typed cqrs handler for `identity.tenant_registered.v1`.
// Topic routing + payload decode are owned by the EventProcessor (ADR
// 0067); this is the business reaction only. Returns nil on duplicate
// (AlreadyExisted=true) — the natural-key precheck makes replay a no-op.
func (h *TenantRegisteredIngestor) Handle(ctx context.Context, evt *identityevents.TenantRegisteredV1) error {
	out, err := h.cmd.Handle(ctx, command.InitialiseLeadCreditsCommand{
		TenantID: leadcredit.TenantID(evt.TenantID.String()),
	})
	if err != nil {
		// retry — command-side failure (DB hiccup / lock contention);
		// the natural-key (tenant_id) idempotency check makes the
		// retry safe.
		return fmt.Errorf("platform subscribers: initialise lead credits: %w", err)
	}
	if out.AlreadyExisted {
		h.log.InfoContext(ctx, "platform: lead-credits row already existed (idempotency hit)",
			"tenant_id", evt.TenantID.String(), "slug", evt.Slug)
		return nil
	}
	h.log.InfoContext(ctx, "platform: lead-credits row initialised",
		"tenant_id", evt.TenantID.String(), "slug", evt.Slug)
	return nil
}
