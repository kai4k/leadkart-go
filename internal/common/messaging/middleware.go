package messaging

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.27.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/common/audit"
)

// Middleware metadata header names, kept stable across producers +
// consumers so the chain reads consistent metadata regardless of
// publisher origin (forwarder, direct publish, etc.).
//
// HeaderActOperatorID / HeaderActSessionID / HeaderActReason carry the
// RFC 8693 actor claim across the outbox → subscriber boundary per
// ADR 0056. Stamped by the OutboxForwarder when the row carries an
// act_* column; consumed by [AuditMiddleware] to populate
// audit_log_entry.act_*. Empty on the non-impersonation hot path.
const (
	HeaderTenantID      = "tenant_id"
	HeaderEventType     = "event_type"
	HeaderCorrelationID = "correlation_id"
	HeaderOccurredAt    = "occurred_at"
	HeaderActOperatorID = "act_operator_id"
	HeaderActSessionID  = "act_session_id"
	HeaderActReason     = "act_reason"
)

// TenantContextMiddleware bridges the tenant_id metadata header into
// the request ctx via [tenancy.WithID] before the handler runs.
//
// Watermill's "envelope" is the *Message — its metadata headers carry
// tenant_id stamped by the OutboxForwarder. The router pipes ctx
// through to handlers via msg.SetContext / msg.Context().
//
// Per messaging.md "Tenant channel — `Envelope.TenantId` is canonical".
func TenantContextMiddleware(h message.HandlerFunc) message.HandlerFunc {
	return func(msg *message.Message) ([]*message.Message, error) {
		if tid := msg.Metadata.Get(HeaderTenantID); tid != "" && tid != uuid.Nil.String() {
			msg.SetContext(tenancy.WithID(msg.Context(), tenancy.ID(tid)))
		}
		return h(msg)
	}
}

// tracerName is the OTel tracer used by [TraceContextMiddleware]. One
// tracer per package is the OTel canon; "messaging.consumer" matches
// the OpenTelemetry messaging semantic conventions §"Messaging spans"
// (operation name "process" on the receive side).
const tracerName = "github.com/leadkart/leadkart-go/internal/common/messaging"

// TraceContextMiddleware extracts the W3C Trace Context the producer
// stamped on the message metadata + opens a per-message "process" span
// rooted at that remote parent. Without this the consumer side of every
// outbox edge starts a NEW root span — the trace tree fragments at
// every async hop + distributed tracing collapses to per-process slices.
//
// OTel semantic conventions for messaging (v1.27): the receive-side
// span MUST be named `<destination> process` and tagged with
// messaging.system + messaging.destination.name. Span ends when the
// handler returns; errors are recorded on the span.
//
// Pairs with the inject in [adapters.OutboxForwarder.ForwardOnce].
func TraceContextMiddleware(h message.HandlerFunc) message.HandlerFunc {
	tracer := otel.Tracer(tracerName)
	propagator := otel.GetTextMapPropagator()
	return func(msg *message.Message) ([]*message.Message, error) {
		parentCtx := propagator.Extract(msg.Context(), propagation.MapCarrier(msg.Metadata))
		eventType := msg.Metadata.Get(HeaderEventType)
		spanName := eventType + " process"
		if eventType == "" {
			spanName = "watermill process"
		}
		ctx, span := tracer.Start(parentCtx, spanName,
			trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithAttributes(
				semconv.MessagingSystemKey.String("watermill"),
				semconv.MessagingOperationTypeProcess,
				semconv.MessagingDestinationName(eventType),
				semconv.MessagingMessageID(msg.UUID),
			),
		)
		defer span.End()
		msg.SetContext(ctx)
		out, err := h(msg)
		if err != nil {
			span.RecordError(err)
		}
		return out, err
	}
}

