// Package adapters holds companysettings outbound adapters (ADR 0002): the
// pg-backed settings repository and the outbox drain helper.
package adapters

import (
	"context"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/companysettings/integrationevents"
)

// writeOutboxEvents persists integration events to the shared common.outbox
// relay inside tx (the same pgx.Tx as the aggregate mutation), per ADR 0064 and
// ADR 0067.
func writeOutboxEvents(
	ctx context.Context,
	tx pgx.Tx,
	tenantID uuid.UUID,
	events []integrationevents.Event,
) error {
	return messaging.PublishOutbox(ctx, tx, integrationevents.Topic, tenantID, events)
}
