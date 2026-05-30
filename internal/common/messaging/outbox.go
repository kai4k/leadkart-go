package messaging

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/ThreeDotsLabs/watermill"
	wmsql "github.com/ThreeDotsLabs/watermill-sql/v4/pkg/sql"
	"github.com/ThreeDotsLabs/watermill/components/forwarder"
	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/leadkart/leadkart-go/internal/common/actclaim"
)

// OutboxForwarderTopic is the single logical topic the transactional
// outbox is keyed on. The watermill-sql publisher writes enveloped
// messages to [OutboxTable]; the Watermill Forwarder (in cmd/worker)
// subscribes to this same topic, unwraps each envelope, and republishes
// to the destination module topic embedded inside it.
//
// One shared relay table across all bounded contexts: per ADR 0064 the
// outbox is platform-only infrastructure (a pure relay, not per-tenant
// state, not the audit log), so it does NOT need a table-per-module nor
// RLS — the destination module topic travels in the envelope.
const OutboxForwarderTopic = "outbox"

// OutboxTable is the physical relay table backing [OutboxForwarderTopic].
// Created by migration 20260604000002 with the exact column shape
// watermill-sql's PostgreSQLQueueSchema expects (offset/uuid/payload/
// metadata/acked/created_at).
const OutboxTable = "common.outbox"

// outboxTableName maps every topic to [OutboxTable]. The producer side
// only ever publishes to [OutboxForwarderTopic], and the subscriber side
// only ever reads it, so the single mapping is sufficient.
func outboxTableName(string) string { return OutboxTable }

// OutboxQueueSchema is the watermill-sql schema adapter shared by the
// producer publisher and the worker subscriber. Both sides MUST use the
// same schema + table name or the INSERT and SELECT disagree.
//
// DeleteOnAck (on the offsets adapter, not here) drains the row once the
// Forwarder republishes it, so the relay table stays small.
func OutboxQueueSchema() wmsql.PostgreSQLQueueSchema {
	return wmsql.PostgreSQLQueueSchema{
		GenerateMessagesTableName: outboxTableName,
	}
}

// OutboxOffsetsAdapter is the matching offsets adapter for the worker
// subscriber. DeleteOnAck=true so a forwarded row is deleted (the relay
// is not an archive — durability of the *event* is the destination
// broker's job once republished).
func OutboxOffsetsAdapter() wmsql.PostgreSQLQueueOffsetsAdapter {
	return wmsql.PostgreSQLQueueOffsetsAdapter{
		DeleteOnAck:               true,
		GenerateMessagesTableName: outboxTableName,
	}
}

// OutboxEvent is the minimal contract [PublishOutbox] needs from a
// module's integration event. Every module's integrationevents.Event
// already satisfies it (Topic returns the per-event wire alias; OccurredAt
// the domain timestamp).
type OutboxEvent interface {
	Topic() string
	OccurredAt() time.Time
}

// PublishOutbox writes events to the transactional outbox inside tx — the
// same pgx.Tx as the aggregate state mutation, so the rows commit
// atomically with the write (Brandur "Transactionally staged job drains";
// TDL outbox canon). Replaces the four hand-rolled module outbox writers.
//
// destinationTopic is the module's Watermill topic (e.g. "identity.events")
// that subscribers consume; the Forwarder republishes there after unwrapping.
// Each event's Topic() is stamped as the event_type metadata header so the
// subscriber routes by event kind on the shared topic.
//
// Canonical metadata stamped per message: event_type, tenant_id (omitted
// when Nil — platform-scoped), occurred_at, the RFC 8693 act_* claim from
// ctx (ADR 0056), and the W3C trace context (so the consumer span joins the
// producer trace across the async hop).
func PublishOutbox[E OutboxEvent](
	ctx context.Context,
	tx pgx.Tx,
	destinationTopic string,
	tenantID uuid.UUID,
	events []E,
) error {
	if len(events) == 0 {
		return nil
	}

	sqlPub, err := wmsql.NewPublisher(
		wmsql.TxFromPgx(tx),
		wmsql.PublisherConfig{SchemaAdapter: OutboxQueueSchema()},
		watermill.NopLogger{},
	)
	if err != nil {
		return fmt.Errorf("outbox: new sql publisher: %w", err)
	}
	pub := forwarder.NewPublisher(sqlPub, forwarder.PublisherConfig{ForwarderTopic: OutboxForwarderTopic})

	actOperatorID, actSessionID, actReason := outboxActClaim(ctx)
	propagator := otel.GetTextMapPropagator()

	for _, ev := range events {
		payload, merr := json.Marshal(ev)
		if merr != nil {
			return fmt.Errorf("outbox: marshal %s: %w", ev.Topic(), merr)
		}
		msg := message.NewMessage(uuid.NewString(), payload)
		msg.Metadata.Set(HeaderEventType, ev.Topic())
		if tenantID != uuid.Nil {
			msg.Metadata.Set(HeaderTenantID, tenantID.String())
		}
		msg.Metadata.Set(HeaderOccurredAt, ev.OccurredAt().UTC().Format(time.RFC3339Nano))
		if actOperatorID != "" {
			msg.Metadata.Set(HeaderActOperatorID, actOperatorID)
		}
		if actSessionID != "" {
			msg.Metadata.Set(HeaderActSessionID, actSessionID)
		}
		if actReason != "" {
			msg.Metadata.Set(HeaderActReason, actReason)
		}
		// Inject W3C trace context onto the metadata so the subscriber span
		// (messaging.TraceContextMiddleware) joins this producer's trace.
		msg.SetContext(ctx)
		propagator.Inject(ctx, propagation.MapCarrier(msg.Metadata))

		if perr := pub.Publish(destinationTopic, msg); perr != nil {
			return fmt.Errorf("outbox: publish %s: %w", ev.Topic(), perr)
		}
	}
	return nil
}

// outboxActClaim projects the per-request RFC 8693 actor claim (set by the
// authn middleware after JWT verification) into three string metadata
// values. Defensive: a malformed OperatorID/SessionID is dropped to ""
// rather than failing the write — audit-log outage MUST NOT cascade
// (audit-checklist.md §12). Empty on the non-impersonation hot path.
func outboxActClaim(ctx context.Context) (operatorID, sessionID, reason string) {
	c, ok := actclaim.FromContext(ctx)
	if !ok || c.IsZero() {
		return "", "", ""
	}
	// Validate UUID shape; drop to "" on malformed (mirrors the prior
	// pgconv.PgUUID-with-parse-guard behaviour in the per-module writers).
	if _, err := uuid.Parse(c.OperatorID); err != nil {
		operatorID = ""
	} else {
		operatorID = c.OperatorID
	}
	if _, err := uuid.Parse(c.SessionID); err != nil {
		sessionID = ""
	} else {
		sessionID = c.SessionID
	}
	return operatorID, sessionID, c.Reason
}
