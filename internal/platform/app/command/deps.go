// Package command holds Platform CQRS command handlers.
//
// Per TDL Wild Workouts canonical layout: each handler is a concrete
// struct with a single Handle method. Handlers aggregate as fields on
// the Application facade; HTTP ports call `app.Commands.X.Handle(...)`.
//
// Boundary discipline (ADR 0047): handlers depend on domain repository
// INTERFACES + a handful of cross-cutting infra interfaces
// (pg.UnitOfWork, OutboxEnqueuer). No pgx, no pgxpool, no concrete
// adapter struct.
package command

import (
	"context"

	"github.com/leadkart/leadkart-go/internal/platform/integrationevents"
)

// OutboxEnqueuer is the app-layer interface for direct integration-
// event emission. Some events (LeadVerifiedV1, LeadPurchasedV1) carry
// derived data (LeadSnapshot) that the aggregate's domain event does
// NOT hold — the handler builds the integration event + enqueues it
// here. The adapter implementation writes to the platform.outbox table
// inside the surrounding UnitOfWork tx (via pg.TxFromContext).
//
// Mirrors the pattern used in identity.RegisterTenantHandler for the
// post-multi-aggregate "emit derived event" path.
type OutboxEnqueuer interface {
	// EnqueueInTx writes the supplied integration events to the
	// platform outbox INSIDE the active UoW tx (pulled from ctx via
	// [pg.TxFromContext]). Returns an error when no tx is active
	// (programmer bug — call only from inside a UoW.WithinTx closure).
	EnqueueInTx(ctx context.Context, events ...integrationevents.Event) error
}
