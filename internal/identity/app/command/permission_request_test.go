package command_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/clock"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permissionrequest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// fakePermissionRequestRepo is the minimum [permissionrequest.Repository]
// surface the elevation handlers exercise. Tracks Pending uniqueness
// in-memory so the Add → ErrPendingRequestExists path can be exercised.
type fakePermissionRequestRepo struct {
	requests map[permissionrequest.ID]*permissionrequest.Request
	pending  map[string]permissionrequest.ID // (requester|permission) → id
}

func newFakePermissionRequestRepo() *fakePermissionRequestRepo {
	return &fakePermissionRequestRepo{
		requests: make(map[permissionrequest.ID]*permissionrequest.Request),
		pending:  make(map[string]permissionrequest.ID),
	}
}

func pendingKey(m membership.ID, perm string) string {
	return m.String() + "|" + perm
}

func (r *fakePermissionRequestRepo) Add(_ context.Context, req *permissionrequest.Request) error {
	if req.State() == permissionrequest.StatePending {
		k := pendingKey(req.RequesterMembershipID(), req.Permission().Name())
		if _, exists := r.pending[k]; exists {
			return permissionrequest.ErrPendingRequestExists
		}
		r.pending[k] = req.ID()
	}
	r.requests[req.ID()] = req
	req.PullEvents()
	return nil
}

func (r *fakePermissionRequestRepo) UpdateByID(
	_ context.Context,
	id permissionrequest.ID,
	fn func(*permissionrequest.Request) (bool, error),
) error {
	x, ok := r.requests[id]
	if !ok {
		return permissionrequest.ErrNotFound
	}
	prevState := x.State()
	prevPerm := x.Permission().Name()
	prevRequester := x.RequesterMembershipID()
	commit, err := fn(x)
	if err != nil {
		return err
	}
	if !commit {
		return nil
	}
	// If state moved away from pending, free the (membership, permission)
	// slot so a future Add can succeed.
	if prevState == permissionrequest.StatePending && x.State() != permissionrequest.StatePending {
		delete(r.pending, pendingKey(prevRequester, prevPerm))
	}
	x.PullEvents()
	return nil
}

func (r *fakePermissionRequestRepo) GetByID(_ context.Context, id permissionrequest.ID) (*permissionrequest.Request, error) {
	x, ok := r.requests[id]
	if !ok {
		return nil, permissionrequest.ErrNotFound
	}
	return x, nil
}

func (r *fakePermissionRequestRepo) GetPendingForMembership(_ context.Context, m membership.ID) ([]*permissionrequest.Request, error) {
	var out []*permissionrequest.Request
	for _, x := range r.requests {
		if x.RequesterMembershipID() == m && x.State() == permissionrequest.StatePending {
			out = append(out, x)
		}
	}
	return out, nil
}

func (r *fakePermissionRequestRepo) ListPendingApprovableBy(_ context.Context, approver membership.ID, pageSize int, _ pagination.Cursor) (pagination.Page[*permissionrequest.Request], error) {
	var items []*permissionrequest.Request
	for _, x := range r.requests {
		if x.ApproverMembershipID() == approver && x.State() == permissionrequest.StatePending {
			items = append(items, x)
		}
	}
	return pagination.BuildPage(items, pageSize, func(req *permissionrequest.Request) pagination.Cursor {
		return pagination.Cursor{SortValue: req.CreatedAt(), ID: req.ID().String()}
	}), nil
}

func (r *fakePermissionRequestRepo) ListByRequester(_ context.Context, requester membership.ID, pageSize int, _ pagination.Cursor) (pagination.Page[*permissionrequest.Request], error) {
	var items []*permissionrequest.Request
	for _, x := range r.requests {
		if x.RequesterMembershipID() == requester {
			items = append(items, x)
		}
	}
	return pagination.BuildPage(items, pageSize, func(req *permissionrequest.Request) pagination.Cursor {
		return pagination.Cursor{SortValue: req.CreatedAt(), ID: req.ID().String()}
	}), nil
}

var _ permissionrequest.Repository = (*fakePermissionRequestRepo)(nil)

// fakeMembershipRepoForPermReq holds Memberships by id. Only the
// methods exercised by the elevation handlers are functional.
type fakeMembershipRepoForPermReq struct {
	memberships map[membership.ID]*membership.Membership
}

func newFakeMembershipRepoForPermReq() *fakeMembershipRepoForPermReq {
	return &fakeMembershipRepoForPermReq{memberships: make(map[membership.ID]*membership.Membership)}
}

