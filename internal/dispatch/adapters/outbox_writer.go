// Package adapters holds Dispatch-module outbound adapters (ADR 0002):
// the pg-backed ConsignmentNote repository + the outbox drain helper.
// Concrete types — app/ consumers depend on the domain repository interface.
package adapters

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/dispatch/integrationevents"
)

// writeOutboxEvents persists integration events to the shared common.outbox
// relay inside tx (same pgx.Tx as the aggregate mutation), per ADR 0064/0067.
// tenant_id / occurred_at / act_* travel as message metadata stamped by
// messaging.PublishOutbox; this wrapper supplies the Dispatch destination topic.
func writeOutboxEvents(
	ctx context.Context,
	tx pgx.Tx,
	tenantID uuid.UUID,
	events []integrationevents.Event,
) error {
	return messaging.PublishOutbox(ctx, tx, integrationevents.Topic, tenantID, events)
}
