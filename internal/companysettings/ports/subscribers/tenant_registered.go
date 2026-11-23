// Package subscribers holds the companysettings event subscribers.
package subscribers

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/ThreeDotsLabs/watermill/components/cqrs"

	"github.com/leadkart/leadkart-go/internal/companysettings/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	identityevents "github.com/leadkart/leadkart-go/internal/identity/integrationevents"
)

// HandlerSeedSettings is the CI-stable handler name. Renaming it makes every
// previously processed message "fresh" against the inbox dedup table.
const HandlerSeedSettings = "companysettings.subscribers.SeedSettings"

// arch-test:idempotency-via-natural-key-precheck — the seed command runs under the
// tenant's policy row lock and stores version 1 only when the stored version is 0;
// a replay finds version 1 already stored and acknowledges without writing.

// TenantRegisteredSubscriber gives a newly registered tenant its policy through
// the normal path: version 1 (the defaults), with a history row and a
// policy_changed event so Orders and Inventory learn it like any other version.
type TenantRegisteredSubscriber struct {
	seed command.SeedSettingsHandler
	log  *slog.Logger
}

// NewTenantRegisteredSubscriber wires the subscriber. log is required.
func NewTenantRegisteredSubscriber(seed command.SeedSettingsHandler, log *slog.Logger) *TenantRegisteredSubscriber {
	if log == nil {
		panic("subscribers: NewTenantRegisteredSubscriber log required")
	}
	return &TenantRegisteredSubscriber{seed: seed, log: log}
}

// Handle is the typed cqrs handler for `identity.tenant_registered.v1`. Returns
// nil on a replay (the policy already exists).
func (h *TenantRegisteredSubscriber) Handle(ctx context.Context, evt *identityevents.TenantRegisteredV1) error {
	out, err := h.seed.Handle(ctx, command.SeedSettingsCommand{TenantID: tenant.ID(evt.TenantID.String())})
	if err != nil {
		return fmt.Errorf("companysettings tenant registered: %w", err)
	}
	h.log.InfoContext(ctx, "companysettings: policy seeded",
		"tenant_id", evt.TenantID.String(), "already_existed", out.AlreadyExisted)
	return nil
}

// Handlers returns the companysettings cqrs handlers. Each arg may be nil in
// test fixtures that opt out; nils are skipped. The EventProcessor derives the
// subscribe topic from the event alias (identity.* -> identity.events).
func Handlers(tenantRegistered *TenantRegisteredSubscriber) []cqrs.EventHandler {
	var handlers []cqrs.EventHandler
	if tenantRegistered != nil {
		handlers = append(handlers, cqrs.NewEventHandler(HandlerSeedSettings, tenantRegistered.Handle))
	}
	return handlers
}
