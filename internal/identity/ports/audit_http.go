package ports

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/app"
	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
)

// handleListMyAuditEvents serves GET /api/v1/auth/me/activity.
//
// Returns the caller's own audit-event feed — every command the
// authenticated person executed across all their memberships.
// Per ADR 0027 + 0038 — keyset pagination on (occurred_at_utc DESC,
// id DESC).
//
// Caller derivation: user_id filter comes straight from JWT.Subject
// (= person_id). No further authorization gate — caller can always
// see their own activity (privacy default per security.md "Audit
// log access").
//
// Auth: REQUIRES authenticated JWT (RequireAuth).
func handleListMyAuditEvents(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			log.ErrorContext(r.Context(), "audit/me: missing claims in authenticated handler")
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}

		cursor, pageSize, ok := parsePaginationParams(w, r)
		if !ok {
			return
		}

		page, err := a.Queries.ListAuditEventsByUser.Handle(r.Context(), query.ListAuditEventsByUserQuery{
			UserID:   person.ID(c.Subject),
			Cursor:   cursor,
			PageSize: pageSize,
		})
		if err != nil {
			log.ErrorContext(r.Context(), "audit/me failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusOK, projectAuditEventsPage(page))
	})
}

// handleListTenantAuditEvents serves
// GET /api/v1/tenants/{tenantId}/activity.
//
// Returns the audit-event feed scoped to one tenant. Caller MUST be
// either a same-tenant admin (RequireTenantContext middleware
// validates) or a platform operator with X-Tenant-Id override per
// ADR 0039. Path UUID parse failures surface as 400.
//
// Auth: gated by RequireTenantContext at the route table; this
// handler trusts the middleware to have enforced caller↔tenant
// authorization.
func handleListTenantAuditEvents(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.PathValue("tenantId")
		tid, err := uuid.Parse(raw)
		if err != nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidTenantID,
				"tenantId path parameter must be a valid UUID")
			return
		}

		cursor, pageSize, ok := parsePaginationParams(w, r)
		if !ok {
			return
		}

		page, err := a.Queries.ListAuditEventsByTenant.Handle(r.Context(), query.ListAuditEventsByTenantQuery{
			TenantID: tenant.ID(tid.String()),
			Cursor:   cursor,
			PageSize: pageSize,
		})
		if err != nil {
			log.ErrorContext(r.Context(), "audit/tenant failed", "err", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusOK, projectAuditEventsPage(page))
	})
}

// parsePaginationParams extracts the standard ?cursor= and
// ?page_size= query parameters used by every keyset-paginated
// endpoint. Returns the decoded Cursor, the (un-clamped) page_size,
// and a "should we keep going" boolean — false means the handler
// already wrote a 400 response.
//
// Co-located here (not in pagination kit) because the package has
// no project-internal imports per its godoc; the HTTP boundary
// owns parsing.
func parsePaginationParams(w http.ResponseWriter, r *http.Request) (pagination.Cursor, int, bool) {
	cursor, err := pagination.Decode(r.URL.Query().Get("cursor"))
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidCursor,
			"cursor failed to decode; retry without it to fetch first page")
		return pagination.Cursor{}, 0, false
	}
	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	return cursor, pageSize, true
}

// projectAuditEventsPage maps the query-layer Page to the wire shape.
func projectAuditEventsPage(p pagination.Page[query.AuditEventView]) ListAuditEventsResponse {
	events := make([]AuditEventDto, 0, len(p.Items))
	for _, v := range p.Items {
		dto := AuditEventDto{
			ID:            v.ID,
			Action:        v.Action,
			ActorID:       v.ActorID,
			TenantID:      v.TenantID,
			CorrelationID: v.CorrelationID,
			OccurredAt:    v.OccurredAt,
			DurationMs:    v.DurationMs,
			Succeeded:     v.Succeeded,
			FailureReason: v.FailureReason,
		}
		if len(v.PayloadRaw) > 0 {
			// Payload is stored as jsonb in Postgres → returned by
			// pgx as []byte containing valid JSON. Re-emit as
			// json.RawMessage so the wire body inlines the JSON
			// structure (vs encoding the bytes as a base64 string).
			dto.Payload = json.RawMessage(v.PayloadRaw)
		}
		events = append(events, dto)
	}
	return ListAuditEventsResponse{
		Events:     events,
		HasMore:    p.HasMore,
		NextCursor: p.NextCursor,
	}
}

