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
	now       func() time.Time
}

// NewOutboxForwarder wires the forwarder. `now` is the explicit time
// source per the clock-injection refactor — composition root wires
// `time.Now`; tests may inject a fixed-time closure for deterministic
// forwarded_at assertions. Nil → time.Now.
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

// ForwardOnce drains one batch of unforwarded rows.
func (f *OutboxForwarder) ForwardOnce(ctx context.Context) (int, error) {
	count := 0
	err := f.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		q := db.New(tx)
		rows, err := q.ListUnforwardedPlatformOutboxEvents(ctx, f.batchSize)
		if err != nil {
			return fmt.Errorf("platform forwarder: list unforwarded: %w", err)
		}
		now := f.now()
		propagator := otel.GetTextMapPropagator()
		for _, row := range rows {
			msg := message.NewMessage(uuidFromPg(row.ID).String(), row.Payload)
			msg.Metadata.Set(messaging.HeaderEventType, row.Topic)
			// tenant_id is set only when the row carries a real tenant FK.
			// Platform-scoped events (tenant_id NULL) ship without the
			// header, so downstream TenantContextMiddleware leaves the
			// scope empty.
			if row.TenantID.Valid {
				msg.Metadata.Set(messaging.HeaderTenantID, uuidFromPg(row.TenantID).String())
			}
			msg.Metadata.Set(messaging.HeaderOccurredAt, timeFromPg(row.OccurredAt).Format(time.RFC3339Nano))
			if row.ActOperatorID.Valid {
				msg.Metadata.Set(messaging.HeaderActOperatorID, uuidFromPg(row.ActOperatorID).String())
			}
			if row.ActSessionID.Valid {
				msg.Metadata.Set(messaging.HeaderActSessionID, uuidFromPg(row.ActSessionID).String())
			}
			if row.ActReason != nil && *row.ActReason != "" {
				msg.Metadata.Set(messaging.HeaderActReason, *row.ActReason)
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
