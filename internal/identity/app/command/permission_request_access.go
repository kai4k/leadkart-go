package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permissionrequest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Who may SEE a permission request and which requests are in an approver's
// queue are the same authority question as who may decide one (ADR 0069 A6):
// both read through the facts loader the approval commands use and ask the
// policy ([authority.Actor.MayDeny]), so visibility, the queue and the 403 on
// approve can never disagree. These are reads (no lock, no transaction); they
// live beside the loader because the loader is unexported.

// approverQueueWindow bounds one queue call: at most this many pending requests
// of the tenant are examined, and each DISTINCT requester in that window is
// projected once (membership, roles, person: three reads), so one call costs at
// most 3 x approverQueueWindow reads however many requests or approvers there
// are. A call may return fewer items than the window (the policy filtered the
// rest out); next_cursor continues after the last request examined.
const approverQueueWindow = 100

// AuthorizePermissionRequestViewCommand asks whether Actor may read one request.
type AuthorizePermissionRequestViewCommand struct {
	TenantID  tenant.ID
	RequestID permissionrequest.ID
	// Actor is built once by the port via authn.AuthorityFrom; the zero value sees nothing.
	Actor authority.Actor
}

// AuthorizePermissionRequestViewHandler decides read visibility of one request:
// the requester, and anyone who may deny it (the policy's approver eligibility).
type AuthorizePermissionRequestViewHandler struct {
	requests permissionrequest.Repository
	facts    authorityFacts
	now      func() time.Time
}

// NewAuthorizePermissionRequestViewHandler wires the handler.
func NewAuthorizePermissionRequestViewHandler(requests permissionrequest.Repository, m membership.Repository, r role.Repository, p person.Repository, now func() time.Time) AuthorizePermissionRequestViewHandler {
	if requests == nil || m == nil || r == nil || p == nil {
		panic("command: NewAuthorizePermissionRequestViewHandler requests, memberships, roles and persons required")
	}
	if now == nil {
		now = time.Now
	}
	return AuthorizePermissionRequestViewHandler{requests: requests, facts: authorityFacts{memberships: m, roles: r, persons: p}, now: now}
}

// Handle returns nil when the request exists in the tenant and Actor may see it;
// every other outcome (missing, other tenant, not eligible, facts that cannot be
// loaded) is [ErrPermissionRequestNotFound], indistinguishable from a missing id.
func (h AuthorizePermissionRequestViewHandler) Handle(ctx context.Context, cmd AuthorizePermissionRequestViewCommand) error {
	if cmd.TenantID.IsZero() || cmd.RequestID.IsZero() {
		return errors.New("authorize_permission_request_view: tenant id and request id required")
	}
	if !cmd.Actor.Valid() {
		return ErrPermissionRequestNotFound
	}
	req, err := h.requests.GetByID(ctx, cmd.TenantID, cmd.RequestID)
	if err != nil {
		if errors.Is(err, permissionrequest.ErrNotFound) {
			return ErrPermissionRequestNotFound
		}
		return fmt.Errorf("authorize_permission_request_view: %w", err)
	}
	if req.RequesterMembershipID() == cmd.Actor.MembershipID() {
		return nil
	}
	_, requester, err := h.facts.memberByID(ctx, cmd.TenantID, req.RequesterMembershipID(), h.now())
	if err != nil {
		return ErrPermissionRequestNotFound
	}
	if cmd.Actor.MayDeny(requester) != nil {
		return ErrPermissionRequestNotFound
	}
	return nil
}

// ListDecidablePermissionRequestsQuery asks for one window of the approver queue.
type ListDecidablePermissionRequestsQuery struct {
	TenantID tenant.ID
	Cursor   pagination.Cursor
	PageSize int
	// Actor is built once by the port via authn.AuthorityFrom; the zero value is refused.
	Actor authority.Actor
}

// ListDecidablePermissionRequestsResult is one window of the approver queue.
type ListDecidablePermissionRequestsResult struct {
	Page pagination.Page[*permissionrequest.Request]
}

// ListDecidablePermissionRequestsHandler lists the pending requests the caller
// may decide under the policy: exactly those [authority.Actor.MayDeny] accepts,
// so none of them answers 403 on approve or deny for authority reasons.
type ListDecidablePermissionRequestsHandler struct {
	requests permissionrequest.Repository
	facts    authorityFacts
	now      func() time.Time
}

// NewListDecidablePermissionRequestsHandler wires the handler.
func NewListDecidablePermissionRequestsHandler(requests permissionrequest.Repository, m membership.Repository, r role.Repository, p person.Repository, now func() time.Time) ListDecidablePermissionRequestsHandler {
	if requests == nil || m == nil || r == nil || p == nil {
		panic("command: NewListDecidablePermissionRequestsHandler requests, memberships, roles and persons required")
	}
	if now == nil {
		now = time.Now
	}
	return ListDecidablePermissionRequestsHandler{requests: requests, facts: authorityFacts{memberships: m, roles: r, persons: p}, now: now}
}

// Handle examines one bounded window of the tenant's pending requests (see
// [approverQueueWindow]) and keeps those the caller may decide.
func (h ListDecidablePermissionRequestsHandler) Handle(ctx context.Context, q ListDecidablePermissionRequestsQuery) (ListDecidablePermissionRequestsResult, error) {
	if q.TenantID.IsZero() {
		return ListDecidablePermissionRequestsResult{}, errors.New("list_decidable_permission_requests: tenant id required")
	}
	if !q.Actor.Valid() {
		return ListDecidablePermissionRequestsResult{}, authority.ErrNoAuthority
	}
	window := min(pagination.ClampPageSize(q.PageSize), approverQueueWindow)
	page, err := h.requests.ListPendingInTenant(ctx, q.TenantID, window, q.Cursor)
	if err != nil {
		return ListDecidablePermissionRequestsResult{}, fmt.Errorf("list_decidable_permission_requests: %w", err)
	}
	now := h.now()
	members := make(map[membership.ID]*authority.Member, len(page.Items))
	out := pagination.Page[*permissionrequest.Request]{
		Items:      make([]*permissionrequest.Request, 0, len(page.Items)),
		HasMore:    page.HasMore,
		NextCursor: page.NextCursor,
	}
	for _, req := range page.Items {
		rid := req.RequesterMembershipID()
		if rid == q.Actor.MembershipID() {
			continue // never one's own request
		}
		m, seen := members[rid]
		if !seen {
			if _, loaded, lerr := h.facts.memberByID(ctx, q.TenantID, rid, now); lerr == nil {
				m = &loaded
			}
			members[rid] = m // nil when the requester cannot be projected: not decidable
		}
		if m != nil && q.Actor.MayDeny(*m) == nil {
			out.Items = append(out.Items, req)
		}
	}
	return ListDecidablePermissionRequestsResult{Page: out}, nil
}
