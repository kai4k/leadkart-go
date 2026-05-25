package permissionrequest_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permissionrequest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// fixedClock returns a stable wall-clock for deterministic tests.
func fixedClock() time.Time {
	return time.Date(2026, 5, 23, 12, 0, 0, 0, time.UTC)
}

// validNew constructs a baseline valid request — every test starts from this.
func validNew(t *testing.T) *permissionrequest.Request {
	t.Helper()
	r, err := permissionrequest.New(
		permissionrequest.ID(ids.NewV7().String()),
		tenant.ID(ids.NewV7().String()),
		membership.ID(ids.NewV7().String()),
		permission.FromConstant(permission.IdentityPermissions.Users.Create),
		7,
		"need to onboard 5 users for monthly sales drive",
		fixedClock(),
	)
	if err != nil {
		t.Fatalf("permissionrequest.New baseline: %v", err)
	}
	return r
}

func TestNew_RejectsShortReason(t *testing.T) {
	t.Parallel()
	_, err := permissionrequest.New(
		permissionrequest.ID(ids.NewV7().String()),
		tenant.ID(ids.NewV7().String()),
		membership.ID(ids.NewV7().String()),
		permission.FromConstant(permission.IdentityPermissions.Users.Create),
		7,
		"short",
		fixedClock(),
	)
	if !errors.Is(err, permissionrequest.ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestNew_RejectsLongReason(t *testing.T) {
	t.Parallel()
	_, err := permissionrequest.New(
		permissionrequest.ID(ids.NewV7().String()),
		tenant.ID(ids.NewV7().String()),
		membership.ID(ids.NewV7().String()),
		permission.FromConstant(permission.IdentityPermissions.Users.Create),
		7,
		strings.Repeat("x", permissionrequest.MaxReasonLength+1),
		fixedClock(),
	)
	if !errors.Is(err, permissionrequest.ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestNew_RejectsZeroDuration(t *testing.T) {
	t.Parallel()
	_, err := permissionrequest.New(
		permissionrequest.ID(ids.NewV7().String()),
		tenant.ID(ids.NewV7().String()),
		membership.ID(ids.NewV7().String()),
		permission.FromConstant(permission.IdentityPermissions.Users.Create),
		0,
		"need elevated access for the rollout this week",
		fixedClock(),
	)
	if !errors.Is(err, permissionrequest.ErrInvalidDuration) {
		t.Fatalf("err = %v, want ErrInvalidDuration", err)
	}
}

func TestNew_RejectsExcessiveDuration(t *testing.T) {
	t.Parallel()
	_, err := permissionrequest.New(
		permissionrequest.ID(ids.NewV7().String()),
		tenant.ID(ids.NewV7().String()),
		membership.ID(ids.NewV7().String()),
		permission.FromConstant(permission.IdentityPermissions.Users.Create),
		permissionrequest.MaxDurationDays+1,
		"need elevated access for the rollout this week",
		fixedClock(),
	)
	if !errors.Is(err, permissionrequest.ErrInvalidDuration) {
		t.Fatalf("err = %v, want ErrInvalidDuration", err)
	}
}

func TestNew_RecordsRequestedEvent(t *testing.T) {
	t.Parallel()
	r := validNew(t)
	events := r.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev, ok := events[0].(permissionrequest.RequestedEvent)
	if !ok {
		t.Fatalf("event type = %T, want RequestedEvent", events[0])
	}
	if ev.RequestID != r.ID() {
		t.Errorf("event RequestID = %v want %v", ev.RequestID, r.ID())
	}
	if ev.Permission != permission.IdentityPermissions.Users.Create {
		t.Errorf("event Permission = %q want %q", ev.Permission, permission.IdentityPermissions.Users.Create)
	}
}

func TestApprove_TransitionsToApproved(t *testing.T) {
	t.Parallel()
	r := validNew(t)
	_ = r.PullEvents()
	approver := membership.ID(ids.NewV7().String())
	expiresAt := fixedClock().Add(7 * 24 * time.Hour)

	if err := r.Approve(approver, "looks fine", uuid.New(), expiresAt, fixedClock()); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if r.State() != permissionrequest.StateApproved {
		t.Errorf("State = %s, want approved", r.State())
	}
	if !r.IsTerminal() {
		t.Error("IsTerminal should be true after Approve")
	}
	if r.ApproverMembershipID() != approver {
		t.Errorf("ApproverMembershipID = %v, want %v", r.ApproverMembershipID(), approver)
	}
	if !r.ExpiresAt().Equal(expiresAt.UTC()) {
		t.Errorf("ExpiresAt = %v, want %v", r.ExpiresAt(), expiresAt.UTC())
	}
}

func TestApprove_RejectsSelfApproval(t *testing.T) {
	t.Parallel()
	r := validNew(t)
	_ = r.PullEvents()
	// approverID == requesterID
	err := r.Approve(r.RequesterMembershipID(), "", uuid.New(), fixedClock(), fixedClock())
	if !errors.Is(err, permissionrequest.ErrSelfApproval) {
		t.Fatalf("err = %v, want ErrSelfApproval", err)
	}
}

func TestApprove_RejectsNonPending(t *testing.T) {
	t.Parallel()
	r := validNew(t)
	_ = r.PullEvents()
	approver := membership.ID(ids.NewV7().String())
	// First approval — succeeds.
	if err := r.Approve(approver, "", uuid.New(), fixedClock(), fixedClock()); err != nil {
		t.Fatalf("first Approve: %v", err)
	}
	// Second approval on already-Approved row.
	err := r.Approve(approver, "", uuid.New(), fixedClock(), fixedClock())
	if !errors.Is(err, permissionrequest.ErrNotPending) {
		t.Fatalf("err = %v, want ErrNotPending", err)
	}
}

func TestApprove_RecordsApprovedEvent(t *testing.T) {
	t.Parallel()
	r := validNew(t)
	_ = r.PullEvents()
	approver := membership.ID(ids.NewV7().String())
	expiresAt := fixedClock().Add(7 * 24 * time.Hour)
	_ = r.Approve(approver, "ok", uuid.New(), expiresAt, fixedClock()) // arch-test:ignore-err - test fixture setup
	events := r.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev, ok := events[0].(permissionrequest.ApprovedEvent)
	if !ok {
		t.Fatalf("event type = %T, want ApprovedEvent", events[0])
	}
	if ev.ApproverMembershipID != approver {
		t.Errorf("event ApproverMembershipID = %v, want %v", ev.ApproverMembershipID, approver)
	}
}

func TestDeny_RejectsEmptyReason(t *testing.T) {
	t.Parallel()
	r := validNew(t)
	_ = r.PullEvents()
	approver := membership.ID(ids.NewV7().String())
	err := r.Deny(approver, "", fixedClock())
	if !errors.Is(err, permissionrequest.ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestDeny_TransitionsToDenied(t *testing.T) {
	t.Parallel()
	r := validNew(t)
	_ = r.PullEvents()
	approver := membership.ID(ids.NewV7().String())
	if err := r.Deny(approver, "scope too broad", fixedClock()); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	if r.State() != permissionrequest.StateDenied {
		t.Errorf("State = %s, want denied", r.State())
	}
	if !r.IsTerminal() {
		t.Error("IsTerminal should be true after Deny")
	}
	events := r.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	if _, ok := events[0].(permissionrequest.DeniedEvent); !ok {
		t.Fatalf("event type = %T, want DeniedEvent", events[0])
	}
}

func TestCancel_OnlyAllowedOnPending(t *testing.T) {
	t.Parallel()
	r := validNew(t)
	_ = r.PullEvents()
	if err := r.Cancel(fixedClock()); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if r.State() != permissionrequest.StateCancelled {
		t.Errorf("State = %s, want cancelled", r.State())
	}
	// Second Cancel should fail-loud per ADR 0055.
	err := r.Cancel(fixedClock())
	if !errors.Is(err, permissionrequest.ErrNotPending) {
		t.Fatalf("err = %v, want ErrNotPending", err)
	}
}

func TestIsTerminal_TrueForApprovedDeniedCancelled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		state permissionrequest.State
		want  bool
	}{
		{"pending", permissionrequest.StatePending, false},
		{"approved", permissionrequest.StateApproved, true},
		{"denied", permissionrequest.StateDenied, true},
		{"cancelled", permissionrequest.StateCancelled, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.state.IsTerminal(); got != tc.want {
				t.Errorf("State(%q).IsTerminal() = %v, want %v", tc.state, got, tc.want)
			}
		})
	}
}
