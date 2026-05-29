package adapters

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/inventory/adapters/db"
	"github.com/leadkart/leadkart-go/internal/inventory/app/actclaim"
	"github.com/leadkart/leadkart-go/internal/inventory/integrationevents"
)

// writeOutboxEvents persists framework-neutral integration events to
// inventory.outbox inside the supplied transaction.
//
// Mirror of internal/identity/adapters/outbox_writer.go. Per ADR 0027 +
// Brandur "events table" pattern: outbox doubles as audit log, single
// tx with the aggregate write. Per ADR 0056: act_* columns carry the
// RFC 8693 actor claim (NULL on the non-impersonation hot path).
func writeOutboxEvents(
	ctx context.Context,
	tx pgx.Tx,
	tenantID uuid.UUID,
	events []integrationevents.Event,
) error {
	if len(events) == 0 {
		return nil
	}
	actOperatorID, actSessionID, actReason := outboxActParams(ctx)

	q := db.New(tx)
	for _, e := range events {
		payload, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("inventory outbox: marshal %s: %w", e.Topic(), err)
		}
		err = q.InsertOutboxEvent(ctx, db.InsertOutboxEventParams{
			ID:            pgconv.PgUUID(ids.NewV7()),
			TenantID:      pgconv.PgUUID(tenantID),
			Topic:         e.Topic(),
			Payload:       payload,
			OccurredAt:    pgconv.PgRequiredTimestamp(e.OccurredAt()),
			ActOperatorID: actOperatorID,
			ActSessionID:  actSessionID,
			ActReason:     actReason,
		})
		if err != nil {
			return fmt.Errorf("inventory outbox: insert %s: %w", e.Topic(), err)
		}
	}
	return nil
}

// outboxActParams projects the per-request actclaim ctx onto the three
// sqlc param slots. Defensive: malformed OperatorID/SessionID (non-UUID)
// is dropped to NULL rather than failing the outbox write — audit-log
// outage MUST NOT cascade per audit-checklist.md §12.
func outboxActParams(ctx context.Context) (pgtype.UUID, pgtype.UUID, *string) {
	c, ok := actclaim.FromContext(ctx)
	if !ok || c.IsZero() {
		return pgtype.UUID{}, pgtype.UUID{}, nil
	}
	var (
		opID  pgtype.UUID
		sesID pgtype.UUID
	)
	if parsed, err := uuid.Parse(c.OperatorID); err == nil {
		opID = pgconv.PgUUID(parsed)
	}
	if parsed, err := uuid.Parse(c.SessionID); err == nil {
		sesID = pgconv.PgUUID(parsed)
	}
	return opID, sesID, pgconv.ZeroToNil(c.Reason)
}

// mapDomainEvents translates a slice of domain events into the
// canonical integration events for outbox storage.
func mapDomainEvents(domainEvents []any) ([]integrationevents.Event, error) {
	if len(domainEvents) == 0 {
		return nil, nil
	}
	out := make([]integrationevents.Event, 0, len(domainEvents))
	for _, d := range domainEvents {
		ie, err := integrationevents.FromDomainEvent(d)
		if err != nil {
			return nil, err
		}
		if ie == nil {
			continue
		}
		out = append(out, ie)
	}
	return out, nil
}
