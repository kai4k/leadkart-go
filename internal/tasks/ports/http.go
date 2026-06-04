package ports

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/ports/authn"
	"github.com/leadkart/leadkart-go/internal/tasks/app"
	"github.com/leadkart/leadkart-go/internal/tasks/app/command"
	"github.com/leadkart/leadkart-go/internal/tasks/app/query"
	"github.com/leadkart/leadkart-go/internal/tasks/domain/workitem"
)

// tenantFromContext extracts the caller's tenant ID from ctx (bound
// by authn middleware).
func tenantFromContext(r *http.Request) (tenant.ID, bool) {
	tid, ok := tenancy.FromContext(r.Context())
	if !ok || tid == "" {
		return tenant.ID(""), false
	}
	return tenant.ID(tid), true
}

// AddRoutes registers Tasks HTTP handlers on mux per Mat Ryer 2024
// canon.
//
// Routes registered here (all under /api/v1/tasks/):
//
//	POST   /api/v1/tasks/work-items                       create manual task
//	GET    /api/v1/tasks/work-items                       cursor-paginated list
//	GET    /api/v1/tasks/work-items/dashboard             per-membership / team counts
//	GET    /api/v1/tasks/work-items/{workItemId}          single task read
//	POST   /api/v1/tasks/work-items/{workItemId}/start    flip → in_progress
//	POST   /api/v1/tasks/work-items/{workItemId}/complete terminal complete
//	POST   /api/v1/tasks/work-items/{workItemId}/cancel   terminal cancel
//	POST   /api/v1/tasks/work-items/{workItemId}/reassign hierarchy-gated reassign
//
// Per-handler permission gates (ADR 0036 closed-set catalog):
//
//	read:     tasks.work_items.read
//	manage:   tasks.work_items.manage (create, start, complete, cancel)
//	reassign: tasks.work_items.reassign
//
// "Only my tasks" filter is enforced INLINE — callers WITHOUT
// tasks.work_items.read_all see a SelfFilter-narrowed list.
func AddRoutes(mux *http.ServeMux, log *slog.Logger, a app.Application, verifier authn.Verifier, stampValidator authn.StampValidator) {
	if verifier == nil || stampValidator == nil {
		return
	}
	read := authn.RequirePermission(verifier, stampValidator, permission.IdentityPermissions.Tasks.WorkItems.Read)
	manage := authn.RequirePermission(verifier, stampValidator, permission.IdentityPermissions.Tasks.WorkItems.Manage)
	reassign := authn.RequirePermission(verifier, stampValidator, permission.IdentityPermissions.Tasks.WorkItems.Reassign)

	mux.Handle("POST /api/v1/tasks/work-items", manage(handleCreateWorkItem(log, a)))
	mux.Handle("GET /api/v1/tasks/work-items", read(handleListWorkItems(log, a)))
	mux.Handle("GET /api/v1/tasks/work-items/dashboard", read(handleDashboard(log, a)))
	mux.Handle("GET /api/v1/tasks/work-items/{workItemId}", read(handleGetWorkItem(log, a)))
	mux.Handle("POST /api/v1/tasks/work-items/{workItemId}/start", manage(handleStartWorkItem(log, a)))
	mux.Handle("POST /api/v1/tasks/work-items/{workItemId}/complete", manage(handleCompleteWorkItem(log, a)))
	mux.Handle("POST /api/v1/tasks/work-items/{workItemId}/cancel", manage(handleCancelWorkItem(log, a)))
	mux.Handle("POST /api/v1/tasks/work-items/{workItemId}/reassign", reassign(handleReassignWorkItem(log, a)))
}

// ----- Handlers -------------------------------------------------------------

