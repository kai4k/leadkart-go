package adapters

import (
	"context"
	"fmt"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/inventory/adapters/db"
)

// OutboxForwarder polls inventory.outbox + republishes unforwarded rows
// to a Watermill publisher. Mirror of internal/identity/adapters/outbox_forwarder.go
// per the per-module-outbox rule (CLAUDE.md §"Each module owns its
// Postgres schema" — identity forwarder would only see identity.outbox
// because its sqlc queries are hardcoded to that schema; inventory needs
// its own forwarder that uses inventory's sqlc Queries).
//
// Per Brandur "There's always an events table" + Chris Richardson
// *Microservices Patterns* ch.3. Runs as a long-lived goroutine inside
// cmd/worker; ForwardOnce drains one batch and returns — used directly
// in tests for deterministic verification.
//
// All reads + UPDATEs run under TxScopePlatform: the inventory.outbox
// table is RLS+FORCE; the forwarder reads across every tenant.
type OutboxForwarder struct {
	pool      *pgxpool.Pool
	tx        *pg.Transactor
	publisher message.Publisher
	topic     string // Watermill destination — usually "inventory.events"
	batchSize int32
	now       func() time.Time
}

// NewOutboxForwarder wires the forwarder against a pool + publisher.
// topic is the Watermill destination; the per-event Topic() metadata
// goes into the message envelope so downstream subscribers can route
// by event_type without a separate topic per event kind.
//
// batchSize 0 → 100. Production tunes higher under load.
//
// `now` is the explicit time source per the clock-injection refactor —
// composition root wires `time.Now`; tests can inject a fixed-time
// closure for deterministic forwarded_at assertions. Nil → time.Now.
func NewOutboxForwarder(
	pool *pgxpool.Pool,
	tx *pg.Transactor,
	publisher message.Publisher,
	topic string,
	batchSize int32,
	now func() time.Time,
) *OutboxForwarder {
	if batchSize <= 0 {
		batchSize = 100
	}
	if now == nil {
		now = time.Now
	}
	return &OutboxForwarder{
		pool:      pool,
		tx:        tx,
		publisher: publisher,
		topic:     topic,
		batchSize: batchSize,
		now:       now,
	}
}

// ForwardOnce drains one batch of unforwarded rows. Returns the count
// drained + any error. Tests call this directly; Run wraps it in a
// poll loop with backoff.
func (f *OutboxForwarder) ForwardOnce(ctx context.Context) (int, error) {
	count := 0
	err := f.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		q := db.New(tx)
		rows, err := q.ListUnforwardedOutboxEvents(ctx, f.batchSize)
		if err != nil {
			return fmt.Errorf("inventory forwarder: list unforwarded: %w", err)
		}
		now := f.now()
		propagator := otel.GetTextMapPropagator()
		for _, row := range rows {
			msg := message.NewMessage(pgconv.UUIDFromPg(row.ID).String(), row.Payload)
			msg.Metadata.Set(messaging.HeaderEventType, row.Topic)
			msg.Metadata.Set(messaging.HeaderTenantID, pgconv.UUIDFromPg(row.TenantID).String())
			msg.Metadata.Set(messaging.HeaderOccurredAt, pgconv.TimeFromPg(row.OccurredAt).Format(time.RFC3339Nano))
			// Per ADR 0056: propagate the RFC 8693 actor claim from the
			// outbox row onto Watermill message metadata. Subscriber-side
			// AuditMiddleware reads these back to populate
			// audit_log_entry.act_*. Empty metadata for non-impersonation
			// rows — the AuditMiddleware path is presence-checked.
			if row.ActOperatorID.Valid {
				msg.Metadata.Set(messaging.HeaderActOperatorID, pgconv.UUIDFromPg(row.ActOperatorID).String())
			}
			if row.ActSessionID.Valid {
				msg.Metadata.Set(messaging.HeaderActSessionID, pgconv.UUIDFromPg(row.ActSessionID).String())
			}
			if row.ActReason != nil && *row.ActReason != "" {
				msg.Metadata.Set(messaging.HeaderActReason, *row.ActReason)
			}
			// W3C Trace Context propagation across the broker. Mirror of
			// identity forwarder's inject — every async hop must inject on
			// send + extract on receive. Subscriber-side extract lives in
			// messaging.TraceContextMiddleware.
			propagator.Inject(ctx, propagation.MapCarrier(msg.Metadata))
			msg.SetContext(ctx)

			if err := f.publisher.Publish(f.topic, msg); err != nil {
				return fmt.Errorf("inventory forwarder: publish %s: %w", row.Topic, err)
			}
			if err := q.MarkOutboxEventForwarded(ctx, db.MarkOutboxEventForwardedParams{
				ID:          row.ID,
				ForwardedAt: pgconv.PgRequiredTimestamp(now),
			}); err != nil {
				return fmt.Errorf("inventory forwarder: mark forwarded: %w", err)
			}
			count++
		}
		return nil
	})
	return count, err
}

// Run loops ForwardOnce with a fixed-interval poll. Backs off when no
// rows were forwarded; tightens to a faster cadence after a non-empty
// batch so spikes drain quickly.
//
// Returns when ctx is cancelled. Errors per cycle are logged via the
// caller-supplied errFn; the loop never exits on a forwarder failure
// — outbox publish is best-effort and will retry on the next tick.
//
// idleInterval: poll cadence when last batch was empty (e.g. 1s).
// busyInterval: poll cadence when last batch had rows (e.g. 50ms).
func (f *OutboxForwarder) Run(ctx context.Context, idleInterval, busyInterval time.Duration, errFn func(error)) {
	if errFn == nil {
		errFn = func(error) {}
	}
	if idleInterval <= 0 {
		idleInterval = time.Second
	}
	if busyInterval <= 0 {
		busyInterval = 50 * time.Millisecond
	}
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		count, err := f.ForwardOnce(ctx)
		if err != nil {
			errFn(err)
		}
		wait := idleInterval
		if count > 0 {
			wait = busyInterval
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
