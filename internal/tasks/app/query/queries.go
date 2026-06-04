// Package query holds Tasks query handlers per TDL canon. Read-side
// only — no state mutation. Mirrors the [command] facade.
package query

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/tasks/domain/workitem"
)

// ErrWorkItemNotFound surfaces when the work item ID does not exist
// in the caller's tenant scope.
var ErrWorkItemNotFound = errors.New("tasks query: work item not found")

// ----- Read models ----------------------------------------------------------

// WorkItemView is the read-side projection of a WorkItem aggregate.
// Strict CQRS (ADR 0067): query handlers return this flat view, never the
// aggregate — the port maps it straight to the wire DTO.
type WorkItemView struct {
	ID                     string
	TenantID               string
	Type                   string
	Priority               string
	State                  string
	Title                  string
	Description            string
	AssignedToMembershipID string
	AssignedByMembershipID string
	DueAt                  time.Time
	CompletedAt            time.Time
	CancelledAt            time.Time
	CancellationReason     string
	BatchID                string
	SourceModule           string
	SourceEntityType       string
	SourceEntityID         string
	CreatedAt              time.Time
	CreatedByMembershipID  string
}

// newWorkItemView projects a WorkItem aggregate into its read model.
func newWorkItemView(w *workitem.WorkItem) WorkItemView {
	src := w.Source()
	return WorkItemView{
		ID:                     w.ID().String(),
		TenantID:               w.TenantID().String(),
		Type:                   w.Type().String(),
		Priority:               w.Priority().String(),
		State:                  w.State().String(),
		Title:                  w.Title(),
		Description:            w.Description(),
		AssignedToMembershipID: w.AssignedToMembershipID(),
		AssignedByMembershipID: w.AssignedByMembershipID(),
		DueAt:                  w.DueAt(),
		CompletedAt:            w.CompletedAt(),
		CancelledAt:            w.CancelledAt(),
		CancellationReason:     w.CancellationReason(),
		BatchID:                w.BatchID(),
		SourceModule:           src.Module,
		SourceEntityType:       src.EntityType,
		SourceEntityID:         src.EntityID,
		CreatedAt:              w.CreatedAt(),
		CreatedByMembershipID:  w.CreatedByMembershipID(),
	}
}

// DashboardView is the read-side projection of the dashboard counts.
type DashboardView struct {
	Today          int
	Upcoming       int
	Overdue        int
	CompletedToday int
	TotalPending   int
}

// ----- GetWorkItem ---------------------------------------------------------

// GetWorkItemQuery selects a single work item by ID under the tenant
// scope.
type GetWorkItemQuery struct {
	TenantID   tenant.ID
	WorkItemID workitem.ID
}

// GetWorkItemHandler runs the single-work-item read.
type GetWorkItemHandler struct {
	repo workitem.Repository
}

// NewGetWorkItemHandler wires the handler.
func NewGetWorkItemHandler(repo workitem.Repository) GetWorkItemHandler {
	if repo == nil {
		panic("query: NewGetWorkItemHandler repo required")
	}
	return GetWorkItemHandler{repo: repo}
}

// Handle returns the work-item read model or [ErrWorkItemNotFound].
func (h GetWorkItemHandler) Handle(ctx context.Context, q GetWorkItemQuery) (*WorkItemView, error) {
	if q.TenantID.IsZero() {
		return nil, errors.New("tasks get_work_item: tenant id required")
	}
	if q.WorkItemID.IsZero() {
		return nil, errors.New("tasks get_work_item: work item id required")
	}
	w, err := h.repo.GetByID(ctx, q.TenantID, q.WorkItemID)
	if err != nil {
		if errors.Is(err, workitem.ErrNotFound) {
			return nil, ErrWorkItemNotFound
		}
		return nil, fmt.Errorf("tasks get_work_item: %w", err)
	}
	view := newWorkItemView(w)
	return &view, nil
}

// ----- ListWorkItems -------------------------------------------------------

// ListWorkItemsQuery carries the cursor-paginated + filter inputs.
type ListWorkItemsQuery struct {
	TenantID   tenant.ID
	Cursor     pagination.Cursor
	PageSize   int
	Filter     workitem.ListFilter
	SelfFilter string
}

