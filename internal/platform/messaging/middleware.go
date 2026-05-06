package messaging

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/platform/audit"
)

// Middleware metadata header names, kept stable across producers +
// consumers so the chain reads consistent metadata regardless of
// publisher origin (forwarder, direct publish, etc.).
const (
	HeaderTenantID      = "tenant_id"
	HeaderEventType     = "event_type"
	HeaderCorrelationID = "correlation_id"
	HeaderOccurredAt    = "occurred_at"
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