func (r *fakeMembershipRepoForPermReq) Add(_ context.Context, m *membership.Membership) error {
	r.memberships[m.ID()] = m
	return nil
}

func (r *fakeMembershipRepoForPermReq) UpdateByID(_ context.Context, id membership.ID, fn func(*membership.Membership) (bool, error)) error {
	m, ok := r.memberships[id]
	if !ok {
		return membership.ErrNotFound
	}
	commit, err := fn(m)
	if err != nil {
		return err
	}
	_ = commit
	m.PullEvents()
	return nil
}

func (r *fakeMembershipRepoForPermReq) GetByID(_ context.Context, id membership.ID) (*membership.Membership, error) {
	m, ok := r.memberships[id]
	if !ok {
		return nil, membership.ErrNotFound
	}
	return m, nil
}

func (r *fakeMembershipRepoForPermReq) GetActiveForPerson(_ context.Context, _ person.ID) (*membership.Membership, error) {
	return nil, membership.ErrNotFound
}

func (r *fakeMembershipRepoForPermReq) ListForTenant(_ context.Context, _ tenant.ID) ([]*membership.Membership, error) {
	return nil, nil
}

func (r *fakeMembershipRepoForPermReq) ListForTenantPage(_ context.Context, _ time.Time, _ string, _ int) ([]*membership.Membership, error) {
	return nil, nil
}

func (r *fakeMembershipRepoForPermReq) ListAllForPerson(_ context.Context, _ person.ID) ([]*membership.Membership, error) {
	return nil, nil
}

func (r *fakeMembershipRepoForPermReq) HasActiveSuperAdmin(_ context.Context, _ tenant.ID) (bool, error) {
	return false, nil
}

var _ membership.Repository = (*fakeMembershipRepoForPermReq)(nil)

func freshMembershipForPermReq(t *testing.T) *membership.Membership {
	t.Helper()
	m, err := membership.New(
		membership.ID(ids.NewV7().String()),
		person.ID(ids.NewV7().String()),
		tenant.ID(ids.NewV7().String()),
		membership.ID(""),
	)
	if err != nil {
		t.Fatalf("membership.New: %v", err)
	}
	_ = m.PullEvents()
	return m
}

func fixedTimeFn() func() time.Time {
	t := time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
	return func() time.Time { return t }
}

// ----- RequestPermissionElevation -----------------------------------------

func TestRequestPermissionElevation_HappyPath(t *testing.T) {
	t.Parallel()
	clock.Set(time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC))
	t.Cleanup(clock.Reset)

	reqs := newFakePermissionRequestRepo()
	mems := newFakeMembershipRepoForPermReq()
	requester := freshMembershipForPermReq(t)
	managerID := membership.ID(ids.NewV7().String())
	_ = requester.AssignManager(managerID)
	_ = requester.PullEvents()
	_ = mems.Add(t.Context(), requester)

	h := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn())
	out, err := h.Handle(t.Context(), command.RequestPermissionElevationCommand{
		RequesterMembershipID: requester.ID(),
		Permission:            permission.FromConstant(permission.IdentityPermissions.Users.Create),
		Reason:                "need to onboard 5 users for monthly sales drive",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out.RequestID.IsZero() {
		t.Fatal("expected non-empty RequestID")
	}
	if out.ApproverMembershipID != managerID {
		t.Errorf("ApproverMembershipID = %v, want %v", out.ApproverMembershipID, managerID)
	}
}

func TestRequestPermissionElevation_NonExistentMembership(t *testing.T) {
	t.Parallel()
	reqs := newFakePermissionRequestRepo()
	mems := newFakeMembershipRepoForPermReq()
	h := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn())

	_, err := h.Handle(t.Context(), command.RequestPermissionElevationCommand{
		RequesterMembershipID: membership.ID(ids.NewV7().String()),
		Permission:            permission.FromConstant(permission.IdentityPermissions.Users.Create),
		Reason:                "need to onboard 5 users for monthly sales drive",
	})
	if !errors.Is(err, command.ErrUserNotFound) {
		t.Fatalf("err = %v, want ErrUserNotFound", err)
	}
}

