// Package command holds Platform CQRS command handlers.
//
// TDL Wild Workouts shape: each handler is a concrete struct with one
// Handle method, exposed as a field on the Application facade.
//
// Boundary discipline (ADR 0047): handlers depend only on domain
// repository interfaces plus cross-cutting infra interfaces
// (pg.UnitOfWork, OutboxEnqueuer) — never pgx or a concrete adapter.
package command

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/platform/domain/platformlead"
	"github.com/leadkart/leadkart-go/internal/platform/integrationevents"
)

// OutboxEnqueuer emits integration events the aggregate's own domain
// event cannot carry. LeadVerifiedV1/LeadPurchasedV1 hold derived data
// (LeadSnapshot), so the handler builds them and enqueues here; the
// adapter writes to platform.outbox in the surrounding UoW tx. Mirrors
// identity.RegisterTenantHandler's "emit derived event" path.
type OutboxEnqueuer interface {
	// EnqueueInTx writes events to the platform outbox inside the active
	// UoW tx (from ctx via [pg.TxFromContext]). Errors if no tx is
	// active — call only from within a UoW.WithinTx closure.
	EnqueueInTx(ctx context.Context, events ...integrationevents.Event) error
}

// TierReader resolves the per-tier marketplace config (default sale limit +
// base price) from platform.lead_tiers. The PurchaseLead handler feeds it into
// dynamic pricing + the sale-limit invariant (ADR 0065).
type TierReader interface {
	GetTier(ctx context.Context, tier platformlead.Tier) (platformlead.TierConfig, error)
}
