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
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/ids"
)

// Entry is the row shape of buildingblocks.audit_log_entry — used by
// both [Writer.Write] (caller leaves ID zero; writer generates) and
// [Reader] (read-back populates ID from the row).
//
// UserID / TenantID / CorrelationID use uuid.Nil for "absent" (NULL
// in the column) — keeps the API signature simple without sprinkling
// pointers everywhere. The writer translates Nil → SQL NULL.
//
// ActOperatorID / ActSessionID / ActReason are the RFC 8693 actor-
// chain columns (per ADR 0045 + migration 20260524000001). Populated
// ONLY for rows emitted under a scoped impersonation token; nil/zero
// for regular rows. Translates to SQL NULL the same way.
type Entry struct {
	ID            uuid.UUID // populated on read; ignored on write (writer mints v7)
	Action        string
	UserID        uuid.UUID
	TenantID      uuid.UUID
	CorrelationID uuid.UUID
	OccurredAtUTC time.Time
	Duration      time.Duration
	Succeeded     bool
	FailureReason string
	Payload       []byte // optional jsonb; nil = SQL NULL

	// Impersonation actor chain (RFC 8693 act claim) — populated by
	// AuditLoggingMiddleware when the request's JWT carries Claims.Act.
	ActOperatorID uuid.UUID // uuid.Nil for non-impersonation rows
	ActSessionID  uuid.UUID // uuid.Nil for non-impersonation rows
	ActReason     string    // empty for non-impersonation rows
}

// Writer persists [Entry] rows. Concrete implementation; constructed
// once at composition root + injected wherever audit writes happen.
type Writer struct {
	pool *pgxpool.Pool
	log  *slog.Logger
	now  func() time.Time
}

// NewWriter wires the writer. `now` is the explicit time source per
// the clock-injection refactor — composition root wires `time.Now`.
// Nil → time.Now.
func NewWriter(pool *pgxpool.Pool, log *slog.Logger, now func() time.Time) *Writer {
	if log == nil {
		panic("audit: NewWriter log required")
	}
	if now == nil {
		now = time.Now
	}
	return &Writer{pool: pool, log: log, now: now}
}

// Write inserts an audit row. Returns nil on success.
//
// Per audit-checklist.md §12 — Write failures NEVER cascade out of the
// caller. The writer logs at WARN + returns nil so an audit-log outage
// doesn't 500 every authenticated request. Caller can ignore the
// return; it exists for tests + observability.
func (w *Writer) Write(ctx context.Context, e Entry) error {
	if e.OccurredAtUTC.IsZero() {
		e.OccurredAtUTC = w.now()
	}
	e.OccurredAtUTC = e.OccurredAtUTC.UTC()
	if e.Action == "" {
		return errors.New("audit: Entry.Action required")
	}

	var (
		userIDArg         any = nil
		tenantIDArg       any = nil
		correlationIDArg  any = nil
		failureReasonArg  any = nil
		payloadArg        any = nil
		actOperatorIDArg  any = nil
		actSessionIDArg   any = nil
		actReasonArg      any = nil
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
	if e.ActOperatorID != uuid.Nil {
		actOperatorIDArg = e.ActOperatorID
	}
	if e.ActSessionID != uuid.Nil {
		actSessionIDArg = e.ActSessionID
	}
	if e.ActReason != "" {
		actReasonArg = e.ActReason
	}

	_, err := w.pool.Exec(ctx, `
		INSERT INTO buildingblocks.audit_log_entry
			(id, action, user_id, tenant_id, correlation_id,
			 occurred_at_utc, duration_ms, succeeded, failure_reason, payload,
			 act_operator_id, act_session_id, act_reason)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
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
		actOperatorIDArg,
		actSessionIDArg,
		actReasonArg,
	)
	if err != nil {
		// Audit-log outage MUST NOT cascade. Log + swallow per
		// audit-checklist.md §12. Returning the error here would 500
		// every authenticated request the moment the audit table /
		// Postgres has a transient blip — which is the exact failure
		// mode the doctrine forbids. Caller can inspect the WARN log
		// + the buildingblocks.audit_log_entry row count metric to
		// detect outage; it MUST NOT branch on the return value.
		w.log.WarnContext(ctx, "audit: write failed (swallowed)",
			"action", e.Action, "err", err)
		return nil
	}
	return nil
}
