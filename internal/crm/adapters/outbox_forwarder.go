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
	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/crm/adapters/db"
)

// OutboxForwarder polls crm.outbox + republishes unforwarded rows to a
// Watermill publisher. Mirror of identity-side OutboxForwarder per ADR
// 0008 + 0027 + 0056.
//
// Runs as a long-lived goroutine inside cmd/worker. All reads + UPDATEs
// run under TxScopePlatform — crm.outbox is RLS+FORCE and the forwarder
// reads across every tenant.
type OutboxForwarder struct {
	pool      *pgxpool.Pool
	tx        *pg.Transactor
	publisher message.Publisher
	topic     string // Watermill destination — usually "crm.events"
	batchSize int32
}

// NewOutboxForwarder wires the forwarder. batchSize 0 → 100.
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
// drained + any error.
func (f *OutboxForwarder) ForwardOnce(ctx context.Context) (int, error) {
	count := 0
	err := f.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		q := db.New(tx)
		rows, err := q.SelectUnforwardedCRMEvents(ctx, f.batchSize)
		if err != nil {
			return fmt.Errorf("crm forwarder: list unforwarded: %w", err)
		}
		now := clock.Now()
		propagator := otel.GetTextMapPropagator()
		for _, row := range rows {
			msg := message.NewMessage(uuidFromPg(row.ID).String(), row.Payload)
			msg.Metadata.Set(messaging.HeaderEventType, row.Topic)
			msg.Metadata.Set(messaging.HeaderTenantID, uuidFromPg(row.TenantID).String())
			msg.Metadata.Set(messaging.HeaderOccurredAt, timeFromPg(row.OccurredAt).Format(time.RFC3339Nano))
			// Per ADR 0056: propagate the RFC 8693 actor claim from the
			// outbox row onto Watermill message metadata.
			if row.ActOperatorID.Valid {
				msg.Metadata.Set(messaging.HeaderActOperatorID, uuidFromPg(row.ActOperatorID).String())
			}
			if row.ActSessionID.Valid {
				msg.Metadata.Set(messaging.HeaderActSessionID, uuidFromPg(row.ActSessionID).String())
			}
			if row.ActReason != nil && *row.ActReason != "" {
				msg.Metadata.Set(messaging.HeaderActReason, *row.ActReason)
			}
			// W3C Trace Context propagation across the broker. Subscriber-
			// side extract lives in messaging.TraceContextMiddleware.
			propagator.Inject(ctx, propagation.MapCarrier(msg.Metadata))
			msg.SetContext(ctx)

			if err := f.publisher.Publish(f.topic, msg); err != nil {
				return fmt.Errorf("crm forwarder: publish %s: %w", row.Topic, err)
			}
			if err := q.MarkCRMEventForwarded(ctx, db.MarkCRMEventForwardedParams{
				ID:          row.ID,
				ForwardedAt: pgRequiredTimestamp(now),
			}); err != nil {
				return fmt.Errorf("crm forwarder: mark forwarded: %w", err)
			}
			count++
		}
		return nil
	})
	return count, err
}

// Run loops ForwardOnce with a fixed-interval poll. Mirror of
// identity-side Run.
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