func handleCreateWorkItem(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		tid, ok := tenantFromContext(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		var req CreateWorkItemRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidBody, "request body is not valid JSON")
			return
		}
		taskType, err := workitem.ParseType(req.Type)
		if err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidType, err.Error())
			return
		}
		priority, err := workitem.ParsePriority(req.Priority)
		if err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidPriority, err.Error())
			return
		}
		if _, err := uuid.Parse(req.AssignedToMembershipID); err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidMembershipID, "assigned_to_membership_id must be a UUID")
			return
		}
		if req.BatchID != "" {
			if _, err := uuid.Parse(req.BatchID); err != nil {
				writeError(w, http.StatusBadRequest, errCodeInvalidBody, "batch_id must be a UUID")
				return
			}
		}
		if req.DueAt.IsZero() {
			writeError(w, http.StatusBadRequest, errCodeInvalidDueAt, "due_at required")
			return
		}
		out, err := a.Commands.CreateWorkItem.Handle(r.Context(), command.CreateWorkItemCommand{
			TenantID:               tid,
			Type:                   taskType,
			Priority:               priority,
			Title:                  req.Title,
			Description:            req.Description,
			AssignedToMembershipID: req.AssignedToMembershipID,
			AssignedByMembershipID: c.MembershipID,
			BatchID:                req.BatchID,
			DueAt:                  req.DueAt,
		})
		switch {
		case errors.Is(err, command.ErrInvalidAssignee):
			writeError(w, http.StatusUnprocessableEntity, errCodeAssigneeInactive,
				"assignee membership is not active in this tenant")
			return
		case err != nil && errors.Is(err, workitem.ErrInvalid):
			writeError(w, http.StatusUnprocessableEntity, errCodeInvalidBody, err.Error())
			return
		case err != nil:
			log.ErrorContext(r.Context(), "tasks: create work item", "err", err)
			writeError(w, http.StatusInternalServerError, errCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusCreated, CreateWorkItemResponse{WorkItemID: out.WorkItemID.String()})
	})
}

func handleListWorkItems(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		tid, ok := tenantFromContext(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		params := r.URL.Query()
		cursor, err := pagination.Decode(params.Get("cursor"))
		if err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidCursor, err.Error())
			return
		}
		pageSize := 0
		if raw := strings.TrimSpace(params.Get("page_size")); raw != "" {
			if n, perr := strconv.Atoi(raw); perr == nil {
				pageSize = n
			}
		}
		filter, err := parseListFilter(params)
		if err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidState, err.Error())
			return
		}

		// "Only my tasks" gate.
		selfFilter := ""
		if !c.IsSuperUser && !c.IsPlatform &&
			!slices.Contains(c.Permissions, permission.IdentityPermissions.Tasks.WorkItems.ReadAll) {
			selfFilter = c.MembershipID
		}
		// Privilege-probe rejection (mirrors CRM H8 pattern).
		if selfFilter != "" && filter.AssignedToMembershipID != "" && filter.AssignedToMembershipID != selfFilter {
			writeError(w, http.StatusForbidden, errCodeForbidden,
				"caller lacks tasks.work_items.read_all; ?assignee must equal the caller's own membership")
			return
		}

		page, err := a.Queries.ListWorkItems.Handle(r.Context(), query.ListWorkItemsQuery{
			TenantID: tid, Cursor: cursor, PageSize: pageSize, Filter: filter, SelfFilter: selfFilter,
		})
		if err != nil {
			log.ErrorContext(r.Context(), "tasks: list work items", "err", err)
			writeError(w, http.StatusInternalServerError, errCodeInternalError, "")
			return
		}
		out := ListWorkItemsResponse{
			Items:      make([]WorkItemDto, 0, len(page.Items)),
			HasMore:    page.HasMore,
			NextCursor: page.NextCursor,
		}
		for _, w := range page.Items {
			out.Items = append(out.Items, workItemToDto(w))
		}
		writeJSON(w, http.StatusOK, out)
	})
}

func handleGetWorkItem(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := tenantFromContext(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		id, ok := parseWorkItemID(w, r)
		if !ok {
			return
		}
		got, err := a.Queries.GetWorkItem.Handle(r.Context(), query.GetWorkItemQuery{TenantID: tid, WorkItemID: id})
		switch {
		case errors.Is(err, query.ErrWorkItemNotFound):
			writeError(w, http.StatusNotFound, errCodeWorkItemNotFound, "")
			return
		case err != nil:
			log.ErrorContext(r.Context(), "tasks: get work item", "err", err)
			writeError(w, http.StatusInternalServerError, errCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusOK, workItemToDto(*got))
	})
}