// CorrelationIDMiddleware ensures every message has a correlation_id
// header — generates one if absent, propagates the existing value if
// present. Downstream handlers + the audit middleware read it via
// [HeaderCorrelationID].
func CorrelationIDMiddleware(h message.HandlerFunc) message.HandlerFunc {
	return func(msg *message.Message) ([]*message.Message, error) {
		if msg.Metadata.Get(HeaderCorrelationID) == "" {
			msg.Metadata.Set(HeaderCorrelationID, uuid.NewString())
		}
		return h(msg)
	}
}

// AuditMiddleware writes one audit row per processed message — success
// or failure. Action is the event_type metadata; UserID + TenantID
// come from headers (when populated); duration is wall-clock from
// invocation to handler return.
//
// Per audit-checklist.md §12: failure to audit MUST NOT cascade. The
// audit Writer already swallows + logs; this middleware just calls it.
func AuditMiddleware(writer *audit.Writer, log *slog.Logger) message.HandlerMiddleware {
	if log == nil {
		log = slog.Default()
	}
	return func(h message.HandlerFunc) message.HandlerFunc {
		return func(msg *message.Message) ([]*message.Message, error) {
			start := time.Now()
			out, err := h(msg)
			duration := time.Since(start)

			entry := audit.Entry{
				Action:        msg.Metadata.Get(HeaderEventType),
				TenantID:      parseUUIDHeader(msg.Metadata.Get(HeaderTenantID)),
				CorrelationID: parseUUIDHeader(msg.Metadata.Get(HeaderCorrelationID)),
				OccurredAtUTC: parseTimeHeader(msg.Metadata.Get(HeaderOccurredAt)),
				Duration:      duration,
				Succeeded:     err == nil,
				// Per ADR 0056: propagate the RFC 8693 actor claim from
				// Watermill metadata onto the audit row. Malformed
				// UUIDs are dropped to uuid.Nil — audit-log outage MUST
				// NOT cascade per audit-checklist.md §12.
				ActOperatorID: parseUUIDHeader(msg.Metadata.Get(HeaderActOperatorID)),
				ActSessionID:  parseUUIDHeader(msg.Metadata.Get(HeaderActSessionID)),
				ActReason:     msg.Metadata.Get(HeaderActReason),
			}
			if err != nil {
				entry.FailureReason = err.Error()
			}
			if werr := writer.Write(msg.Context(), entry); werr != nil {
				// Audit writer already logs — surface at DEBUG only so
				// we don't double-log. The handler error (if any) is
				// what gets surfaced to the broker.
				log.DebugContext(msg.Context(), "audit middleware: write failed",
					"action", entry.Action, "err", werr)
			}
			return out, err
		}
	}
}

// IdempotencyMiddleware wraps each handler with [IdempotentReceiver]
// dedup. handlerName must be unique per registered subscriber; the
// router exposes a helper that derives it from the registration name.
func IdempotencyMiddleware(receiver *IdempotentReceiver, handlerName string) message.HandlerMiddleware {
	return func(h message.HandlerFunc) message.HandlerFunc {
		// Translate Watermill's HandlerFunc to messaging.HandlerFunc
		// so IdempotentReceiver.Wrap can wrap it. The translation is
		// straight-through except we surface the published-messages
		// slice via a captured variable.
		return func(msg *message.Message) ([]*message.Message, error) {
			var capturedOut []*message.Message
			capturedErr := receiver.Wrap(handlerName, func(_ context.Context, _ string) error {
				out, err := h(msg)
				capturedOut = out
				return err
			})(msg.Context(), msg.UUID)
			if capturedErr != nil {
				return nil, capturedErr
			}
			return capturedOut, nil
		}
	}
}

// parseUUIDHeader returns the parsed UUID or uuid.Nil on empty/invalid.
// uuid.Nil is the audit Writer's "absent" sentinel.
func parseUUIDHeader(raw string) uuid.UUID {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return uuid.Nil
	}
	parsed, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil
	}
	return parsed
}

// parseTimeHeader returns the parsed time or time.Now().UTC() on
// empty/invalid. The audit Writer falls back to now() too, but doing
// it here surfaces the parse failure earlier in observability.
func parseTimeHeader(raw string) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

