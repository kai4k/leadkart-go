// Package adapters holds CRM-module outbound adapters per ADR 0002:
// pg-backed repositories + outbox writer + forwarder. Concrete
// (non-interface) types — domain consumers in internal/crm/app/ depend
// on the interfaces declared in internal/crm/domain/*/repository.go.
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
	"github.com/leadkart/leadkart-go/internal/crm/adapters/db"
	"github.com/leadkart/leadkart-go/internal/crm/integrationevents"
	"github.com/leadkart/leadkart-go/internal/identity/app/actclaim"
)

// writeOutboxEvents persists a batch of framework-neutral integration
// events to crm.outbox inside the supplied transaction. Called by every
// aggregate repository's Add / UpdateByID after the state-mutation
// insert/update.
//
// Mirror of identity-side writeOutboxEvents — same invariants:
//   - One row per event.
//   - Row id is UUIDv7 generated here.
//   - tenant_id anchors the row for RLS scoping.
//   - occurred_at is the domain-time on the event itself.
//   - Per ADR 0056: the per-request actclaim ctx stamps act_* columns
//     onto every emitted outbox row. NULL on the non-impersonation hot
//     path.
//
// The actclaim package is imported from internal/identity/app/ — that
// package has no further deps inward and is the canonical
// cross-module ctx-accessor for the act claim per ADR 0056. The CRM
// app/ layer never imports it (it's an adapter-side concern).
func writeOutboxEvents(
	ctx context.Context,
	tx pgx.Tx,
	tenantID uuid.UUID,
	events []integrationevents.Event,
) error {
	if len(events) == 0 {
		return nil
	}
	actOp, actSes, actReason := outboxActParams(ctx)
	q := db.New(tx)
	for _, e := range events {
		payload, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("crm outbox: marshal %s: %w", e.Topic(), err)
		}
		err = q.InsertCRMOutboxEvent(ctx, db.InsertCRMOutboxEventParams{
			ID:            pgconv.PgUUID(ids.NewV7()),
			TenantID:      pgconv.PgUUID(tenantID),
			Topic:         e.Topic(),
			Payload:       payload,
			OccurredAt:    pgconv.PgRequiredTimestamp(e.OccurredAt()),
			ActOperatorID: actOp,
			ActSessionID:  actSes,
			ActReason:     actReason,
		})
		if err != nil {
			return fmt.Errorf("crm outbox: insert %s: %w", e.Topic(), err)
		}
	}
	return nil
}

// outboxActParams projects the per-request actclaim ctx onto the three
// sqlc param slots. Returns three NULL pgtype values when ctx carries
// no actclaim — the non-impersonation hot path.
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

// mapDomainEvents translates a slice of CRM domain events into V1
// integration events for outbox storage. Surfaces a clear error if a
// UUID inside an event fails to parse (per reviewer H6); panics if
// the mapper hasn't been taught a new domain-event type (per reviewer
// H5 — fail-loud rather than silent-skip).
//
// Returns a non-nil slice (possibly empty) on success. NEVER returns
// (nil, nil) for a non-empty input that had every event mapped — the
// previous defensive `if ie == nil { continue }` was removed because
// FromDomainEvent now panics on unknowns (so nil result is impossible)
// + the defensive branch hid the H5 silent-skip bug class.
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
		out = append(out, ie)
	}
	return out, nil
}