func TestRequestPermissionElevation_RejectsDuplicatePending(t *testing.T) {
	t.Parallel()
	clock.Set(time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC))
	t.Cleanup(clock.Reset)

	reqs := newFakePermissionRequestRepo()
	mems := newFakeMembershipRepoForPermReq()
	requester := freshMembershipForPermReq(t)
	_ = mems.Add(t.Context(), requester)
	h := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn())

	cmd := command.RequestPermissionElevationCommand{
		RequesterMembershipID: requester.ID(),
		Permission:            permission.FromConstant(permission.IdentityPermissions.Users.Create),
		Reason:                "need to onboard 5 users for monthly sales drive",
	}
	if _, err := h.Handle(t.Context(), cmd); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	_, err := h.Handle(t.Context(), cmd)
	if !errors.Is(err, command.ErrPermissionRequestPendingExists) {
		t.Fatalf("second Handle err = %v, want ErrPermissionRequestPendingExists", err)
	}
}

// ----- ApprovePermissionRequest -------------------------------------------

func TestApprovePermissionRequest_HappyPath(t *testing.T) {
	t.Parallel()
	clock.Set(time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC))
	t.Cleanup(clock.Reset)

	reqs := newFakePermissionRequestRepo()
	mems := newFakeMembershipRepoForPermReq()
	requester := freshMembershipForPermReq(t)
	manager := freshMembershipForPermReq(t)
	_ = requester.AssignManager(manager.ID())
	_ = requester.PullEvents()
	_ = mems.Add(t.Context(), requester)
	_ = mems.Add(t.Context(), manager)

	submitH := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn())
	out, err := submitH.Handle(t.Context(), command.RequestPermissionElevationCommand{
		RequesterMembershipID: requester.ID(),
		Permission:            permission.FromConstant(permission.IdentityPermissions.Users.Create),
		Reason:                "need to onboard 5 users for monthly sales drive",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	approveH := command.NewApprovePermissionRequestHandler(reqs, mems, fixedTimeFn())
	if err := approveH.Handle(t.Context(), command.ApprovePermissionRequestCommand{
		RequestID:            out.RequestID,
		ApproverMembershipID: manager.ID(),
		DecisionReason:       "approved for the rollout",
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}

	// Request state should be Approved.
	req, _ := reqs.GetByID(t.Context(), out.RequestID)
	if req.State() != permissionrequest.StateApproved {
		t.Errorf("State = %v, want approved", req.State())
	}

	// Membership overlay should hold a time-bound grant.
	granted := requester.GrantedPermissions()
	if len(granted) != 1 {
		t.Fatalf("granted = %d, want 1", len(granted))
	}
	if !granted[0].Permission.Equal(permission.FromConstant(permission.IdentityPermissions.Users.Create)) {
		t.Errorf("granted permission mismatch")
	}
	if granted[0].ExpiresAt.IsZero() {
		t.Error("ExpiresAt should be set (time-bound grant)")
	}
}

func TestApprovePermissionRequest_BlocksSelfApproval(t *testing.T) {
	t.Parallel()
	clock.Set(time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC))
	t.Cleanup(clock.Reset)

	reqs := newFakePermissionRequestRepo()
	mems := newFakeMembershipRepoForPermReq()
	requester := freshMembershipForPermReq(t)
	_ = mems.Add(t.Context(), requester)

	submitH := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn())
	out, _ := submitH.Handle(t.Context(), command.RequestPermissionElevationCommand{
		RequesterMembershipID: requester.ID(),
		Permission:            permission.FromConstant(permission.IdentityPermissions.Users.Create),
		Reason:                "need to onboard 5 users for monthly sales drive",
	})

	approveH := command.NewApprovePermissionRequestHandler(reqs, mems, fixedTimeFn())
	err := approveH.Handle(t.Context(), command.ApprovePermissionRequestCommand{
		RequestID:            out.RequestID,
		ApproverMembershipID: requester.ID(), // self-approval
	})
	if !errors.Is(err, command.ErrPermissionRequestSelfApproval) {
		t.Fatalf("err = %v, want ErrPermissionRequestSelfApproval", err)
	}
}

