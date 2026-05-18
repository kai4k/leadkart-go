package query

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/adapters/db"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/platform/pg"
)

// AuditEventView is one row of the audit-log read shape per
// ADR 0027 (outbox doubles as audit) + the additive
// buildingblocks.audit_log_entry table from migration
// 20260507000001. Fields:
//
//   - Action — domain command name (e.g. "tenant.suspended")
//   - Actor / TenantContext — uuid strings; empty when NULL
//   - Succeeded + FailureReason — outcome surface for forensic
//     queries ("show me every failed suspend in the last week")
//   - OccurredAt — UTC timestamp; ALSO the keyset sort column
//   - DurationMs — command execution wall-clock
//   - Payload — raw JSONB bytes; HTTP layer surfaces as-is
type AuditEventView struct {
	ID            string
	Action        string
	ActorID       string
	TenantID      string
	CorrelationID string
	OccurredAt    time.Time
	DurationMs    int64
	Succeeded     bool
	FailureReason string
	PayloadRaw    []byte
}

// ----- ListAuditEventsByTenant ---------------------------------------------

// ListAuditEventsByTenantQuery returns paginated tenant-scoped
// audit events. Cursor walks (occurred_at_utc DESC, id DESC). Per
// ADR 0038 keyset semantics; per ADR 0039 caller MUST be operator
// (is_platform=true) OR same-tenant admin — HTTP layer gates.
type ListAuditEventsByTenantQuery struct {
	TenantID tenant.ID
	Cursor   pagination.Cursor
	PageSize int
}

// ListAuditEventsByTenantHandler runs the keyset query under
// platform scope (the audit table has no RLS but cross-tenant
// access surface is gated above this layer).
type ListAuditEventsByTenantHandler struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewListAuditEventsByTenantHandler wires the handler.
func NewListAuditEventsByTenantHandler(pool *pgxpool.Pool, tx *pg.Transactor) ListAuditEventsByTenantHandler {
	if pool == nil {
		panic("query: NewListAuditEventsByTenantHandler pool required")
	}
	if tx == nil {
		panic("query: NewListAuditEventsByTenantHandler transactor required")
	}
	return ListAuditEventsByTenantHandler{pool: pool, tx: tx, q: db.New(pool)}
}

// Handle returns one page of tenant-scoped events.
func (h ListAuditEventsByTenantHandler) Handle(ctx context.Context, q ListAuditEventsByTenantQuery) (pagination.Page[AuditEventView], error) {
	if q.TenantID.IsZero() {
		return pagination.Page[AuditEventView]{}, errors.New("list_audit_by_tenant: tenant id required")
	}
	pageSize := pagination.ClampPageSize(q.PageSize)
	beforeAt, beforeID := cursorOrSentinel(q.Cursor)

	tenantUUID, err := uuid.Parse(q.TenantID.String())
	if err != nil {
		return pagination.Page[AuditEventView]{}, fmt.Errorf("list_audit_by_tenant: parse tenant id: %w", err)
	}

	var rows []db.BuildingblocksAuditLogEntry
	err = h.tx.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		out, qerr := h.q.WithTx(tx).ListAuditEventsByTenantPage(ctx, db.ListAuditEventsByTenantPageParams{
			TenantID:       pgtype.UUID{Bytes: tenantUUID, Valid: true},
			BeforeOccurred: pgtype.Timestamptz{Time: beforeAt, Valid: true},
			BeforeID:       pgtype.UUID{Bytes: beforeID, Valid: true},
			Limit:          int32(pageSize + 1), //nolint:gosec // pageSize capped at 200
		})
		if qerr != nil {
			return qerr
		}
		rows = out
		return nil
	})
	if err != nil {
		return pagination.Page[AuditEventView]{}, fmt.Errorf("list_audit_by_tenant: %w", err)
	}

	views := projectAuditRows(rows)
	return pagination.BuildPage(views, pageSize, func(v AuditEventView) pagination.Cursor {
		return pagination.Cursor{SortValue: v.OccurredAt, ID: v.ID}
	}), nil
}

// ----- ListAuditEventsByUser -----------------------------------------------

// ListAuditEventsByUserQuery returns paginated person-scoped audit
// events. UserID is the JWT subject (person_id), NOT a membership
// id — audit_log_entry.user_id mirrors JWT.Subject.
type ListAuditEventsByUserQuery struct {
	UserID   person.ID
	Cursor   pagination.Cursor
	PageSize int
}