func handleStartWorkItem(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := tenantFromContext(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		id, ok := parseWorkItemID(w, r)
		if !ok {
			return
		}
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		err := a.Commands.StartWorkItem.Handle(r.Context(), command.StartWorkItemCommand{
			TenantID: tid, WorkItemID: id, ActorID: c.MembershipID,
		})
		mapMutationErr(w, log, r, err, "start work item")
	})
}

func handleCompleteWorkItem(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := tenantFromContext(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		id, ok := parseWorkItemID(w, r)
		if !ok {
			return
		}
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		err := a.Commands.CompleteWorkItem.Handle(r.Context(), command.CompleteWorkItemCommand{
			TenantID: tid, WorkItemID: id, ActorID: c.MembershipID,
		})
		mapMutationErr(w, log, r, err, "complete work item")
	})
}

func handleCancelWorkItem(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := tenantFromContext(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		id, ok := parseWorkItemID(w, r)
		if !ok {
			return
		}
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		var req CancelWorkItemRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidBody, "request body is not valid JSON")
			return
		}
		if strings.TrimSpace(req.Reason) == "" {
			writeError(w, http.StatusUnprocessableEntity, errCodeReasonRequired, "reason is required for cancel")
			return
		}
		err := a.Commands.CancelWorkItem.Handle(r.Context(), command.CancelWorkItemCommand{
			TenantID: tid, WorkItemID: id, ActorID: c.MembershipID, Reason: req.Reason,
		})
		mapMutationErr(w, log, r, err, "cancel work item")
	})
}

func handleReassignWorkItem(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tid, ok := tenantFromContext(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		id, ok := parseWorkItemID(w, r)
		if !ok {
			return
		}
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		var req ReassignWorkItemRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidBody, "request body is not valid JSON")
			return
		}
		if _, err := uuid.Parse(req.NewAssigneeMembershipID); err != nil {
			writeError(w, http.StatusBadRequest, errCodeInvalidMembershipID, "new_assignee_membership_id must be a UUID")
			return
		}
		err := a.Commands.ReassignWorkItem.Handle(r.Context(), command.ReassignWorkItemCommand{
			TenantID: tid, WorkItemID: id,
			NewAssigneeMembershipID:  req.NewAssigneeMembershipID,
			ReassignedByMembershipID: c.MembershipID,
			Reason:                   req.Reason,
		})
		switch {
		case errors.Is(err, command.ErrForbiddenReassign):
			writeError(w, http.StatusForbidden, errCodeReassignForbidden,
				"target assignee outside actor's subordinate scope")
		case errors.Is(err, command.ErrInvalidAssignee):
			writeError(w, http.StatusUnprocessableEntity, errCodeAssigneeInactive,
				"new assignee membership is not active in this tenant")
		default:
			mapMutationErr(w, log, r, err, "reassign work item")
		}
	})
}

func handleDashboard(log *slog.Logger, a app.Application) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, ok := authn.ClaimsFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		tid, ok := tenantFromContext(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, errCodeUnauthenticated, "")
			return
		}
		includeTeam := strings.EqualFold(r.URL.Query().Get("include_team"), "true")
		if includeTeam && !c.IsSuperUser && !c.IsPlatform &&
			!slices.Contains(c.Permissions, permission.IdentityPermissions.Tasks.WorkItems.ReadAll) {
			writeError(w, http.StatusForbidden, errCodeForbidden,
				"caller lacks tasks.work_items.read_all for team view")
			return
		}
		counts, err := a.Queries.Dashboard.Handle(r.Context(), query.DashboardQuery{
			TenantID: tid, MembershipID: c.MembershipID,
			IncludeTeam: includeTeam, AsOf: time.Now().UTC(),
		})
		if err != nil {
			log.ErrorContext(r.Context(), "tasks: dashboard", "err", err)
			writeError(w, http.StatusInternalServerError, errCodeInternalError, "")
			return
		}
		writeJSON(w, http.StatusOK, DashboardResponse{
			Today:          counts.Today,
			Upcoming:       counts.Upcoming,
			Overdue:        counts.Overdue,
			CompletedToday: counts.CompletedToday,
			TotalPending:   counts.TotalPending,
		})
	})
}

