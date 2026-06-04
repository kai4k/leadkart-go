// Package ports holds the Tasks module's inbound HTTP + subscriber
// adapters per ADR 0002 + the TDL hexagonal terminology canon.
//
// HTTP handlers translate request/response shapes into [app.Application]
// command + query calls. Per ADR 0046 the wire shapes here are
// MIRRORED in api/openapi.yaml — adding a route or field requires a
// paired spec update (CI-gated by the route-spec-drift test).
package ports

import "time"

// ----- Work-item read DTOs --------------------------------------------------

// WorkItemDto is the wire shape returned by GET /api/v1/tasks/work-items/{id}.
type WorkItemDto struct {
	ID                     string    `json:"id"`
	TenantID               string    `json:"tenant_id"`
	Type                   string    `json:"type"`
	Priority               string    `json:"priority"`
	State                  string    `json:"state"`
	Title                  string    `json:"title"`
	Description            string    `json:"description,omitzero"`
	AssignedToMembershipID string    `json:"assigned_to_membership_id"`
	AssignedByMembershipID string    `json:"assigned_by_membership_id"`
	DueAt                  time.Time `json:"due_at"`
	CompletedAt            time.Time `json:"completed_at,omitzero"`
	CancelledAt            time.Time `json:"cancelled_at,omitzero"`
	CancellationReason     string    `json:"cancellation_reason,omitzero"`
	BatchID                string    `json:"batch_id,omitzero"`
	SourceModule           string    `json:"source_module,omitzero"`
	SourceEntityType       string    `json:"source_entity_type,omitzero"`
	SourceEntityID         string    `json:"source_entity_id,omitzero"`
	CreatedAt              time.Time `json:"created_at"`
	CreatedByMembershipID  string    `json:"created_by_membership_id"`
}

// ListWorkItemsResponse is the wire shape returned by GET /api/v1/tasks/work-items.
type ListWorkItemsResponse struct {
	Items      []WorkItemDto `json:"items"`
	HasMore    bool          `json:"has_more"`
	NextCursor string        `json:"next_cursor,omitzero"`
}

// ----- Mutation requests + responses ---------------------------------------

// CreateWorkItemRequest is the body for POST /api/v1/tasks/work-items.
type CreateWorkItemRequest struct {
	Type                   string    `json:"type"`
	Priority               string    `json:"priority,omitzero"`
	Title                  string    `json:"title"`
	Description            string    `json:"description,omitzero"`
	AssignedToMembershipID string    `json:"assigned_to_membership_id"`
	BatchID                string    `json:"batch_id,omitzero"`
	DueAt                  time.Time `json:"due_at"`
}

// CreateWorkItemResponse returns the new work-item ID.
type CreateWorkItemResponse struct {
	WorkItemID string `json:"work_item_id"`
}

// CancelWorkItemRequest is the body for POST /api/v1/tasks/work-items/{id}/cancel.
type CancelWorkItemRequest struct {
	Reason string `json:"reason"`
}

// ReassignWorkItemRequest is the body for POST /api/v1/tasks/work-items/{id}/reassign.
type ReassignWorkItemRequest struct {
	NewAssigneeMembershipID string `json:"new_assignee_membership_id"`
	Reason                  string `json:"reason,omitzero"`
}

// DashboardResponse is the wire shape for GET /api/v1/tasks/work-items/dashboard.
type DashboardResponse struct {
	Today          int `json:"today"`
	Upcoming       int `json:"upcoming"`
	Overdue        int `json:"overdue"`
	CompletedToday int `json:"completed_today"`
	TotalPending   int `json:"total_pending"`
}

// errorResponse is the wire shape for non-success responses.
type errorResponse struct {
	Type    string              `json:"type,omitzero"`
	Title   string              `json:"title,omitzero"`
	Status  int                 `json:"status,omitzero"`
	Detail  string              `json:"detail,omitzero"`
	Error   string              `json:"error"`
	Message string              `json:"message,omitzero"`
	Errors  map[string][]string `json:"errors,omitzero"`
}
