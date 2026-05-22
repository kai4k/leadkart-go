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

	"github.com/leadkart/leadkart-go/internal/common/clock"
	"github.com/leadkart/leadkart-go/internal/identity/adapters/db"
	"github.com/leadkart/leadkart-go/internal/platform/pg"
)

// OutboxForwarder polls identity.outbox + republishes unforwarded rows
// to a Watermill publisher. Implements the canonical outbox pattern
// per Brandur "There's always an events table" + Chris Richardson
// *Microservices Patterns* ch.3.
//
// Runs as a long-lived goroutine inside the API binary (or a separate
// worker process); ForwardOnce drains one batch and returns — used
// directly in tests for deterministic verification.
//
// All reads + UPDATEs run under TxScopePlatform: the outbox table is
// RLS+FORCE; the forwarder reads across every tenant.
type OutboxForwarder struct {
	pool      *pgxpool.Pool
	tx        *pg.Transactor
	publisher message.Publisher
	topic     string // Watermill destination — usually "identity.events"
	batchSize int32
}

// NewOutboxForwarder wires the forwarder against a pool + publisher.
// topic is the Watermill destination; the per-event Topic() metadata
// goes into the message envelope so downstream subscribers can route
// by event_type without a separate topic per event kind.
//
// batchSize 0 → 100. Production tunes higher under load.
func NewOutboxForwarder(
	pool *pgxpool.Pool,
	tx *pg.Transactor,
	publisher message.Publisher,
	topic string,
	batchSize int32,
) *OutboxForwarder {
	if batchSize <= 0 {
		batchSize = 100
	}
	return &OutboxForwarder{
		pool:      pool,
		tx:        tx,
		publisher: publisher,
		topic:     topic,
		batchSize: batchSize,
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
			return fmt.Errorf("forwarder: list unforwarded: %w", err)
		}
		now := clock.Now()
		propagator := otel.GetTextMapPropagator()
		for _, row := range rows {
			msg := message.NewMessage(uuidFromPg(row.ID).String(), row.Payload)
			msg.Metadata.Set("event_type", row.Topic)
			msg.Metadata.Set("tenant_id", uuidFromPg(row.TenantID).String())
			msg.Metadata.Set("occurred_at", timeFromPg(row.OccurredAt).Format(time.RFC3339Nano))
			// W3C Trace Context propagation across the broker. The forwarder
			// runs in a separate process from the producing handler (cmd/api
			// → cmd/worker over Postgres-backed broker in v0.3); without
			// inject the consumer span has no parent and the trace tree
			// breaks at every async edge. OTel canon: every async hop must
			// inject on send + extract on receive. Subscriber-side extract
			// lives in messaging.TraceContextMiddleware.
			propagator.Inject(ctx, propagation.MapCarrier(msg.Metadata))
			msg.SetContext(ctx)

			if err := f.publisher.Publish(f.topic, msg); err != nil {
				return fmt.Errorf("forwarder: publish %s: %w", row.Topic, err)
			}
			if err := q.MarkOutboxEventForwarded(ctx, db.MarkOutboxEventForwardedParams{
				ID:          row.ID,
				ForwardedAt: pgRequiredTimestamp(now),
			}); err != nil {
				return fmt.Errorf("forwarder: mark forwarded: %w", err)
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
		// Idle cadence on empty batch; tighter cadence after a non-empty
		// batch so spikes drain quickly. Doctrine ban on `else`: pick
		// idleInterval first, narrow on the busy branch.
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
