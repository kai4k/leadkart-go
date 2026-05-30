package messaging

import (
	"fmt"

	"github.com/ThreeDotsLabs/watermill"
	wmsql "github.com/ThreeDotsLabs/watermill-sql/v4/pkg/sql"
	"github.com/ThreeDotsLabs/watermill/components/forwarder"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewOutboxForwarder builds the single Watermill Forwarder that drains the
// shared transactional outbox ([OutboxTable]) and republishes each event
// to its destination module topic on outPub. Replaces the four hand-rolled
// per-module OutboxForwarder poll loops (ADR 0064).
//
// Mechanics: a watermill-sql subscriber reads enveloped rows from
// common.outbox (PostgreSQLQueueSchema, DeleteOnAck), the Forwarder unwraps
// each envelope and publishes the inner message to the destination topic
// embedded in it (set by [PublishOutbox] via forwarder.NewPublisher), then
// the row is deleted on ack. The producer + this subscriber MUST share the
// same schema/table — both go through [OutboxQueueSchema] / the matching
// offsets adapter, so they cannot drift.
//
// outPub is the destination broker publisher (gochannel in v0.2; the same
// pub/sub the subscriber router consumes). The returned Forwarder is driven
// with Run(ctx)/Close() like the router.
func NewOutboxForwarder(
	pool *pgxpool.Pool,
	outPub message.Publisher,
	logger watermill.LoggerAdapter,
) (*forwarder.Forwarder, error) {
	if logger == nil {
		logger = watermill.NopLogger{}
	}
	sub, err := wmsql.NewSubscriber(
		wmsql.BeginnerFromPgx(pool),
		wmsql.SubscriberConfig{
			SchemaAdapter:  OutboxQueueSchema(),
			OffsetsAdapter: OutboxOffsetsAdapter(),
			// Schema is owned by goose (migration 20260604000002); do NOT
			// let the library issue CREATE TABLE.
			InitializeSchema: false,
		},
		logger,
	)
	if err != nil {
		return nil, fmt.Errorf("outbox forwarder: new sql subscriber: %w", err)
	}

	fwd, err := forwarder.NewForwarder(sub, outPub, logger, forwarder.Config{
		ForwarderTopic: OutboxForwarderTopic,
	})
	if err != nil {
		return nil, fmt.Errorf("outbox forwarder: new forwarder: %w", err)
	}
	return fwd, nil
}
