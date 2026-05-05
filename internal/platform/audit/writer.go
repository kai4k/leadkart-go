// Package audit holds the cross-cutting audit-log writer that persists
// to `buildingblocks.audit_log_entry` per messaging.md "Audit log
// middleware — commands only" + data-retention.md "Audit log retention".
//
// The writer is consumed by both the Watermill router's
// AuditLoggingMiddleware (auto-write per processed message) and the
// HTTP idempotency middleware (audit replay-attempts). Both call sites
// produce one row per command; queries filter by action + user_id +
// tenant_id + occurred_at_utc.
//
// Per audit-checklist.md §12: failure to write the audit row MUST
// NOT cascade — audit-log outage is logged at WARN and swallowed.
// Otherwise a Postgres blip would 500 every command.
package audit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/ids"
)

// Entry is the row shape written to buildingblocks.audit_log_entry.
//
// UserID / TenantID / CorrelationID use uuid.Nil for "absent" (NULL
// in the column) — keeps the API signature simple without sprinkling
// pointers everywhere. The writer translates Nil → SQL NULL.
type Entry struct {
	Action         string
	UserID         uuid.UUID
	TenantID       uuid.UUID
	CorrelationID  uuid.UUID
	OccurredAtUTC  time.Time
	Duration       time.Duration
	Succeeded      bool
	FailureReason  string
	Payload        []byte // optional jsonb; nil = SQL NULL
}

// Writer persists [Entry] rows. Concrete implementation; constructed
// once at composition root + injected wherever audit writes happen.
type Writer struct {
	pool *pgxpool.Pool
	log  *slog.Logger
}

// NewWriter wires the writer.
func NewWriter(pool *pgxpool.Pool, log *slog.Logger) *Writer {
	if log == nil {
		log = slog.Default()
	}
	return &Writer{pool: pool, log: log}
}

// Write inserts an audit row. Returns nil on success.
//
// Per audit-checklist.md §12 — Write failures NEVER cascade out of the
// caller. The writer logs at WARN + returns nil so an audit-log outage
// doesn't 500 every authenticated request. Caller can ignore the
// return; it exists for tests + observability.
func (w *Writer) Write(ctx context.Context, e Entry) error {
	if e.OccurredAtUTC.IsZero() {
		e.OccurredAtUTC = time.Now().UTC()
	} else {
		e.OccurredAtUTC = e.OccurredAtUTC.UTC()
	}
	if e.Action == "" {
		return errors.New("audit: Entry.Action required")
	}

	var (
		userIDArg        any = nil
		tenantIDArg      any = nil
		correlationIDArg any = nil
		failureReasonArg any = nil
		payloadArg       any = nil
	)
	if e.UserID != uuid.Nil {
		userIDArg = e.UserID
	}
	if e.TenantID != uuid.Nil {
		tenantIDArg = e.TenantID
	}
	if e.CorrelationID != uuid.Nil {
		correlationIDArg = e.CorrelationID
	}
	if e.FailureReason != "" {
		failureReasonArg = e.FailureReason
	}
	if len(e.Payload) > 0 {
		payloadArg = e.Payload
	}

	_, err := w.pool.Exec(ctx, `
		INSERT INTO buildingblocks.audit_log_entry
			(id, action, user_id, tenant_id, correlation_id,
			 occurred_at_utc, duration_ms, succeeded, failure_reason, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`,
		ids.NewV7(),
		e.Action,
		userIDArg,
		tenantIDArg,
		correlationIDArg,
		e.OccurredAtUTC,
		e.Duration.Milliseconds(),
		e.Succeeded,
		failureReasonArg,
		payloadArg,
	)
	if err != nil {
		// Audit-log outage MUST NOT cascade. Log + swallow.
		w.log.WarnContext(ctx, "audit: write failed (swallowed)",
			"action", e.Action, "err", err)
		return fmt.Errorf("audit: write %s: %w", e.Action, err)
	}
	return nil
}