// ----- Helpers --------------------------------------------------------------

func parseWorkItemID(w http.ResponseWriter, r *http.Request) (workitem.ID, bool) {
	raw := r.PathValue("workItemId")
	if _, err := uuid.Parse(raw); err != nil {
		writeError(w, http.StatusBadRequest, errCodeInvalidWorkItemID, "workItemId must be a UUID")
		return "", false
	}
	return workitem.ID(raw), true
}

func parseListFilter(params url.Values) (workitem.ListFilter, error) {
	f := workitem.ListFilter{}
	if s := strings.TrimSpace(params.Get("state")); s != "" {
		st, err := workitem.ParseState(s)
		if err != nil {
			return workitem.ListFilter{}, err
		}
		f.State = st
	}
	if s := strings.TrimSpace(params.Get("type")); s != "" {
		tp, err := workitem.ParseType(s)
		if err != nil {
			return workitem.ListFilter{}, err
		}
		f.Type = tp
	}
	if s := strings.TrimSpace(params.Get("priority")); s != "" {
		p, err := workitem.ParsePriority(s)
		if err != nil {
			return workitem.ListFilter{}, err
		}
		f.Priority = p
	}
	if s := strings.TrimSpace(params.Get("assignee")); s != "" {
		if _, err := uuid.Parse(s); err != nil {
			return workitem.ListFilter{}, errors.New("assignee must be a UUID")
		}
		f.AssignedToMembershipID = s
	}
	if s := strings.TrimSpace(params.Get("batch_id")); s != "" {
		if _, err := uuid.Parse(s); err != nil {
			return workitem.ListFilter{}, errors.New("batch_id must be a UUID")
		}
		f.BatchID = s
	}
	if s := strings.TrimSpace(params.Get("due_before")); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return workitem.ListFilter{}, errors.New("due_before must be RFC3339")
		}
		f.DueBefore = t
	}
	if s := strings.TrimSpace(params.Get("due_after")); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return workitem.ListFilter{}, errors.New("due_after must be RFC3339")
		}
		f.DueAfter = t
	}
	return f, nil
}

func mapMutationErr(w http.ResponseWriter, log *slog.Logger, r *http.Request, err error, op string) {
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, command.ErrWorkItemNotFound):
		writeError(w, http.StatusNotFound, errCodeWorkItemNotFound, "")
	case errors.Is(err, command.ErrWorkItemTerminal):
		writeError(w, http.StatusConflict, errCodeWorkItemTerminal, "")
	case errors.Is(err, workitem.ErrInvalid):
		writeError(w, http.StatusUnprocessableEntity, errCodeInvalidBody, err.Error())
	case errors.Is(err, workitem.ErrConflict):
		writeError(w, http.StatusConflict, errCodeWorkItemTerminal, "")
	default:
		log.ErrorContext(r.Context(), "tasks: "+op, "err", err)
		writeError(w, http.StatusInternalServerError, errCodeInternalError, "")
	}
}

func workItemToDto(v query.WorkItemView) WorkItemDto {
	return WorkItemDto{
		ID:                     v.ID,
		TenantID:               v.TenantID,
		Type:                   v.Type,
		Priority:               v.Priority,
		State:                  v.State,
		Title:                  v.Title,
		Description:            v.Description,
		AssignedToMembershipID: v.AssignedToMembershipID,
		AssignedByMembershipID: v.AssignedByMembershipID,
		DueAt:                  v.DueAt,
		CompletedAt:            v.CompletedAt,
		CancelledAt:            v.CancelledAt,
		CancellationReason:     v.CancellationReason,
		BatchID:                v.BatchID,
		SourceModule:           v.SourceModule,
		SourceEntityType:       v.SourceEntityType,
		SourceEntityID:         v.SourceEntityID,
		CreatedAt:              v.CreatedAt,
		CreatedByMembershipID:  v.CreatedByMembershipID,
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{
		Type:    "https://leadkart.api/errors/" + code,
		Title:   http.StatusText(status),
		Status:  status,
		Detail:  message,
		Error:   code,
		Message: message,
	})
}