func TestApprovePermissionRequest_MissingManagerRequiresPlatform(t *testing.T) {
	t.Parallel()
	clock.Set(time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC))
	t.Cleanup(clock.Reset)

	reqs := newFakePermissionRequestRepo()
	mems := newFakeMembershipRepoForPermReq()
	requester := freshMembershipForPermReq(t)
	// NO manager assigned — orphan / root membership.
	_ = mems.Add(t.Context(), requester)

	submitH := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn())
	out, _ := submitH.Handle(t.Context(), command.RequestPermissionElevationCommand{
		RequesterMembershipID: requester.ID(),
		Permission:            permission.FromConstant(permission.IdentityPermissions.Users.Create),
		Reason:                "need to onboard 5 users for monthly sales drive",
	})

	approveH := command.NewApprovePermissionRequestHandler(reqs, mems, fixedTimeFn())
	// Non-platform random approver should be Forbidden.
	err := approveH.Handle(t.Context(), command.ApprovePermissionRequestCommand{
		RequestID:            out.RequestID,
		ApproverMembershipID: membership.ID(ids.NewV7().String()),
		IsPlatformOperator:   false,
	})
	if !errors.Is(err, command.ErrPermissionRequestForbidden) {
		t.Fatalf("non-platform err = %v, want ErrPermissionRequestForbidden", err)
	}

	// Platform operator should be allowed.
	if err := approveH.Handle(t.Context(), command.ApprovePermissionRequestCommand{
		RequestID:            out.RequestID,
		ApproverMembershipID: membership.ID(ids.NewV7().String()),
		IsPlatformOperator:   true,
	}); err != nil {
		t.Fatalf("platform Approve: %v", err)
	}
}

// ----- DenyPermissionRequest ----------------------------------------------

func TestDenyPermissionRequest_HappyPath(t *testing.T) {
	t.Parallel()
	clock.Set(time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC))
	t.Cleanup(clock.Reset)

	reqs := newFakePermissionRequestRepo()
	mems := newFakeMembershipRepoForPermReq()
	requester := freshMembershipForPermReq(t)
	manager := freshMembershipForPermReq(t)
	_ = requester.AssignManager(manager.ID())
	_ = requester.PullEvents()
	_ = mems.Add(t.Context(), requester)
	_ = mems.Add(t.Context(), manager)

	submitH := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn())
	out, _ := submitH.Handle(t.Context(), command.RequestPermissionElevationCommand{
		RequesterMembershipID: requester.ID(),
		Permission:            permission.FromConstant(permission.IdentityPermissions.Users.Create),
		Reason:                "need to onboard 5 users for monthly sales drive",
	})

	denyH := command.NewDenyPermissionRequestHandler(reqs, mems, fixedTimeFn())
	if err := denyH.Handle(t.Context(), command.DenyPermissionRequestCommand{
		RequestID:            out.RequestID,
		ApproverMembershipID: manager.ID(),
		DecisionReason:       "scope too broad; rethink and re-submit",
	}); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	req, _ := reqs.GetByID(t.Context(), out.RequestID)
	if req.State() != permissionrequest.StateDenied {
		t.Errorf("State = %v, want denied", req.State())
	}
}

// ----- CancelPermissionRequest --------------------------------------------

func TestCancelPermissionRequest_OnlyRequesterCanCancel(t *testing.T) {
	t.Parallel()
	clock.Set(time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC))
	t.Cleanup(clock.Reset)

	reqs := newFakePermissionRequestRepo()
	mems := newFakeMembershipRepoForPermReq()
	requester := freshMembershipForPermReq(t)
	_ = mems.Add(t.Context(), requester)

	submitH := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn())
	out, _ := submitH.Handle(t.Context(), command.RequestPermissionElevationCommand{
		RequesterMembershipID: requester.ID(),
		Permission:            permission.FromConstant(permission.IdentityPermissions.Users.Create),
		Reason:                "need to onboard 5 users for monthly sales drive",
	})

	cancelH := command.NewCancelPermissionRequestHandler(reqs, fixedTimeFn())
	// Different caller — collapses to 404 enumeration-safe.
	err := cancelH.Handle(t.Context(), command.CancelPermissionRequestCommand{
		RequestID:             out.RequestID,
		RequesterMembershipID: membership.ID(ids.NewV7().String()),
	})
	if !errors.Is(err, command.ErrPermissionRequestNotFound) {
		t.Fatalf("cross-caller err = %v, want ErrPermissionRequestNotFound", err)
	}

	// True requester can cancel.
	if err := cancelH.Handle(t.Context(), command.CancelPermissionRequestCommand{
		RequestID:             out.RequestID,
		RequesterMembershipID: requester.ID(),
	}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	req, _ := reqs.GetByID(t.Context(), out.RequestID)
	if req.State() != permissionrequest.StateCancelled {
		t.Errorf("State = %v, want cancelled", req.State())
	}
}
