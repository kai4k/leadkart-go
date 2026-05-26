package command_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pagination"
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

// =============================================================================
// B2 — additional branch coverage per ADR 0062 §6 (handler-unit MANY).
//
// Existing tests above cover happy paths + a handful of rejections. The blocks
// below close the remaining branches:
//
//   - input validation tables for all four handlers (TenantID/RequestID/Membership
//     isZero rejections),
//   - non-NotFound repository error wrapping for loads,
//   - aggregate-invariant rejections propagated unchanged (short reason etc.),
//   - non-Pending state translations,
//   - approver-not-manager-and-not-platform Forbidden,
//   - compensating-state behaviour when Step 2 (membership grant) fails after
//     Step 1 (Request.Approve) committed,
//   - cross-requester cancel enumeration safety,
//   - default DurationDays fallback,
//   - ErrPendingRequestExists vs generic Add error wrap.
//
// Inline wrappers `b2PermReqRepo` + `b2MemRepo` add error-injection that the
// shared per-aggregate fakes don't expose. Names are scoped to this file (b2*)
// per the concurrent-agent coordination convention.
// =============================================================================

// ----- b2 inline wrappers --------------------------------------------------

// b2PermReqRepo wraps the shared permissionrequest FakeRepository with
// per-call error injection. Used for non-NotFound load + persist error
// wrapping tests; production-shape contract still flows through the
// embedded fake.
type b2PermReqRepo struct {
	*permissionrequesttest.FakeRepository

	// errOnGetByID forces GetByID to return this error on the NEXT call.
	// Consumed-on-use (set back to nil after the first hit).
	errOnGetByID error
	// errOnAdd forces Add to return this error on the NEXT call.
	errOnAdd error
	// errOnUpdateByID forces UpdateByID to return this error on the NEXT
	// call (mimics persist-time failure after the mutate closure runs).
	errOnUpdateByID error
}

func newB2PermReqRepo() *b2PermReqRepo {
	return &b2PermReqRepo{FakeRepository: permissionrequesttest.NewFakeRepository()}
}

func (r *b2PermReqRepo) GetByID(ctx context.Context, tid tenant.ID, id permissionrequest.ID) (*permissionrequest.Request, error) {
	if r.errOnGetByID != nil {
		err := r.errOnGetByID
		r.errOnGetByID = nil
		return nil, err
	}
	return r.FakeRepository.GetByID(ctx, tid, id)
}

func (r *b2PermReqRepo) Add(ctx context.Context, req *permissionrequest.Request) error {
	if r.errOnAdd != nil {
		err := r.errOnAdd
		r.errOnAdd = nil
		return err
	}
	return r.FakeRepository.Add(ctx, req)
}

func (r *b2PermReqRepo) UpdateByID(ctx context.Context, tid tenant.ID, id permissionrequest.ID, fn func(*permissionrequest.Request) (bool, error)) error {
	if r.errOnUpdateByID != nil {
		err := r.errOnUpdateByID
		r.errOnUpdateByID = nil
		return err
	}
	return r.FakeRepository.UpdateByID(ctx, tid, id, fn)
}

// b2MemRepo wraps the shared membership FakeRepository with per-call error
// injection on GetByID + UpdateByID. Used to exercise non-NotFound load
// errors and the compensating-state branch when the Step-2 grant fails
// after Step-1 Approve committed.
type b2MemRepo struct {
	*membershiptest.FakeRepository

	errOnGetByID    error
	errOnUpdateByID error
}

func newB2MemRepo() *b2MemRepo {
	return &b2MemRepo{FakeRepository: membershiptest.NewFakeRepository()}
}

func (r *b2MemRepo) GetByID(ctx context.Context, tid tenant.ID, id membership.ID) (*membership.Membership, error) {
	if r.errOnGetByID != nil {
		err := r.errOnGetByID
		r.errOnGetByID = nil
		return nil, err
	}
	return r.FakeRepository.GetByID(ctx, tid, id)
}

