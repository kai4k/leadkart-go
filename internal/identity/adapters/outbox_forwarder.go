package adapters

import (
	"context"
	"fmt"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

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
	err := f.tx.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		q := New(tx)
		rows, err := q.ListUnforwardedOutboxEvents(ctx, f.batchSize)
		if err != nil {
			return fmt.Errorf("forwarder: list unforwarded: %w", err)
		}
		now := time.Now().UTC()
		for _, row := range rows {
			msg := message.NewMessage(uuidFromPg(row.ID).String(), row.Payload)
			msg.Metadata.Set("event_type", row.Topic)
			msg.Metadata.Set("tenant_id", uuidFromPg(row.TenantID).String())
			msg.Metadata.Set("occurred_at", timeFromPg(row.OccurredAt).Format(time.RFC3339Nano))
			msg.SetContext(ctx)

			if err := f.publisher.Publish(f.topic, msg); err != nil {
				return fmt.Errorf("forwarder: publish %s: %w", row.Topic, err)
			}
			if err := q.MarkOutboxEventForwarded(ctx, MarkOutboxEventForwardedParams{
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
		var wait time.Duration
		if count == 0 {
			wait = idleInterval
		} else {
			wait = busyInterval
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
