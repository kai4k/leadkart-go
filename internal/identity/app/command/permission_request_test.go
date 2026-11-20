package command_test

import (
	"context"
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

// newFakePermissionRequestRepo aliases the co-located permissionrequesttest fake.
func newFakePermissionRequestRepo() *permissionrequesttest.FakeRepository {
	return permissionrequesttest.NewFakeRepository()
}

// newFakeMembershipRepoForPermReq aliases the co-located membershiptest fake.
func newFakeMembershipRepoForPermReq() *membershiptest.FakeRepository {
	return membershiptest.NewFakeRepository()
}

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
		Actor:                 submitterActor(t),
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
		Actor:                 submitterActor(t),
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
		Actor:                 submitterActor(t),
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

// ----- DenyPermissionRequest ----------------------------------------------

// ----- CancelPermissionRequest --------------------------------------------

func TestCancelPermissionRequest_OnlyRequesterCanCancel(t *testing.T) {
	t.Parallel()

	reqs := newFakePermissionRequestRepo()
	mems := newFakeMembershipRepoForPermReq()
	requester := freshMembershipForPermReq(t)
	_ = mems.Add(t.Context(), requester) // arch-test:ignore-err - test fixture setup

	submitH := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn(), func() permissionrequest.ID { return permissionrequest.ID(ids.NewV7().String()) })
	out, _ := submitH.Handle(t.Context(), command.RequestPermissionElevationCommand{
		Actor:                 submitterActor(t),
		TenantID:              requester.TenantID(),
		RequesterMembershipID: requester.ID(),
		Permission:            permission.FromConstant(permission.IdentityPermissions.Users.Create),
		Reason:                "need to onboard 5 users for monthly sales drive",
	})

	cancelH := command.NewCancelPermissionRequestHandler(reqs, fixedTimeFn())
	// Different caller → 404 (enumeration-safe).
	err := cancelH.Handle(t.Context(), command.CancelPermissionRequestCommand{
		TenantID:              requester.TenantID(),
		RequestID:             out.RequestID,
		RequesterMembershipID: membership.ID(ids.NewV7().String()),
	})
	if !errors.Is(err, command.ErrPermissionRequestNotFound) {
		t.Fatalf("cross-caller err = %v, want ErrPermissionRequestNotFound", err)
	}

	// The true requester can cancel.
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

// Additional branch coverage (ADR 0062 §6): input validation, non-NotFound
// error wrapping, aggregate-invariant + non-Pending translations, authz
// Forbidden, the compensating-state path when the Step-2 grant fails, cancel
// enumeration safety, and the default-duration fallback. b2* wrappers add the
// error injection the shared fakes lack.

// b2PermReqRepo injects per-call errors on GetByID / Add / UpdateByID.
type b2PermReqRepo struct {
	*permissionrequesttest.FakeRepository

	// errOnGetByID fires once on the next GetByID, then resets to nil.
	errOnGetByID error
	// errOnAdd fires once on the next Add, then resets to nil.
	errOnAdd error
	// errOnUpdateByID fires once on the next UpdateByID, then resets to nil.
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

// b2MemRepo injects per-call errors on GetByID + UpdateByID.
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

// errB2Permreq is the synthetic error for the non-NotFound wrapped branches.
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
				Actor:                 submitterActor(t),
				TenantID:              tenant.ID(""),
				RequesterMembershipID: mid,
				Permission:            perm,
				Reason:                "need to onboard 5 users for monthly sales drive",
			},
		},
		{
			name: "requester membership id required",
			cmd: command.RequestPermissionElevationCommand{
				Actor:                 submitterActor(t),
				TenantID:              tid,
				RequesterMembershipID: membership.ID(""),
				Permission:            perm,
				Reason:                "need to onboard 5 users for monthly sales drive",
			},
		},
		{
			name: "permission required",
			cmd: command.RequestPermissionElevationCommand{
				Actor:                 submitterActor(t),
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
		Actor:                 submitterActor(t),
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
		Actor:                 submitterActor(t),
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
	// A too-short reason trips permissionrequest.New; the handler propagates
	// the error unchanged.
	t.Parallel()

	reqs := newFakePermissionRequestRepo()
	mems := newFakeMembershipRepoForPermReq()
	requester := freshMembershipForPermReq(t)
	_ = mems.Add(t.Context(), requester) // arch-test:ignore-err - test fixture setup

	h := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn(),
		func() permissionrequest.ID { return permissionrequest.ID(ids.NewV7().String()) })
	_, err := h.Handle(t.Context(), command.RequestPermissionElevationCommand{
		Actor:                 submitterActor(t),
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
		Actor:                 submitterActor(t),
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

// ----- DenyPermissionRequest — additional branches ------------------------

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
	// Re-cancel by the requester surfaces NotPending (not NotFound).
	t.Parallel()

	reqs := newFakePermissionRequestRepo()
	mems := newFakeMembershipRepoForPermReq()
	requester := freshMembershipForPermReq(t)
	_ = mems.Add(t.Context(), requester) // arch-test:ignore-err - test fixture setup

	submitH := command.NewRequestPermissionElevationHandler(reqs, mems, fixedTimeFn(),
		func() permissionrequest.ID { return permissionrequest.ID(ids.NewV7().String()) })
	out, _ := submitH.Handle(t.Context(), command.RequestPermissionElevationCommand{
		Actor:                 submitterActor(t),
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
