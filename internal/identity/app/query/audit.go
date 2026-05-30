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

// AuditEventView is one row of the audit-log read shape per
// ADR 0027 (outbox doubles as audit) + the additive
// common.audit_log_entry table from migration
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

// ListAuditEventsByTenantHandler depends on [audit.Reader] only. No
// pgxpool / pgx / sqlc imports — boundary discipline per ADR 0047
// (app/ may NOT depend on the database driver or generated row types;
// adapter implementations live behind the audit.Reader interface).
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

// ListAuditEventsByUserQuery returns paginated person-scoped audit
// events. UserID is the JWT subject (person_id), NOT a membership
// id — audit_log_entry.user_id mirrors JWT.Subject.
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

// cursorOrAuditSentinel decodes the keyset cursor into (occurred_at, id)
// tuple, falling back to the first-page sentinel from the audit
// package when the cursor is empty or malformed.
func cursorOrAuditSentinel(c pagination.Cursor) (time.Time, uuid.UUID) {
	if c.ID == "" && c.SortValue.IsZero() {
		return audit.FirstPageBefore, audit.FirstPageBeforeID
	}
	id, err := uuid.Parse(c.ID)
	if err != nil {
		// Malformed cursor ID — fall back to sentinel. HTTP boundary
		// should have already rejected invalid cursors via
		// pagination.Decode; this is belt-and-braces.
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
