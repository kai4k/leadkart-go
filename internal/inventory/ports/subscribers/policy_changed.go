package subscribers

import (
	"context"
	"fmt"
	"log/slog"

	companysettingsevents "github.com/leadkart/leadkart-go/internal/companysettings/integrationevents"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/inventory/app/command"
)

// HandlerPolicyChanged is the CI-stable handler name. DO NOT rename.
const HandlerPolicyChanged = "inventory.subscribers.PolicyChanged"

// arch-test:idempotency-via-noop-on-replay — the replica stores a policy only when
// its version is newer than the stored one, so a replayed or reordered
// companysettings.policy_changed.v1 carries a version that is not newer and
// writes nothing.

// PolicyChangedSubscriber keeps Inventory' replica of the company commerce policy
// current from companysettings.policy_changed.v1 (ADR 0070). The event carries
// the WHOLE policy, so the subscriber upserts and never merges.
type PolicyChangedSubscriber struct {
	apply command.ApplyPolicyHandler
	log   *slog.Logger
}

// NewPolicyChangedSubscriber wires the subscriber. log is required.
func NewPolicyChangedSubscriber(apply command.ApplyPolicyHandler, log *slog.Logger) *PolicyChangedSubscriber {
	if log == nil {
		panic("subscribers: NewPolicyChangedSubscriber log required")
	}
	return &PolicyChangedSubscriber{apply: apply, log: log}
}

// Handle is the typed cqrs handler for `companysettings.policy_changed.v1`. A
// malformed policy is an error (retried, then dead-lettered), never stored.
func (h *PolicyChangedSubscriber) Handle(ctx context.Context, evt *companysettingsevents.PolicyChangedV1) error {
	out, err := h.apply.Handle(ctx, command.ApplyPolicyCommand{
		TenantID: tenant.ID(evt.TenantIDClaim.String()), Version: evt.Version, Policy: evt.Policy,
	})
	if err != nil {
		return fmt.Errorf("inventory policy changed: %w", err)
	}
	h.log.InfoContext(ctx, "inventory: policy replica applied",
		"tenant_id", evt.TenantIDClaim.String(), "version", evt.Version, "stored", out.Stored, "republish", evt.Republish)
	return nil
}
