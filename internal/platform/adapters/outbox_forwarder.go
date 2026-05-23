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
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/platform/adapters/db"
)

// OutboxForwarder polls platform.outbox + republishes unforwarded rows
// to a Watermill publisher. Mirror of internal/identity/adapters/
// OutboxForwarder per ADR 0008. Topic typically "platform.events".
type OutboxForwarder struct {
	pool      *pgxpool.Pool
	tx        *pg.Transactor
	publisher message.Publisher
	topic     string
	batchSize int32
}

// NewOutboxForwarder wires the forwarder.
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

// ForwardOnce drains one batch of unforwarded rows.
func (f *OutboxForwarder) ForwardOnce(ctx context.Context) (int, error) {
	count := 0
	err := f.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		q := db.New(tx)
		rows, err := q.ListUnforwardedPlatformOutboxEvents(ctx, f.batchSize)
		if err != nil {
			return fmt.Errorf("platform forwarder: list unforwarded: %w", err)
		}
		now := clock.Now()
		propagator := otel.GetTextMapPropagator()
		for _, row := range rows {
			msg := message.NewMessage(uuidFromPg(row.ID).String(), row.Payload)
			msg.Metadata.Set("event_type", row.Topic)
			msg.Metadata.Set("tenant_id", uuidFromPg(row.TenantID).String())
			msg.Metadata.Set("occurred_at", timeFromPg(row.OccurredAt).Format(time.RFC3339Nano))
			if row.ActOperatorID.Valid {
				msg.Metadata.Set("act_operator_id", uuidFromPg(row.ActOperatorID).String())
			}
			if row.ActSessionID.Valid {
				msg.Metadata.Set("act_session_id", uuidFromPg(row.ActSessionID).String())
			}
			if row.ActReason != nil && *row.ActReason != "" {
				msg.Metadata.Set("act_reason", *row.ActReason)
			}
			propagator.Inject(ctx, propagation.MapCarrier(msg.Metadata))
			msg.SetContext(ctx)

			if err := f.publisher.Publish(f.topic, msg); err != nil {
				return fmt.Errorf("platform forwarder: publish %s: %w", row.Topic, err)
			}
			if err := q.MarkPlatformOutboxEventForwarded(ctx, db.MarkPlatformOutboxEventForwardedParams{
				ID:          row.ID,
				ForwardedAt: pgRequiredTimestamp(now),
			}); err != nil {
				return fmt.Errorf("platform forwarder: mark forwarded: %w", err)
			}
			count++
		}
		return nil
	})
	return count, err
}

// Run loops ForwardOnce with a fixed-interval poll.
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
