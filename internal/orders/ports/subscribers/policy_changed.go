package subscribers

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/commercepolicy"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"

	companysettingsevents "github.com/leadkart/leadkart-go/internal/companysettings/integrationevents"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/orders/app/command"
)

// HandlerPolicyChanged is the CI-stable handler name. DO NOT rename.
const HandlerPolicyChanged = "orders.subscribers.PolicyChanged"

// HandlerPolicyRepublished is the CI-stable name of the republish handler. DO NOT rename.
const HandlerPolicyRepublished = "orders.subscribers.PolicyRepublished"

// arch-test:idempotency-via-noop-on-replay — the replica stores a policy only when
// its version is newer than the stored one, so a replayed or reordered
// companysettings.policy_changed.v1 carries a version that is not newer and
// writes nothing.

// PolicyChangedSubscriber keeps Orders' replica of the company commerce policy
// current from companysettings.policy_changed.v1 (ADR 0070). The event carries
// the WHOLE policy, so the subscriber upserts and never merges.
type PolicyChangedSubscriber struct {
	applier command.ApplyPolicyHandler
	log     *slog.Logger
}

// NewPolicyChangedSubscriber wires the subscriber. log is required.
func NewPolicyChangedSubscriber(apply command.ApplyPolicyHandler, log *slog.Logger) *PolicyChangedSubscriber {
	if log == nil {
		panic("subscribers: NewPolicyChangedSubscriber log required")
	}
	return &PolicyChangedSubscriber{applier: apply, log: log}
}

// Handle is the typed cqrs handler for `companysettings.policy_changed.v1`. A
// malformed policy is an error (retried, then dead-lettered), never stored.
func (h *PolicyChangedSubscriber) Handle(ctx context.Context, evt *companysettingsevents.PolicyChangedV1) error {
	return h.applyVersion(ctx, evt.TenantIDClaim, evt.Version, evt.Policy, false)
}

// HandleRepublished is the typed cqrs handler for
// `companysettings.policy_republished.v1`: the same replica code under its own
// name, so a re-announcement is never recorded as a change.
func (h *PolicyChangedSubscriber) HandleRepublished(ctx context.Context, evt *companysettingsevents.PolicyRepublishedV1) error {
	return h.applyVersion(ctx, evt.TenantIDClaim, evt.Version, evt.Policy, true)
}

// applyVersion stores one announced version. The tenant comes from the message
// ENVELOPE (stamped by the outbox from the writing tenant); a payload that names
// another tenant is refused outright, never stored under either (review L1).
func (h *PolicyChangedSubscriber) applyVersion(ctx context.Context, payloadTenant uuid.UUID, version int64, policy commercepolicy.Policy, republish bool) error {
	envelope, ok := tenancy.FromContext(ctx)
	if !ok || envelope.String() != payloadTenant.String() {
		return messaging.NonRetryable(fmt.Errorf("orders policy: envelope tenant %q does not match payload tenant %s", envelope, payloadTenant))
	}
	out, err := h.applier.Handle(ctx, command.ApplyPolicyCommand{TenantID: tenant.ID(payloadTenant.String()), Version: version, Policy: policy})
	if err != nil {
		return fmt.Errorf("orders policy changed: %w", err)
	}
	h.log.InfoContext(ctx, "orders: policy replica applied",
		"tenant_id", payloadTenant.String(), "version", version, "stored", out.Stored, "republish", republish)
	return nil
}