// ListAuditEventsByUserHandler runs the keyset query under
// platform scope (the table has no RLS; cross-user access surface
// is gated above this layer).
type ListAuditEventsByUserHandler struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewListAuditEventsByUserHandler wires the handler.
func NewListAuditEventsByUserHandler(pool *pgxpool.Pool, tx *pg.Transactor) ListAuditEventsByUserHandler {
	if pool == nil {
		panic("query: NewListAuditEventsByUserHandler pool required")
	}
	if tx == nil {
		panic("query: NewListAuditEventsByUserHandler transactor required")
	}
	return ListAuditEventsByUserHandler{pool: pool, tx: tx, q: db.New(pool)}
}

// Handle returns one page of person-scoped events.
func (h ListAuditEventsByUserHandler) Handle(ctx context.Context, q ListAuditEventsByUserQuery) (pagination.Page[AuditEventView], error) {
	if q.UserID.IsZero() {
		return pagination.Page[AuditEventView]{}, errors.New("list_audit_by_user: user id required")
	}
	pageSize := pagination.ClampPageSize(q.PageSize)
	beforeAt, beforeID := cursorOrSentinel(q.Cursor)

	userUUID, err := uuid.Parse(q.UserID.String())
	if err != nil {
		return pagination.Page[AuditEventView]{}, fmt.Errorf("list_audit_by_user: parse user id: %w", err)
	}

	var rows []db.BuildingblocksAuditLogEntry
	err = h.tx.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		out, qerr := h.q.WithTx(tx).ListAuditEventsByUserPage(ctx, db.ListAuditEventsByUserPageParams{
			UserID:         pgtype.UUID{Bytes: userUUID, Valid: true},
			BeforeOccurred: pgtype.Timestamptz{Time: beforeAt, Valid: true},
			BeforeID:       pgtype.UUID{Bytes: beforeID, Valid: true},
			Limit:          int32(pageSize + 1), //nolint:gosec // pageSize capped at 200
		})
		if qerr != nil {
			return qerr
		}
		rows = out
		return nil
	})
	if err != nil {
		return pagination.Page[AuditEventView]{}, fmt.Errorf("list_audit_by_user: %w", err)
	}

	views := projectAuditRows(rows)
	return pagination.BuildPage(views, pageSize, func(v AuditEventView) pagination.Cursor {
		return pagination.Cursor{SortValue: v.OccurredAt, ID: v.ID}
	}), nil
}

// ----- helpers --------------------------------------------------------------

// auditCursorSentinel is the first-page sentinel for the
// (occurred_at_utc, id) keyset predicate. A future timestamp + the
// all-ones UUID admits every existing row through the strict-less-
// than tuple comparison.
var (
	auditCursorSentinelTime = time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC)
	auditCursorSentinelID   = uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
)

func cursorOrSentinel(c pagination.Cursor) (time.Time, uuid.UUID) {
	if c.ID == "" && c.SortValue.IsZero() {
		return auditCursorSentinelTime, auditCursorSentinelID
	}
	id, err := uuid.Parse(c.ID)
	if err != nil {
		// Malformed cursor ID — fall back to sentinel. The
		// HTTP boundary should have already rejected invalid
		// cursors via pagination.Decode; this is belt-and-braces.
		return auditCursorSentinelTime, auditCursorSentinelID
	}
	return c.SortValue, id
}

func projectAuditRows(rows []db.BuildingblocksAuditLogEntry) []AuditEventView {
	out := make([]AuditEventView, 0, len(rows))
	for _, r := range rows {
		v := AuditEventView{
			ID:            uuidString(r.ID),
			Action:        r.Action,
			ActorID:       uuidString(r.UserID),
			TenantID:      uuidString(r.TenantID),
			CorrelationID: uuidString(r.CorrelationID),
			OccurredAt:    r.OccurredAtUtc.Time.UTC(),
			DurationMs:    r.DurationMs,
			Succeeded:     r.Succeeded,
			PayloadRaw:    r.Payload,
		}
		if r.FailureReason != nil {
			v.FailureReason = *r.FailureReason
		}
		out = append(out, v)
	}
	return out
}
