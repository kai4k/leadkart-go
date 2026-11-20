package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
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
// are examined; the distinct requesters in that window, their roles and their
// persons are each read ONCE for all of them, so one call costs a fixed number
// of reads in one transaction however many requests or approvers there are. A call may return fewer items than the window (the
// policy filtered the rest out); next_cursor continues after the last request
// examined.
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

// DecidableRequest is one request in the approver queue: the caller may deny
// it, and CanApprove says whether they may also approve it as asked (the
// stored permission within their authority: approve succeeds iff CanApprove,
// barring a state change since the list was read).
type DecidableRequest struct {
	Request    *permissionrequest.Request
	CanApprove bool
}

// ListDecidablePermissionRequestsResult is one window of the approver queue.
type ListDecidablePermissionRequestsResult struct {
	Page pagination.Page[DecidableRequest]
}

// ListDecidablePermissionRequestsHandler lists the pending requests the caller
// may decide under the policy: exactly those [authority.Actor.MayDeny] accepts,
// so none of them answers 403 on deny for authority reasons, each flagged with
// whether approving it would also pass ([authority.Actor.MayApprove]).
type ListDecidablePermissionRequestsHandler struct {
	uow      pg.UnitOfWork
	requests permissionrequest.Repository
	facts    authorityFacts
	now      func() time.Time
}

// NewListDecidablePermissionRequestsHandler wires the handler.
func NewListDecidablePermissionRequestsHandler(uow pg.UnitOfWork, requests permissionrequest.Repository, m membership.Repository, r role.Repository, p person.Repository, now func() time.Time) ListDecidablePermissionRequestsHandler {
	if uow == nil || requests == nil || m == nil || r == nil || p == nil {
		panic("command: NewListDecidablePermissionRequestsHandler uow, requests, memberships, roles and persons required")
	}
	if now == nil {
		now = time.Now
	}
	return ListDecidablePermissionRequestsHandler{uow: uow, requests: requests, facts: authorityFacts{memberships: m, roles: r, persons: p}, now: now}
}

// Handle examines one bounded window of the tenant's pending requests (see
// [approverQueueWindow]) and keeps those the caller may decide.
//
// Cost: a caller whose token says they can decide nothing
// ([authority.Actor.MayDecideAny]) gets an empty page without a single read.
// Otherwise the caller is re-read once (the fresh actor, three reads) and, when
// the stored facts say the same, the rest follows. A decider below the Admin tier sees only the
// requests of members who report to them (the filter is in the SQL), so the
// window is never spent on requests they can never decide. Everything else runs
// in ONE read transaction: one list, one membership read per distinct requester,
// one role read and one person read for all of them.
func (h ListDecidablePermissionRequestsHandler) Handle(ctx context.Context, q ListDecidablePermissionRequestsQuery) (ListDecidablePermissionRequestsResult, error) {
	if q.TenantID.IsZero() {
		return ListDecidablePermissionRequestsResult{}, errors.New("list_decidable_permission_requests: tenant id required")
	}
	if !q.Actor.Valid() {
		return ListDecidablePermissionRequestsResult{}, authority.ErrNoAuthority
	}
	out := pagination.Page[DecidableRequest]{Items: []DecidableRequest{}}
	if !q.Actor.MayDecideAny(q.TenantID) {
		return ListDecidablePermissionRequestsResult{Page: out}, nil
	}
	window := min(pagination.ClampPageSize(q.PageSize), approverQueueWindow)
	now := h.now()
	err := h.uow.WithinTx(tenancy.WithID(ctx, tenancy.ID(q.TenantID.String())), pg.TxScopeTenant, func(ctx context.Context) error {
		// The caller is decided on the stored facts, like the approve and deny
		// commands decide them, so can_approve is what approve will answer.
		actor, err := h.facts.freshActor(ctx, q.TenantID, q.Actor, now)
		if err != nil {
			return err
		}
		if !actor.MayDecideAny(q.TenantID) {
			return nil
		}
		var reportsTo membership.ID // the whole tenant
		if !actor.DecidesWholeTenant(q.TenantID) {
			reportsTo = actor.MembershipID()
		}
		page, err := h.requests.ListPendingInTenant(ctx, q.TenantID, reportsTo, window, q.Cursor)
		if err != nil {
			return fmt.Errorf("list pending: %w", err)
		}
		out.HasMore, out.NextCursor = page.HasMore, page.NextCursor
		members, err := h.requesters(ctx, q.TenantID, actor, page.Items, now)
		if err != nil {
			return err
		}
		for _, req := range page.Items {
			m, ok := members[req.RequesterMembershipID()]
			if !ok || actor.MayDeny(m) != nil {
				continue // never one's own request; not decidable; requester cannot be projected
			}
			out.Items = append(out.Items, DecidableRequest{
				Request:    req,
				CanApprove: permission.IsRequestable(req.Permission().Name()) && actor.MayApprove(m, req.Permission().Name()) == nil,
			})
		}
		return nil
	})
	if err != nil {
		return ListDecidablePermissionRequestsResult{}, fmt.Errorf("list_decidable_permission_requests: %w", err)
	}
	return ListDecidablePermissionRequestsResult{Page: out}, nil
}

// requesters projects each DISTINCT requester of reqs once (the caller's own
// requests excluded); a requester who is gone or whose roles do not load is
// absent from the result.
func (h ListDecidablePermissionRequestsHandler) requesters(ctx context.Context, tenantID tenant.ID, actor authority.Actor, reqs []*permissionrequest.Request, now time.Time) (map[membership.ID]authority.Member, error) {
	var ids []membership.ID
	seen := make(map[membership.ID]struct{}, len(reqs))
	for _, req := range reqs {
		rid := req.RequesterMembershipID()
		if _, dup := seen[rid]; dup || rid == actor.MembershipID() {
			continue
		}
		seen[rid] = struct{}{}
		ids = append(ids, rid)
	}
	loaded, err := h.facts.memberships.GetByIDs(ctx, tenantID, ids)
	if err != nil {
		return nil, fmt.Errorf("load requesters: %w", err)
	}
	results, err := h.facts.projectBatch(ctx, tenantID, loaded, now)
	if err != nil {
		return nil, err
	}
	out := make(map[membership.ID]authority.Member, len(results))
	for i, r := range results {
		if r.err == nil {
			out[loaded[i].ID()] = r.member
		}
	}
	return out, nil
}
