package query

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/audit"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// AuditEventView is one read-side row from common.audit_log_entry
// (migration 20260507000001, ADR 0027). OccurredAt is also the keyset
// sort column. Payload is raw JSONB; HTTP layer surfaces it as-is.
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

// ListAuditEventsByTenantQuery returns paginated tenant-scoped audit events.
// Cursor walks (occurred_at_utc DESC, id DESC) per ADR 0038.
// HTTP layer gates: caller must be platform operator or same-tenant admin (ADR 0039).
type ListAuditEventsByTenantQuery struct {
	TenantID tenant.ID
	Cursor   pagination.Cursor
	PageSize int
}

// ListAuditEventsByTenantHandler depends on [audit.Reader] only; no
// pgxpool/pgx/sqlc imports per ADR 0047 boundary discipline.
type ListAuditEventsByTenantHandler struct {
	reader audit.Reader
}

// NewListAuditEventsByTenantHandler wires the handler.
func NewListAuditEventsByTenantHandler(reader audit.Reader) ListAuditEventsByTenantHandler {
	if reader == nil {
		panic("query: NewListAuditEventsByTenantHandler reader required")
	}
	return ListAuditEventsByTenantHandler{reader: reader}
}

// Handle returns one page of tenant-scoped events.
func (h ListAuditEventsByTenantHandler) Handle(ctx context.Context, q ListAuditEventsByTenantQuery) (pagination.Page[AuditEventView], error) {
	if q.TenantID.IsZero() {
		return pagination.Page[AuditEventView]{}, errors.New("list_audit_by_tenant: tenant id required")
	}
	pageSize := pagination.ClampPageSize(q.PageSize)
	beforeAt, beforeID := cursorOrAuditSentinel(q.Cursor)

	tenantUUID, err := uuid.Parse(q.TenantID.String())
	if err != nil {
		return pagination.Page[AuditEventView]{}, fmt.Errorf("list_audit_by_tenant: parse tenant id: %w", err)
	}

	//nolint:gosec // pageSize capped at 200 by ClampPageSize
	entries, err := h.reader.ListByTenant(ctx, tenantUUID, beforeAt, beforeID, int32(pageSize+1))
	if err != nil {
		return pagination.Page[AuditEventView]{}, fmt.Errorf("list_audit_by_tenant: %w", err)
	}

	views := entriesToViews(entries)
	return pagination.BuildPage(views, pageSize, func(v AuditEventView) pagination.Cursor {
		return pagination.Cursor{SortValue: v.OccurredAt, ID: v.ID}
	}), nil
}

// ----- ListAuditEventsByUser -----------------------------------------------

// ListAuditEventsByUserQuery returns paginated person-scoped audit events.
// UserID is the JWT Subject (person_id), not a membership id.
type ListAuditEventsByUserQuery struct {
	UserID   person.ID
	Cursor   pagination.Cursor
	PageSize int
}

// ListAuditEventsByUserHandler depends on [audit.Reader] only.
type ListAuditEventsByUserHandler struct {
	reader audit.Reader
}

// NewListAuditEventsByUserHandler wires the handler.
func NewListAuditEventsByUserHandler(reader audit.Reader) ListAuditEventsByUserHandler {
	if reader == nil {
		panic("query: NewListAuditEventsByUserHandler reader required")
	}
	return ListAuditEventsByUserHandler{reader: reader}
}

// Handle returns one page of person-scoped events.
func (h ListAuditEventsByUserHandler) Handle(ctx context.Context, q ListAuditEventsByUserQuery) (pagination.Page[AuditEventView], error) {
	if q.UserID.IsZero() {
		return pagination.Page[AuditEventView]{}, errors.New("list_audit_by_user: user id required")
	}
	pageSize := pagination.ClampPageSize(q.PageSize)
	beforeAt, beforeID := cursorOrAuditSentinel(q.Cursor)

	userUUID, err := uuid.Parse(q.UserID.String())
	if err != nil {
		return pagination.Page[AuditEventView]{}, fmt.Errorf("list_audit_by_user: parse user id: %w", err)
	}

	//nolint:gosec // pageSize capped at 200 by ClampPageSize
	entries, err := h.reader.ListByUser(ctx, userUUID, beforeAt, beforeID, int32(pageSize+1))
	if err != nil {
		return pagination.Page[AuditEventView]{}, fmt.Errorf("list_audit_by_user: %w", err)
	}

	views := entriesToViews(entries)
	return pagination.BuildPage(views, pageSize, func(v AuditEventView) pagination.Cursor {
		return pagination.Cursor{SortValue: v.OccurredAt, ID: v.ID}
	}), nil
}

// ----- helpers --------------------------------------------------------------

// cursorOrAuditSentinel decodes the keyset cursor to (occurred_at, id),
// falling back to the audit package's first-page sentinel when empty or malformed.
func cursorOrAuditSentinel(c pagination.Cursor) (time.Time, uuid.UUID) {
	if c.ID == "" && c.SortValue.IsZero() {
		return audit.FirstPageBefore, audit.FirstPageBeforeID
	}
	id, err := uuid.Parse(c.ID)
	if err != nil {
		// Malformed cursor ID — fall back to sentinel (belt-and-braces;
		// HTTP boundary should already reject invalid cursors).
		return audit.FirstPageBefore, audit.FirstPageBeforeID
	}
	return c.SortValue, id
}

func entriesToViews(entries []audit.Entry) []AuditEventView {
	out := make([]AuditEventView, 0, len(entries))
	for _, e := range entries {
		v := AuditEventView{
			ID:            uuidStringOrEmpty(e.ID),
			Action:        e.Action,
			ActorID:       uuidStringOrEmpty(e.UserID),
			TenantID:      uuidStringOrEmpty(e.TenantID),
			CorrelationID: uuidStringOrEmpty(e.CorrelationID),
			OccurredAt:    e.OccurredAtUTC,
			DurationMs:    e.Duration.Milliseconds(),
			Succeeded:     e.Succeeded,
			FailureReason: e.FailureReason,
			PayloadRaw:    e.Payload,
		}
		out = append(out, v)
	}
	return out
}

func uuidStringOrEmpty(id uuid.UUID) string {
	if id == uuid.Nil {
		return ""
	}
	return id.String()
}
