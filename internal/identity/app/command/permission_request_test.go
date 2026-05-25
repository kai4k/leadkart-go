package command_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership/membershiptest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permissionrequest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permissionrequest/permissionrequesttest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)


// The permission-request-side fake lives in
// internal/identity/domain/permissionrequest/permissionrequesttest/
// per TDL Wild Workouts canon — co-located with the aggregate it
// fakes. newFakePermissionRequestRepo is preserved as a one-line alias
// so existing tests don't need rewriting.
func newFakePermissionRequestRepo() *permissionrequesttest.FakeRepository { return permissionrequesttest.NewFakeRepository() }

// The membership-side fake lives in
// internal/identity/domain/membership/membershiptest/ per TDL Wild
// Workouts canon. newFakeMembershipRepoForPermReq is preserved as a
// one-line alias keying off the same shared fake the user_management
// tests use.
func newFakeMembershipRepoForPermReq() *membershiptest.FakeRepository { return membershiptest.NewFakeRepository() }

func freshMembershipForPermReq(t *testing.T) *membership.Membership {
	t.Helper()
	m, err := membership.New(
		membership.ID(ids.NewV7().String()),
		person.ID(ids.NewV7().String()),
		tenant.ID(ids.NewV7().String()),
		membership.ID(""),
		testNow,
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

	reqs := newFakePermissionRequestRepo()
	mems := newFakeMembershipRepoForPermReq()
	requester := freshMembershipForPermReq(t)
	managerID := membership.ID(ids.NewV7().String())
	_ = requester.AssignManager(managerID, testNow) // arch-test:ignore-err - test fixture setup
	_ = requester.PullEvents()
	_ = mems.Add(t.Context(), requester) // arch-test:ignore-err - test fixture setup

	h := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn(), func() permissionrequest.ID { return permissionrequest.ID(ids.NewV7().String()) })
	out, err := h.Handle(t.Context(), command.RequestPermissionElevationCommand{
		TenantID:              requester.TenantID(),
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
	h := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn(), func() permissionrequest.ID { return permissionrequest.ID(ids.NewV7().String()) })

	_, err := h.Handle(t.Context(), command.RequestPermissionElevationCommand{
		TenantID:              tenant.ID(ids.NewV7().String()),
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

	reqs := newFakePermissionRequestRepo()
	mems := newFakeMembershipRepoForPermReq()
	requester := freshMembershipForPermReq(t)
	_ = mems.Add(t.Context(), requester) // arch-test:ignore-err - test fixture setup
	h := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn(), func() permissionrequest.ID { return permissionrequest.ID(ids.NewV7().String()) })

	cmd := command.RequestPermissionElevationCommand{
		TenantID:              requester.TenantID(),
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

	reqs := newFakePermissionRequestRepo()
	mems := newFakeMembershipRepoForPermReq()
	requester := freshMembershipForPermReq(t)
	manager := freshMembershipForPermReq(t)
	_ = requester.AssignManager(manager.ID(), testNow) // arch-test:ignore-err - test fixture setup
	_ = requester.PullEvents()
	_ = mems.Add(t.Context(), requester) // arch-test:ignore-err - test fixture setup
	_ = mems.Add(t.Context(), manager) // arch-test:ignore-err - test fixture setup

	submitH := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn(), func() permissionrequest.ID { return permissionrequest.ID(ids.NewV7().String()) })
	out, err := submitH.Handle(t.Context(), command.RequestPermissionElevationCommand{
		TenantID:              requester.TenantID(),
		RequesterMembershipID: requester.ID(),
		Permission:            permission.FromConstant(permission.IdentityPermissions.Users.Create),
		Reason:                "need to onboard 5 users for monthly sales drive",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	approveH := command.NewApprovePermissionRequestHandler(reqs, mems, fixedTimeFn(), ids.NewV7)
	if err := approveH.Handle(t.Context(), command.ApprovePermissionRequestCommand{
		TenantID:             requester.TenantID(),
		RequestID:            out.RequestID,
		ApproverMembershipID: manager.ID(),
		DecisionReason:       "approved for the rollout",
	}); err != nil {
		t.Fatalf("approve: %v", err)
	}

	// Request state should be Approved.
	req, _ := reqs.GetByID(t.Context(), requester.TenantID(), out.RequestID)
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

	reqs := newFakePermissionRequestRepo()
	mems := newFakeMembershipRepoForPermReq()
	requester := freshMembershipForPermReq(t)
	_ = mems.Add(t.Context(), requester) // arch-test:ignore-err - test fixture setup

	submitH := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn(), func() permissionrequest.ID { return permissionrequest.ID(ids.NewV7().String()) })
	out, _ := submitH.Handle(t.Context(), command.RequestPermissionElevationCommand{
		TenantID:              requester.TenantID(),
		RequesterMembershipID: requester.ID(),
		Permission:            permission.FromConstant(permission.IdentityPermissions.Users.Create),
		Reason:                "need to onboard 5 users for monthly sales drive",
	})

	approveH := command.NewApprovePermissionRequestHandler(reqs, mems, fixedTimeFn(), ids.NewV7)
	err := approveH.Handle(t.Context(), command.ApprovePermissionRequestCommand{
		TenantID:             requester.TenantID(),
		RequestID:            out.RequestID,
		ApproverMembershipID: requester.ID(), // self-approval
	})
	if !errors.Is(err, command.ErrPermissionRequestSelfApproval) {
		t.Fatalf("err = %v, want ErrPermissionRequestSelfApproval", err)
	}
}

func TestApprovePermissionRequest_MissingManagerRequiresPlatform(t *testing.T) {
	t.Parallel()

	reqs := newFakePermissionRequestRepo()
	mems := newFakeMembershipRepoForPermReq()
	requester := freshMembershipForPermReq(t)
	// NO manager assigned — orphan / root membership.
	_ = mems.Add(t.Context(), requester) // arch-test:ignore-err - test fixture setup

	submitH := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn(), func() permissionrequest.ID { return permissionrequest.ID(ids.NewV7().String()) })
	out, _ := submitH.Handle(t.Context(), command.RequestPermissionElevationCommand{
		TenantID:              requester.TenantID(),
		RequesterMembershipID: requester.ID(),
		Permission:            permission.FromConstant(permission.IdentityPermissions.Users.Create),
		Reason:                "need to onboard 5 users for monthly sales drive",
	})

	approveH := command.NewApprovePermissionRequestHandler(reqs, mems, fixedTimeFn(), ids.NewV7)
	// Non-platform random approver should be Forbidden.
	err := approveH.Handle(t.Context(), command.ApprovePermissionRequestCommand{
		TenantID:             requester.TenantID(),
		RequestID:            out.RequestID,
		ApproverMembershipID: membership.ID(ids.NewV7().String()),
		IsPlatformOperator:   false,
	})
	if !errors.Is(err, command.ErrPermissionRequestForbidden) {
		t.Fatalf("non-platform err = %v, want ErrPermissionRequestForbidden", err)
	}

	// Platform operator should be allowed.
	if err := approveH.Handle(t.Context(), command.ApprovePermissionRequestCommand{
		TenantID:             requester.TenantID(),
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

	reqs := newFakePermissionRequestRepo()
	mems := newFakeMembershipRepoForPermReq()
	requester := freshMembershipForPermReq(t)
	manager := freshMembershipForPermReq(t)
	_ = requester.AssignManager(manager.ID(), testNow) // arch-test:ignore-err - test fixture setup
	_ = requester.PullEvents()
	_ = mems.Add(t.Context(), requester) // arch-test:ignore-err - test fixture setup
	_ = mems.Add(t.Context(), manager) // arch-test:ignore-err - test fixture setup

	submitH := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn(), func() permissionrequest.ID { return permissionrequest.ID(ids.NewV7().String()) })
	out, _ := submitH.Handle(t.Context(), command.RequestPermissionElevationCommand{
		TenantID:              requester.TenantID(),
		RequesterMembershipID: requester.ID(),
		Permission:            permission.FromConstant(permission.IdentityPermissions.Users.Create),
		Reason:                "need to onboard 5 users for monthly sales drive",
	})

	denyH := command.NewDenyPermissionRequestHandler(reqs, mems, fixedTimeFn())
	if err := denyH.Handle(t.Context(), command.DenyPermissionRequestCommand{
		TenantID:             requester.TenantID(),
		RequestID:            out.RequestID,
		ApproverMembershipID: manager.ID(),
		DecisionReason:       "scope too broad; rethink and re-submit",
	}); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	req, _ := reqs.GetByID(t.Context(), requester.TenantID(), out.RequestID)
	if req.State() != permissionrequest.StateDenied {
		t.Errorf("State = %v, want denied", req.State())
	}
}

// ----- CancelPermissionRequest --------------------------------------------

func TestCancelPermissionRequest_OnlyRequesterCanCancel(t *testing.T) {
	t.Parallel()

	reqs := newFakePermissionRequestRepo()
	mems := newFakeMembershipRepoForPermReq()
	requester := freshMembershipForPermReq(t)
	_ = mems.Add(t.Context(), requester) // arch-test:ignore-err - test fixture setup

	submitH := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn(), func() permissionrequest.ID { return permissionrequest.ID(ids.NewV7().String()) })
	out, _ := submitH.Handle(t.Context(), command.RequestPermissionElevationCommand{
		TenantID:              requester.TenantID(),
		RequesterMembershipID: requester.ID(),
		Permission:            permission.FromConstant(permission.IdentityPermissions.Users.Create),
		Reason:                "need to onboard 5 users for monthly sales drive",
	})

	cancelH := command.NewCancelPermissionRequestHandler(reqs, fixedTimeFn())
	// Different caller — collapses to 404 enumeration-safe.
	err := cancelH.Handle(t.Context(), command.CancelPermissionRequestCommand{
		TenantID:              requester.TenantID(),
		RequestID:             out.RequestID,
		RequesterMembershipID: membership.ID(ids.NewV7().String()),
	})
	if !errors.Is(err, command.ErrPermissionRequestNotFound) {
		t.Fatalf("cross-caller err = %v, want ErrPermissionRequestNotFound", err)
	}

	// True requester can cancel.
	if err := cancelH.Handle(t.Context(), command.CancelPermissionRequestCommand{
		TenantID:              requester.TenantID(),
		RequestID:             out.RequestID,
		RequesterMembershipID: requester.ID(),
	}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	req, _ := reqs.GetByID(t.Context(), requester.TenantID(), out.RequestID)
	if req.State() != permissionrequest.StateCancelled {
		t.Errorf("State = %v, want cancelled", req.State())
	}
}
