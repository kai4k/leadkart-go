package query

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permissionrequest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// PermissionRequestView is the wire-shape of [permissionrequest.Request]
// per Vernon IDDD ch. 4 + ADR 0046.
type PermissionRequestView struct {
	ID                    string
	TenantID              string
	RequesterMembershipID string
	Permission            string
	DurationDays          int
	Reason                string
	State                 string
	ApproverMembershipID  string
	DecidedAt             time.Time
	DecisionReason        string
	ExpiresAt             time.Time
	CreatedAt             time.Time
	UpdatedAt             time.Time
}

func projectPermissionRequest(r *permissionrequest.Request) PermissionRequestView {
	v := PermissionRequestView{
		ID:                    r.ID().String(),
		TenantID:              r.TenantID().String(),
		RequesterMembershipID: r.RequesterMembershipID().String(),
		Permission:            r.Permission().Name(),
		DurationDays:          r.DurationDays(),
		Reason:                r.Reason(),
		State:                 string(r.State()),
		DecisionReason:        r.DecisionReason(),
		DecidedAt:             r.DecidedAt(),
		ExpiresAt:             r.ExpiresAt(),
		CreatedAt:             r.CreatedAt(),
		UpdatedAt:             r.UpdatedAt(),
	}
	if id := r.ApproverMembershipID(); !id.IsZero() {
		v.ApproverMembershipID = id.String()
	}
	return v
}

// ----- GetPermissionRequest -------------------------------------------------

// GetPermissionRequestQuery returns the read shape by ID.
// TenantID is the caller's JWT scope; passed explicitly per ADR 0062.
type GetPermissionRequestQuery struct {
	TenantID  tenant.ID
	RequestID permissionrequest.ID
}

// GetPermissionRequestHandler runs the read.
type GetPermissionRequestHandler struct {
	requests permissionrequest.Repository
}

// NewGetPermissionRequestHandler wires the handler.
func NewGetPermissionRequestHandler(r permissionrequest.Repository) GetPermissionRequestHandler {
	if r == nil {
		panic("query: NewGetPermissionRequestHandler requests repository required")
	}
	return GetPermissionRequestHandler{requests: r}
}

// Handle returns the [PermissionRequestView] or [permissionrequest.ErrNotFound].
func (h GetPermissionRequestHandler) Handle(ctx context.Context, q GetPermissionRequestQuery) (PermissionRequestView, error) {
	if q.TenantID.IsZero() {
		return PermissionRequestView{}, errors.New("get_permission_request: tenant id required")
	}
	if q.RequestID.IsZero() {
		return PermissionRequestView{}, errors.New("get_permission_request: request id required")
	}
	r, err := h.requests.GetByID(ctx, q.TenantID, q.RequestID)
	if err != nil {
		return PermissionRequestView{}, fmt.Errorf("get_permission_request: %w", err)
	}
	return projectPermissionRequest(r), nil
}

// ----- ListMyPermissionRequests --------------------------------------------

// ListMyPermissionRequestsQuery returns the requester's paginated history (all states).
// TenantID is the caller's JWT scope; passed explicitly per ADR 0062.
type ListMyPermissionRequestsQuery struct {
	TenantID              tenant.ID
	RequesterMembershipID membership.ID
	Cursor                pagination.Cursor
	PageSize              int
}

// ListMyPermissionRequestsHandler runs the list.
type ListMyPermissionRequestsHandler struct {
	requests permissionrequest.Repository
}

// NewListMyPermissionRequestsHandler wires the handler.
func NewListMyPermissionRequestsHandler(r permissionrequest.Repository) ListMyPermissionRequestsHandler {
	if r == nil {
		panic("query: NewListMyPermissionRequestsHandler requests repository required")
	}
	return ListMyPermissionRequestsHandler{requests: r}
}

// Handle returns one page of requests authored by the supplied Membership.
func (h ListMyPermissionRequestsHandler) Handle(
	ctx context.Context,
	q ListMyPermissionRequestsQuery,
) (pagination.Page[PermissionRequestView], error) {
	if q.TenantID.IsZero() {
		return pagination.Page[PermissionRequestView]{},
			errors.New("list_my_permission_requests: tenant id required")
	}
	if q.RequesterMembershipID.IsZero() {
		return pagination.Page[PermissionRequestView]{},
			errors.New("list_my_permission_requests: requester membership id required")
	}
	pageSize := pagination.ClampPageSize(q.PageSize)
	page, err := h.requests.ListByRequester(ctx, q.TenantID, q.RequesterMembershipID, pageSize, q.Cursor)
	if err != nil {
		return pagination.Page[PermissionRequestView]{},
			fmt.Errorf("list_my_permission_requests: %w", err)
	}
	return projectPermissionRequestPage(page), nil
}

// ----- ListPendingPermissionRequestsForApprover ----------------------------

// ListPendingForApproverQuery returns every Pending request the supplied
// Membership can act on. TenantID is the caller's JWT scope; passed explicitly per ADR 0062.
type ListPendingForApproverQuery struct {
	TenantID             tenant.ID
	ApproverMembershipID membership.ID
	Cursor               pagination.Cursor
	PageSize             int
}

// ListPendingForApproverHandler runs the list.
type ListPendingForApproverHandler struct {
	requests permissionrequest.Repository
}

// NewListPendingForApproverHandler wires the handler.
func NewListPendingForApproverHandler(r permissionrequest.Repository) ListPendingForApproverHandler {
	if r == nil {
		panic("query: NewListPendingForApproverHandler requests repository required")
	}
	return ListPendingForApproverHandler{requests: r}
}

// Handle returns one page of Pending requests where the supplied
// Membership is the named approver.
func (h ListPendingForApproverHandler) Handle(
	ctx context.Context,
	q ListPendingForApproverQuery,
) (pagination.Page[PermissionRequestView], error) {
	if q.TenantID.IsZero() {
		return pagination.Page[PermissionRequestView]{},
			errors.New("list_pending_for_approver: tenant id required")
	}
	if q.ApproverMembershipID.IsZero() {
		return pagination.Page[PermissionRequestView]{},
			errors.New("list_pending_for_approver: approver membership id required")
	}
	pageSize := pagination.ClampPageSize(q.PageSize)
	page, err := h.requests.ListPendingApprovableBy(ctx, q.TenantID, q.ApproverMembershipID, pageSize, q.Cursor)
	if err != nil {
		return pagination.Page[PermissionRequestView]{},
			fmt.Errorf("list_pending_for_approver: %w", err)
	}
	return projectPermissionRequestPage(page), nil
}

// projectPermissionRequestPage maps a domain aggregate page to a view page.
func projectPermissionRequestPage(
	page pagination.Page[*permissionrequest.Request],
) pagination.Page[PermissionRequestView] {
	items := make([]PermissionRequestView, 0, len(page.Items))
	for _, r := range page.Items {
		items = append(items, projectPermissionRequest(r))
	}
	return pagination.Page[PermissionRequestView]{
		Items:      items,
		HasMore:    page.HasMore,
		NextCursor: page.NextCursor,
	}
}