// ListWorkItemsHandler runs the paginated list read.
type ListWorkItemsHandler struct {
	repo workitem.Repository
}

// NewListWorkItemsHandler wires the handler.
func NewListWorkItemsHandler(repo workitem.Repository) ListWorkItemsHandler {
	if repo == nil {
		panic("query: NewListWorkItemsHandler repo required")
	}
	return ListWorkItemsHandler{repo: repo}
}

// Handle returns the paginated work-item read-model page.
func (h ListWorkItemsHandler) Handle(ctx context.Context, q ListWorkItemsQuery) (pagination.Page[WorkItemView], error) {
	if q.TenantID.IsZero() {
		return pagination.Page[WorkItemView]{}, errors.New("tasks list: tenant id required")
	}
	filter := q.Filter
	if q.SelfFilter != "" {
		filter.SelfFilter = q.SelfFilter
	}
	page, err := h.repo.ListPage(ctx, q.TenantID, filter, q.Cursor, pagination.ClampPageSize(q.PageSize))
	if err != nil {
		return pagination.Page[WorkItemView]{}, fmt.Errorf("tasks list: %w", err)
	}
	views := make([]WorkItemView, 0, len(page.Items))
	for _, w := range page.Items {
		views = append(views, newWorkItemView(w))
	}
	return pagination.Page[WorkItemView]{
		Items:      views,
		HasMore:    page.HasMore,
		NextCursor: page.NextCursor,
	}, nil
}

// ----- Dashboard -----------------------------------------------------------

// DashboardQuery carries the dashboard counts request. When
// IncludeTeam is true the handler expands the actor's subordinate
// scope (via HierarchyReader) into the visible-membership filter;
// otherwise only the actor's own tasks are counted.
type DashboardQuery struct {
	TenantID     tenant.ID
	MembershipID string
	IncludeTeam  bool
	AsOf         time.Time
}

// DashboardHandler runs the dashboard counts query.
type DashboardHandler struct {
	repo      workitem.Repository
	hierarchy HierarchyReader
	now       func() time.Time
}

// NewDashboardHandler wires the handler. `now` is the injected
// wall-clock (Pure Domain canon — ADR 0047); nil → time.Now.
func NewDashboardHandler(repo workitem.Repository, hierarchy HierarchyReader, now func() time.Time) DashboardHandler {
	if repo == nil {
		panic("query: NewDashboardHandler repo required")
	}
	if hierarchy == nil {
		panic("query: NewDashboardHandler hierarchy required")
	}
	if now == nil {
		now = time.Now
	}
	return DashboardHandler{repo: repo, hierarchy: hierarchy, now: now}
}

// Handle returns the dashboard counts read model.
func (h DashboardHandler) Handle(ctx context.Context, q DashboardQuery) (DashboardView, error) {
	if q.TenantID.IsZero() {
		return DashboardView{}, errors.New("tasks dashboard: tenant id required")
	}
	if q.MembershipID == "" {
		return DashboardView{}, errors.New("tasks dashboard: membership id required")
	}
	asOf := q.AsOf
	if asOf.IsZero() {
		asOf = h.now().UTC()
	}
	visible := []string{q.MembershipID}
	if q.IncludeTeam {
		subs, err := h.hierarchy.ListSubordinateMembershipIDs(ctx, q.TenantID, q.MembershipID)
		if err != nil {
			return DashboardView{}, fmt.Errorf("tasks dashboard: hierarchy: %w", err)
		}
		// Hierarchy ALWAYS includes the actor — but defensively dedup.
		seen := map[string]struct{}{q.MembershipID: {}}
		for _, s := range subs {
			if _, ok := seen[s]; ok {
				continue
			}
			seen[s] = struct{}{}
			visible = append(visible, s)
		}
	}
	counts, err := h.repo.CountDashboard(ctx, q.TenantID, visible, asOf)
	if err != nil {
		return DashboardView{}, fmt.Errorf("tasks dashboard: counts: %w", err)
	}
	return DashboardView{
		Today:          counts.Today,
		Upcoming:       counts.Upcoming,
		Overdue:        counts.Overdue,
		CompletedToday: counts.CompletedToday,
		TotalPending:   counts.TotalPending,
	}, nil
}
