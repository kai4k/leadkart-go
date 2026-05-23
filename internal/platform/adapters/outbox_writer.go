package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/app/actclaim"
	"github.com/leadkart/leadkart-go/internal/platform/adapters/db"
	"github.com/leadkart/leadkart-go/internal/platform/integrationevents"
)

// OutboxEnqueuer satisfies internal/platform/app/command.OutboxEnqueuer.
// Writes one row per event to platform.outbox inside the active UoW tx
// (pulled from ctx via pg.TxFromContext). Mirrors identity's outbox
// writer per ADR 0008.
//
// Concrete struct + factory keep wiring boundary-clean: cmd/api builds
// this with a single pool dep and passes it to handlers as the
// `OutboxEnqueuer` interface.
type OutboxEnqueuer struct{}

// NewOutboxEnqueuer returns a zero-stateful enqueuer; pool isn't held
// because the tx travels via ctx.
func NewOutboxEnqueuer() *OutboxEnqueuer { return &OutboxEnqueuer{} }

// ErrNoActiveTx surfaces when EnqueueInTx is called outside a
// UoW.WithinTx closure. Programmer bug — surfaces in tests immediately.
var ErrNoActiveTx = errors.New("platform outbox: no active tx in ctx (call from UoW.WithinTx)")

// EnqueueInTx satisfies the app-layer OutboxEnqueuer interface. Writes
// each integration event to platform.outbox under the active tx.
func (e *OutboxEnqueuer) EnqueueInTx(ctx context.Context, events ...integrationevents.Event) error {
	if len(events) == 0 {
		return nil
	}
	tx, ok := pg.TxFromContext(ctx)
	if !ok {
		return ErrNoActiveTx
	}
	return writeOutboxEvents(ctx, tx, tenantOfEvents(events), events)
}

// writeOutboxEvents persists the supplied batch under the supplied tx.
// Unexported — repositories call this from their Add / UpdateByID
// methods to drain aggregate-side events.
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
	for _, ev := range events {
		payload, err := json.Marshal(ev)
		if err != nil {
			return fmt.Errorf("platform outbox: marshal %s: %w", ev.Topic(), err)
		}
		err = q.InsertPlatformOutboxEvent(ctx, db.InsertPlatformOutboxEventParams{
			ID:            pgUUID(ids.NewV7()),
			TenantID:      pgUUID(tenantID),
			Topic:         ev.Topic(),
			Payload:       payload,
			OccurredAt:    pgRequiredTimestamp(ev.OccurredAt()),
			ActOperatorID: actOperatorID,
			ActSessionID:  actSessionID,
			ActReason:     actReason,
		})
		if err != nil {
			return fmt.Errorf("platform outbox: insert %s: %w", ev.Topic(), err)
		}
	}
	return nil
}

// tenantOfEvents picks the tenant FK to stamp on the outbox row. For
// a single-event batch we read from the event itself when it's
// TenantScoped; for Platform events we use uuid.Nil. For a mixed batch
// we use the first TenantScoped's tenant — handlers SHOULD NOT mix
// platform + tenant-scoped events in one EnqueueInTx call (each call is
// idempotent + cheap, separate them).
func tenantOfEvents(events []integrationevents.Event) uuid.UUID {
	for _, ev := range events {
		if ts, ok := ev.(integrationevents.TenantScoped); ok {
			return ts.TenantID()
		}
	}
	return uuid.Nil
}

// outboxActParams projects the per-request actclaim ctx (set by the
// authn middleware after JWT verification) onto the three sqlc param
// slots. Defensive: malformed OperatorID / SessionID (non-UUID) is
// dropped to NULL rather than failing the outbox write — audit-log
// outage MUST NOT cascade per audit-checklist.md §12.
//
// Reuses internal/identity/app/actclaim — that ctx accessor is
// authn-side wiring shared across modules per ADR 0056. Cross-module
// import is legitimate (actclaim is the wire-shaped ctx contract).
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
		opID = pgUUID(parsed)
	}
	if parsed, err := uuid.Parse(c.SessionID); err == nil {
		sesID = pgUUID(parsed)
	}
	var reason *string
	if c.Reason != "" {
		r := c.Reason
		reason = &r
	}
	return opID, sesID, reason
}

// drainEventsToOutbox is the shared "pull domain events + map + persist"
// helper used by every repository. Domain events get mapped through
// integrationevents.FromDomainEvent (which may suppress with nil); the
// resulting integration events go to platform.outbox under tx.
//
// Each repository's Add / UpdateByID call this once per persist.
func drainEventsToOutbox(
	ctx context.Context,
	tx pgx.Tx,
	tenantID uuid.UUID,
	domainEvents []any,
) error {
	mapped, err := mapDomainEvents(domainEvents)
	if err != nil {
		return err
	}
	return writeOutboxEvents(ctx, tx, tenantID, mapped)
}

// mapDomainEvents translates the slice of domain events through
// FromDomainEvent. Suppresses (returns nil) when the mapper does — used
// for events that are emitted directly by the handler with derived data
// (e.g. LeadVerifiedV1 with snapshot).
func mapDomainEvents(domainEvents []any) ([]integrationevents.Event, error) {
	if len(domainEvents) == 0 {
		return nil, nil
	}
	out := make([]integrationevents.Event, 0, len(domainEvents))
	for _, d := range domainEvents {
		ie, err := integrationevents.FromDomainEvent(d)
		if err != nil {
			return nil, fmt.Errorf("platform repo: map domain event %T: %w", d, err)
		}
		if ie == nil {
			continue
		}
		out = append(out, ie)
	}
	return out, nil
}