func (r *b2MemRepo) UpdateByID(ctx context.Context, tid tenant.ID, id membership.ID, fn func(*membership.Membership) (bool, error)) error {
	if r.errOnUpdateByID != nil {
		err := r.errOnUpdateByID
		r.errOnUpdateByID = nil
		return err
	}
	return r.FakeRepository.UpdateByID(ctx, tid, id, fn)
}

// errB2Permreq is the synthetic non-typed error used for "non-NotFound
// wrapped" branches. Detection in tests uses errors.Is — every wrapper
// MUST preserve it via %w.
var errB2Permreq = errors.New("b2: synthetic infrastructure failure")

// ----- RequestPermissionElevation — additional branches -------------------

func TestRequestPermissionElevation_InputValidation(t *testing.T) {
	t.Parallel()
	tid := tenant.ID(ids.NewV7().String())
	mid := membership.ID(ids.NewV7().String())
	perm := permission.FromConstant(permission.IdentityPermissions.Users.Create)

	cases := []struct {
		name string
		cmd  command.RequestPermissionElevationCommand
	}{
		{
			name: "tenant id required",
			cmd: command.RequestPermissionElevationCommand{
				TenantID:              tenant.ID(""),
				RequesterMembershipID: mid,
				Permission:            perm,
				Reason:                "need to onboard 5 users for monthly sales drive",
			},
		},
		{
			name: "requester membership id required",
			cmd: command.RequestPermissionElevationCommand{
				TenantID:              tid,
				RequesterMembershipID: membership.ID(""),
				Permission:            perm,
				Reason:                "need to onboard 5 users for monthly sales drive",
			},
		},
		{
			name: "permission required",
			cmd: command.RequestPermissionElevationCommand{
				TenantID:              tid,
				RequesterMembershipID: mid,
				Permission:            nil,
				Reason:                "need to onboard 5 users for monthly sales drive",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := command.NewRequestPermissionElevationHandler(
				newFakePermissionRequestRepo(),
				newFakeMembershipRepoForPermReq(),
				fixedTimeFn(),
				func() permissionrequest.ID { return permissionrequest.ID(ids.NewV7().String()) },
			)
			_, err := h.Handle(t.Context(), tc.cmd)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestRequestPermissionElevation_DefaultDurationApplied(t *testing.T) {
	t.Parallel()

	reqs := newFakePermissionRequestRepo()
	mems := newFakeMembershipRepoForPermReq()
	requester := freshMembershipForPermReq(t)
	_ = mems.Add(t.Context(), requester) // arch-test:ignore-err - test fixture setup

	h := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn(),
		func() permissionrequest.ID { return permissionrequest.ID(ids.NewV7().String()) })
	out, err := h.Handle(t.Context(), command.RequestPermissionElevationCommand{
		TenantID:              requester.TenantID(),
		RequesterMembershipID: requester.ID(),
		Permission:            permission.FromConstant(permission.IdentityPermissions.Users.Create),
		// DurationDays omitted → handler MUST apply DefaultDurationDays.
		Reason: "need to onboard 5 users for monthly sales drive",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	req, err := reqs.GetByID(t.Context(), requester.TenantID(), out.RequestID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if req.DurationDays() != permissionrequest.DefaultDurationDays {
		t.Errorf("DurationDays = %d, want default %d",
			req.DurationDays(), permissionrequest.DefaultDurationDays)
	}
}

func TestRequestPermissionElevation_LoadRequester_NonNotFoundError_Wrapped(t *testing.T) {
	t.Parallel()

	reqs := newFakePermissionRequestRepo()
	mems := newB2MemRepo()
	mems.errOnGetByID = errB2Permreq
	h := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn(),
		func() permissionrequest.ID { return permissionrequest.ID(ids.NewV7().String()) })

	_, err := h.Handle(t.Context(), command.RequestPermissionElevationCommand{
		TenantID:              tenant.ID(ids.NewV7().String()),
		RequesterMembershipID: membership.ID(ids.NewV7().String()),
		Permission:            permission.FromConstant(permission.IdentityPermissions.Users.Create),
		Reason:                "need to onboard 5 users for monthly sales drive",
	})
	if !errors.Is(err, errB2Permreq) {
		t.Fatalf("err = %v, want wrapped errB2Permreq", err)
	}
	if errors.Is(err, command.ErrUserNotFound) {
		t.Errorf("non-NotFound infra error MUST NOT collapse to ErrUserNotFound")
	}
}

func TestRequestPermissionElevation_AggregateInvariantPropagated(t *testing.T) {
	// permissionrequest.New rejects reason < MinReasonLength. The handler
	// MUST propagate that error unchanged — no translation, no wrap.
	t.Parallel()

	reqs := newFakePermissionRequestRepo()
	mems := newFakeMembershipRepoForPermReq()
	requester := freshMembershipForPermReq(t)
	_ = mems.Add(t.Context(), requester) // arch-test:ignore-err - test fixture setup

	h := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn(),
		func() permissionrequest.ID { return permissionrequest.ID(ids.NewV7().String()) })
	_, err := h.Handle(t.Context(), command.RequestPermissionElevationCommand{
		TenantID:              requester.TenantID(),
		RequesterMembershipID: requester.ID(),
		Permission:            permission.FromConstant(permission.IdentityPermissions.Users.Create),
		Reason:                "short", // < MinReasonLength=10
	})
	if !errors.Is(err, permissionrequest.ErrInvalidRequest) {
		t.Fatalf("err = %v, want permissionrequest.ErrInvalidRequest", err)
	}
}

func TestRequestPermissionElevation_AddPersist_NonPendingExistsError_Wrapped(t *testing.T) {
	t.Parallel()

	reqs := newB2PermReqRepo()
	mems := newFakeMembershipRepoForPermReq()
	requester := freshMembershipForPermReq(t)
	_ = mems.Add(t.Context(), requester) // arch-test:ignore-err - test fixture setup
	reqs.errOnAdd = errB2Permreq

	h := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn(),
		func() permissionrequest.ID { return permissionrequest.ID(ids.NewV7().String()) })
	_, err := h.Handle(t.Context(), command.RequestPermissionElevationCommand{
		TenantID:              requester.TenantID(),
		RequesterMembershipID: requester.ID(),
		Permission:            permission.FromConstant(permission.IdentityPermissions.Users.Create),
		Reason:                "need to onboard 5 users for monthly sales drive",
	})
	if !errors.Is(err, errB2Permreq) {
		t.Fatalf("err = %v, want wrapped errB2Permreq", err)
	}
	if errors.Is(err, command.ErrPermissionRequestPendingExists) {
		t.Errorf("non-PendingExists infra error MUST NOT collapse to ErrPermissionRequestPendingExists")
	}
}

// ----- ApprovePermissionRequest — additional branches ---------------------

func TestApprovePermissionRequest_InputValidation(t *testing.T) {
	t.Parallel()
	tid := tenant.ID(ids.NewV7().String())
	rid := permissionrequest.ID(ids.NewV7().String())
	mid := membership.ID(ids.NewV7().String())

	cases := []struct {
		name string
		cmd  command.ApprovePermissionRequestCommand
	}{
		{
			name: "tenant id required",
			cmd: command.ApprovePermissionRequestCommand{
				TenantID:             tenant.ID(""),
				RequestID:            rid,
				ApproverMembershipID: mid,
			},
		},
		{
			name: "request id required",
			cmd: command.ApprovePermissionRequestCommand{
				TenantID:             tid,
				RequestID:            permissionrequest.ID(""),
				ApproverMembershipID: mid,
			},
		},
		{
			name: "approver membership id required",
			cmd: command.ApprovePermissionRequestCommand{
				TenantID:             tid,
				RequestID:            rid,
				ApproverMembershipID: membership.ID(""),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := command.NewApprovePermissionRequestHandler(
				newFakePermissionRequestRepo(),
				newFakeMembershipRepoForPermReq(),
				fixedTimeFn(),
				ids.NewV7,
			)
			if err := h.Handle(t.Context(), tc.cmd); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestApprovePermissionRequest_RequestNotFound(t *testing.T) {
	t.Parallel()

	reqs := newFakePermissionRequestRepo()
	mems := newFakeMembershipRepoForPermReq()
	h := command.NewApprovePermissionRequestHandler(reqs, mems, fixedTimeFn(), ids.NewV7)

	err := h.Handle(t.Context(), command.ApprovePermissionRequestCommand{
		TenantID:             tenant.ID(ids.NewV7().String()),
		RequestID:            permissionrequest.ID(ids.NewV7().String()),
		ApproverMembershipID: membership.ID(ids.NewV7().String()),
	})
	if !errors.Is(err, command.ErrPermissionRequestNotFound) {
		t.Fatalf("err = %v, want ErrPermissionRequestNotFound", err)
	}
}

func TestApprovePermissionRequest_NonPendingAtLoad(t *testing.T) {
	// Setup: submit + cancel (terminal). Re-approve must surface
	// ErrPermissionRequestNotPending at the load-time state check.
	t.Parallel()

	reqs := newFakePermissionRequestRepo()
	mems := newFakeMembershipRepoForPermReq()
	requester := freshMembershipForPermReq(t)
	_ = mems.Add(t.Context(), requester) // arch-test:ignore-err - test fixture setup

	submitH := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn(),
		func() permissionrequest.ID { return permissionrequest.ID(ids.NewV7().String()) })
	out, _ := submitH.Handle(t.Context(), command.RequestPermissionElevationCommand{
		TenantID:              requester.TenantID(),
		RequesterMembershipID: requester.ID(),
		Permission:            permission.FromConstant(permission.IdentityPermissions.Users.Create),
		Reason:                "need to onboard 5 users for monthly sales drive",
	})
	// Cancel terminates the request.
	cancelH := command.NewCancelPermissionRequestHandler(reqs, fixedTimeFn())
	if err := cancelH.Handle(t.Context(), command.CancelPermissionRequestCommand{
		TenantID:              requester.TenantID(),
		RequestID:             out.RequestID,
		RequesterMembershipID: requester.ID(),
	}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	approveH := command.NewApprovePermissionRequestHandler(reqs, mems, fixedTimeFn(), ids.NewV7)
	err := approveH.Handle(t.Context(), command.ApprovePermissionRequestCommand{
		TenantID:             requester.TenantID(),
		RequestID:             out.RequestID,
		ApproverMembershipID: membership.ID(ids.NewV7().String()),
		IsPlatformOperator:   true,
	})
	if !errors.Is(err, command.ErrPermissionRequestNotPending) {
		t.Fatalf("err = %v, want ErrPermissionRequestNotPending", err)
	}
}

func TestApprovePermissionRequest_RequesterDeletedBetweenSubmitAndApprove(t *testing.T) {
	// Submit, then drop the requester from the fake repo before approve.
	// The membership GetByID returns ErrNotFound — handler collapses to
	// ErrPermissionRequestNotFound (enumeration-safety).
	t.Parallel()

	reqs := newFakePermissionRequestRepo()
	mems := newFakeMembershipRepoForPermReq()
	requester := freshMembershipForPermReq(t)
	manager := freshMembershipForPermReq(t)
	_ = requester.AssignManager(manager.ID(), testNow) // arch-test:ignore-err - test fixture setup
	_ = requester.PullEvents()
	_ = mems.Add(t.Context(), requester) // arch-test:ignore-err - test fixture setup
	_ = mems.Add(t.Context(), manager)   // arch-test:ignore-err - test fixture setup

	submitH := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn(),
		func() permissionrequest.ID { return permissionrequest.ID(ids.NewV7().String()) })
	out, _ := submitH.Handle(t.Context(), command.RequestPermissionElevationCommand{
		TenantID:              requester.TenantID(),
		RequesterMembershipID: requester.ID(),
		Permission:            permission.FromConstant(permission.IdentityPermissions.Users.Create),
		Reason:                "need to onboard 5 users for monthly sales drive",
	})

	// Inject ErrNotFound on the next GetByID — simulates requester deletion
	// race-window between submit + approve.
	wrappedMems := &b2MemRepo{FakeRepository: mems}
	wrappedMems.errOnGetByID = membership.ErrNotFound

	approveH := command.NewApprovePermissionRequestHandler(reqs, wrappedMems, fixedTimeFn(), ids.NewV7)
	err := approveH.Handle(t.Context(), command.ApprovePermissionRequestCommand{
		TenantID:             requester.TenantID(),
		RequestID:            out.RequestID,
		ApproverMembershipID: manager.ID(),
	})
	if !errors.Is(err, command.ErrPermissionRequestNotFound) {
		t.Fatalf("err = %v, want ErrPermissionRequestNotFound", err)
	}
}

func TestApprovePermissionRequest_NonManagerAndNonPlatform_Forbidden(t *testing.T) {
	// Requester has a manager; approver is some OTHER non-manager,
	// non-platform membership.
	t.Parallel()

	reqs := newFakePermissionRequestRepo()
	mems := newFakeMembershipRepoForPermReq()
	requester := freshMembershipForPermReq(t)
	manager := freshMembershipForPermReq(t)
	_ = requester.AssignManager(manager.ID(), testNow) // arch-test:ignore-err - test fixture setup
	_ = requester.PullEvents()
	_ = mems.Add(t.Context(), requester) // arch-test:ignore-err - test fixture setup
	_ = mems.Add(t.Context(), manager)   // arch-test:ignore-err - test fixture setup

	submitH := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn(),
		func() permissionrequest.ID { return permissionrequest.ID(ids.NewV7().String()) })
	out, _ := submitH.Handle(t.Context(), command.RequestPermissionElevationCommand{
		TenantID:              requester.TenantID(),
		RequesterMembershipID: requester.ID(),
		Permission:            permission.FromConstant(permission.IdentityPermissions.Users.Create),
		Reason:                "need to onboard 5 users for monthly sales drive",
	})

	approveH := command.NewApprovePermissionRequestHandler(reqs, mems, fixedTimeFn(), ids.NewV7)
	other := membership.ID(ids.NewV7().String())
	err := approveH.Handle(t.Context(), command.ApprovePermissionRequestCommand{
		TenantID:             requester.TenantID(),
		RequestID:            out.RequestID,
		ApproverMembershipID: other,
		IsPlatformOperator:   false,
	})
	if !errors.Is(err, command.ErrPermissionRequestForbidden) {
		t.Fatalf("err = %v, want ErrPermissionRequestForbidden", err)
	}
}

func TestApprovePermissionRequest_Step2GrantFailure_RequestRemainsApproved(t *testing.T) {
	// Step-1 (Request.Approve UpdateByID on the requests repo) succeeds.
	// Step-2 (Membership.GrantPermission UpdateByID on the memberships
	// repo) fails — handler returns wrapped error AND leaves the Request
	// in Approved state (compensating-state behaviour per ADR 0055).
	t.Parallel()

	reqs := newFakePermissionRequestRepo()
	mems := newB2MemRepo()
	requester := freshMembershipForPermReq(t)
	manager := freshMembershipForPermReq(t)
	_ = requester.AssignManager(manager.ID(), testNow) // arch-test:ignore-err - test fixture setup
	_ = requester.PullEvents()
	_ = mems.Add(t.Context(), requester) // arch-test:ignore-err - test fixture setup
	_ = mems.Add(t.Context(), manager)   // arch-test:ignore-err - test fixture setup

	submitH := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn(),
		func() permissionrequest.ID { return permissionrequest.ID(ids.NewV7().String()) })
	out, _ := submitH.Handle(t.Context(), command.RequestPermissionElevationCommand{
		TenantID:              requester.TenantID(),
		RequesterMembershipID: requester.ID(),
		Permission:            permission.FromConstant(permission.IdentityPermissions.Users.Create),
		Reason:                "need to onboard 5 users for monthly sales drive",
	})

	// Approve calls UpdateByID twice: first on requests (Step 1), then on
	// memberships (Step 2). errOnUpdateByID on mems fires on the Step-2
	// call only — Step 1 has already committed.
	mems.errOnUpdateByID = errB2Permreq

	approveH := command.NewApprovePermissionRequestHandler(reqs, mems, fixedTimeFn(), ids.NewV7)
	err := approveH.Handle(t.Context(), command.ApprovePermissionRequestCommand{
		TenantID:             requester.TenantID(),
		RequestID:            out.RequestID,
		ApproverMembershipID: manager.ID(),
		DecisionReason:       "approved despite infra hiccup",
	})
	if !errors.Is(err, errB2Permreq) {
		t.Fatalf("err = %v, want wrapped errB2Permreq", err)
	}
	// Compensating-state assertion: Request is Approved even though the
	// membership grant failed.
	req, gerr := reqs.GetByID(t.Context(), requester.TenantID(), out.RequestID)
	if gerr != nil {
		t.Fatalf("GetByID after partial failure: %v", gerr)
	}
	if req.State() != permissionrequest.StateApproved {
		t.Errorf("State after Step-2 failure = %v, want approved (audit canon)", req.State())
	}
	// And the membership overlay is empty — no grant landed.
	if got := len(requester.GrantedPermissions()); got != 0 {
		t.Errorf("granted permissions = %d, want 0 (Step-2 failure → no grant)", got)
	}
}

func TestApprovePermissionRequest_UpdateByID_NonPendingTranslation(t *testing.T) {
	// Inject ErrNotPending from the requests UpdateByID — simulates the
	// race where another approver finalised the same row between this
	// handler's load-time check and its update closure.
	t.Parallel()

	reqs := newB2PermReqRepo()
	mems := newFakeMembershipRepoForPermReq()
	requester := freshMembershipForPermReq(t)
	manager := freshMembershipForPermReq(t)
	_ = requester.AssignManager(manager.ID(), testNow) // arch-test:ignore-err - test fixture setup
	_ = requester.PullEvents()
	_ = mems.Add(t.Context(), requester) // arch-test:ignore-err - test fixture setup
	_ = mems.Add(t.Context(), manager)   // arch-test:ignore-err - test fixture setup

	submitH := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn(),
		func() permissionrequest.ID { return permissionrequest.ID(ids.NewV7().String()) })
	out, _ := submitH.Handle(t.Context(), command.RequestPermissionElevationCommand{
		TenantID:              requester.TenantID(),
		RequesterMembershipID: requester.ID(),
		Permission:            permission.FromConstant(permission.IdentityPermissions.Users.Create),
		Reason:                "need to onboard 5 users for monthly sales drive",
	})
	reqs.errOnUpdateByID = permissionrequest.ErrNotPending

	approveH := command.NewApprovePermissionRequestHandler(reqs, mems, fixedTimeFn(), ids.NewV7)
	err := approveH.Handle(t.Context(), command.ApprovePermissionRequestCommand{
		TenantID:             requester.TenantID(),
		RequestID:            out.RequestID,
		ApproverMembershipID: manager.ID(),
	})
	if !errors.Is(err, command.ErrPermissionRequestNotPending) {
		t.Fatalf("err = %v, want ErrPermissionRequestNotPending", err)
	}
}

// ----- DenyPermissionRequest — additional branches ------------------------

func TestDenyPermissionRequest_InputValidation(t *testing.T) {
	t.Parallel()
	tid := tenant.ID(ids.NewV7().String())
	rid := permissionrequest.ID(ids.NewV7().String())
	mid := membership.ID(ids.NewV7().String())

	cases := []struct {
		name string
		cmd  command.DenyPermissionRequestCommand
	}{
		{
			name: "tenant id required",
			cmd: command.DenyPermissionRequestCommand{
				TenantID:             tenant.ID(""),
				RequestID:            rid,
				ApproverMembershipID: mid,
				DecisionReason:       "scope too broad",
			},
		},
		{
			name: "request id required",
			cmd: command.DenyPermissionRequestCommand{
				TenantID:             tid,
				RequestID:            permissionrequest.ID(""),
				ApproverMembershipID: mid,
				DecisionReason:       "scope too broad",
			},
		},
		{
			name: "approver membership id required",
			cmd: command.DenyPermissionRequestCommand{
				TenantID:             tid,
				RequestID:            rid,
				ApproverMembershipID: membership.ID(""),
				DecisionReason:       "scope too broad",
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := command.NewDenyPermissionRequestHandler(
				newFakePermissionRequestRepo(),
				newFakeMembershipRepoForPermReq(),
				fixedTimeFn(),
			)
			if err := h.Handle(t.Context(), tc.cmd); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestDenyPermissionRequest_NonPendingAtLoad(t *testing.T) {
	t.Parallel()

	reqs := newFakePermissionRequestRepo()
	mems := newFakeMembershipRepoForPermReq()
	requester := freshMembershipForPermReq(t)
	_ = mems.Add(t.Context(), requester) // arch-test:ignore-err - test fixture setup

	submitH := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn(),
		func() permissionrequest.ID { return permissionrequest.ID(ids.NewV7().String()) })
	out, _ := submitH.Handle(t.Context(), command.RequestPermissionElevationCommand{
		TenantID:              requester.TenantID(),
		RequesterMembershipID: requester.ID(),
		Permission:            permission.FromConstant(permission.IdentityPermissions.Users.Create),
		Reason:                "need to onboard 5 users for monthly sales drive",
	})
	cancelH := command.NewCancelPermissionRequestHandler(reqs, fixedTimeFn())
	if err := cancelH.Handle(t.Context(), command.CancelPermissionRequestCommand{
		TenantID:              requester.TenantID(),
		RequestID:             out.RequestID,
		RequesterMembershipID: requester.ID(),
	}); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	denyH := command.NewDenyPermissionRequestHandler(reqs, mems, fixedTimeFn())
	err := denyH.Handle(t.Context(), command.DenyPermissionRequestCommand{
		TenantID:             requester.TenantID(),
		RequestID:            out.RequestID,
		ApproverMembershipID: membership.ID(ids.NewV7().String()),
		IsPlatformOperator:   true,
		DecisionReason:       "scope too broad; rethink and re-submit",
	})
	if !errors.Is(err, command.ErrPermissionRequestNotPending) {
		t.Fatalf("err = %v, want ErrPermissionRequestNotPending", err)
	}
}

func TestDenyPermissionRequest_SelfDeny_BlockedAsSelfApproval(t *testing.T) {
	t.Parallel()

	reqs := newFakePermissionRequestRepo()
	mems := newFakeMembershipRepoForPermReq()
	requester := freshMembershipForPermReq(t)
	_ = mems.Add(t.Context(), requester) // arch-test:ignore-err - test fixture setup

	submitH := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn(),
		func() permissionrequest.ID { return permissionrequest.ID(ids.NewV7().String()) })
	out, _ := submitH.Handle(t.Context(), command.RequestPermissionElevationCommand{
		TenantID:              requester.TenantID(),
		RequesterMembershipID: requester.ID(),
		Permission:            permission.FromConstant(permission.IdentityPermissions.Users.Create),
		Reason:                "need to onboard 5 users for monthly sales drive",
	})

	denyH := command.NewDenyPermissionRequestHandler(reqs, mems, fixedTimeFn())
	err := denyH.Handle(t.Context(), command.DenyPermissionRequestCommand{
		TenantID:             requester.TenantID(),
		RequestID:            out.RequestID,
		ApproverMembershipID: requester.ID(), // self-deny
		DecisionReason:       "changed my mind",
	})
	if !errors.Is(err, command.ErrPermissionRequestSelfApproval) {
		t.Fatalf("err = %v, want ErrPermissionRequestSelfApproval", err)
	}
}

func TestDenyPermissionRequest_MissingManagerAndNonPlatform_Forbidden(t *testing.T) {
	t.Parallel()

	reqs := newFakePermissionRequestRepo()
	mems := newFakeMembershipRepoForPermReq()
	requester := freshMembershipForPermReq(t) // NO manager
	_ = mems.Add(t.Context(), requester)      // arch-test:ignore-err - test fixture setup

	submitH := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn(),
		func() permissionrequest.ID { return permissionrequest.ID(ids.NewV7().String()) })
	out, _ := submitH.Handle(t.Context(), command.RequestPermissionElevationCommand{
		TenantID:              requester.TenantID(),
		RequesterMembershipID: requester.ID(),
		Permission:            permission.FromConstant(permission.IdentityPermissions.Users.Create),
		Reason:                "need to onboard 5 users for monthly sales drive",
	})

	denyH := command.NewDenyPermissionRequestHandler(reqs, mems, fixedTimeFn())
	err := denyH.Handle(t.Context(), command.DenyPermissionRequestCommand{
		TenantID:             requester.TenantID(),
		RequestID:            out.RequestID,
		ApproverMembershipID: membership.ID(ids.NewV7().String()), // not the manager
		IsPlatformOperator:   false,
		DecisionReason:       "scope too broad; rethink and re-submit",
	})
	if !errors.Is(err, command.ErrPermissionRequestForbidden) {
		t.Fatalf("err = %v, want ErrPermissionRequestForbidden", err)
	}
}

// ----- CancelPermissionRequest — additional branches ----------------------

func TestCancelPermissionRequest_InputValidation(t *testing.T) {
	t.Parallel()
	tid := tenant.ID(ids.NewV7().String())
	rid := permissionrequest.ID(ids.NewV7().String())
	mid := membership.ID(ids.NewV7().String())

	cases := []struct {
		name string
		cmd  command.CancelPermissionRequestCommand
	}{
		{
			name: "tenant id required",
			cmd: command.CancelPermissionRequestCommand{
				TenantID:              tenant.ID(""),
				RequestID:             rid,
				RequesterMembershipID: mid,
			},
		},
		{
			name: "request id required",
			cmd: command.CancelPermissionRequestCommand{
				TenantID:              tid,
				RequestID:             permissionrequest.ID(""),
				RequesterMembershipID: mid,
			},
		},
		{
			name: "requester membership id required",
			cmd: command.CancelPermissionRequestCommand{
				TenantID:              tid,
				RequestID:             rid,
				RequesterMembershipID: membership.ID(""),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := command.NewCancelPermissionRequestHandler(
				newFakePermissionRequestRepo(),
				fixedTimeFn(),
			)
			if err := h.Handle(t.Context(), tc.cmd); err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

func TestCancelPermissionRequest_NonPending(t *testing.T) {
	// Submit + already-cancelled. Re-cancel by the requester (matching
	// id) MUST surface NotPending (not NotFound).
	t.Parallel()

	reqs := newFakePermissionRequestRepo()
	mems := newFakeMembershipRepoForPermReq()
	requester := freshMembershipForPermReq(t)
	_ = mems.Add(t.Context(), requester) // arch-test:ignore-err - test fixture setup

	submitH := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn(),
		func() permissionrequest.ID { return permissionrequest.ID(ids.NewV7().String()) })
	out, _ := submitH.Handle(t.Context(), command.RequestPermissionElevationCommand{
		TenantID:              requester.TenantID(),
		RequesterMembershipID: requester.ID(),
		Permission:            permission.FromConstant(permission.IdentityPermissions.Users.Create),
		Reason:                "need to onboard 5 users for monthly sales drive",
	})

	cancelH := command.NewCancelPermissionRequestHandler(reqs, fixedTimeFn())
	if err := cancelH.Handle(t.Context(), command.CancelPermissionRequestCommand{
		TenantID:              requester.TenantID(),
		RequestID:             out.RequestID,
		RequesterMembershipID: requester.ID(),
	}); err != nil {
		t.Fatalf("first Cancel: %v", err)
	}

	err := cancelH.Handle(t.Context(), command.CancelPermissionRequestCommand{
		TenantID:              requester.TenantID(),
		RequestID:             out.RequestID,
		RequesterMembershipID: requester.ID(),
	})
	if !errors.Is(err, command.ErrPermissionRequestNotPending) {
		t.Fatalf("err = %v, want ErrPermissionRequestNotPending", err)
	}
}

// ----- Cross-cutting reachability checks ----------------------------------

// _ = b2KeepUnusedRef references symbols imported above for branches
// we don't exercise inline (uuid + pagination + person) — keeps the
// import block honest. Blank-identifier assignment avoids the
// unused-var lint check.
//
//nolint:gochecknoglobals // sentinel reachability marker for unused-import
var (
	_ = uuid.Nil
	_ pagination.Cursor
	_ *person.Person
)

