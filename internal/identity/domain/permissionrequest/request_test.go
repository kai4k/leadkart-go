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

// ----- New: invariant boundaries --------------------------------------------

func TestNew_RejectsZeroID(t *testing.T) {
	t.Parallel()
	_, err := permissionrequest.New(
		permissionrequest.ID(""),
		tenant.ID(ids.NewV7().String()),
		membership.ID(ids.NewV7().String()),
		permission.FromConstant(permission.IdentityPermissions.Users.Create),
		7,
		"need elevated access for the rollout this week",
		fixedClock(),
	)
	if !errors.Is(err, permissionrequest.ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestNew_RejectsZeroTenantID(t *testing.T) {
	t.Parallel()
	_, err := permissionrequest.New(
		permissionrequest.ID(ids.NewV7().String()),
		tenant.ID(""),
		membership.ID(ids.NewV7().String()),
		permission.FromConstant(permission.IdentityPermissions.Users.Create),
		7,
		"need elevated access for the rollout this week",
		fixedClock(),
	)
	if !errors.Is(err, permissionrequest.ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestNew_RejectsZeroRequesterMembershipID(t *testing.T) {
	t.Parallel()
	_, err := permissionrequest.New(
		permissionrequest.ID(ids.NewV7().String()),
		tenant.ID(ids.NewV7().String()),
		membership.ID(""),
		permission.FromConstant(permission.IdentityPermissions.Users.Create),
		7,
		"need elevated access for the rollout this week",
		fixedClock(),
	)
	if !errors.Is(err, permissionrequest.ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestNew_RejectsNilPermission(t *testing.T) {
	t.Parallel()
	_, err := permissionrequest.New(
		permissionrequest.ID(ids.NewV7().String()),
		tenant.ID(ids.NewV7().String()),
		membership.ID(ids.NewV7().String()),
		nil,
		7,
		"need elevated access for the rollout this week",
		fixedClock(),
	)
	if !errors.Is(err, permissionrequest.ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestNew_RejectsPermissionNotInCatalogue(t *testing.T) {
	t.Parallel()
	// permission.Create succeeds for charset-valid input even when the
	// name isn't in the closed-set catalogue — gives us a synthetic
	// Permission whose IsKnown returns false.
	rogue, err := permission.Create("rogue.synthetic.permission")
	if err != nil {
		t.Fatalf("synthesise rogue permission: %v", err)
	}
	if permission.IsKnown(rogue.Name()) {
		t.Fatalf("setup assumption violated: %q in catalogue", rogue.Name())
	}
	_, err = permissionrequest.New(
		permissionrequest.ID(ids.NewV7().String()),
		tenant.ID(ids.NewV7().String()),
		membership.ID(ids.NewV7().String()),
		rogue,
		7,
		"need elevated access for the rollout this week",
		fixedClock(),
	)
	if !errors.Is(err, permissionrequest.ErrInvalidPermission) {
		t.Fatalf("err = %v, want ErrInvalidPermission", err)
	}
}

func TestNew_DurationBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		days int
	}{
		{"min boundary", permissionrequest.MinDurationDays},
		{"max boundary", permissionrequest.MaxDurationDays},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, err := permissionrequest.New(
				permissionrequest.ID(ids.NewV7().String()),
				tenant.ID(ids.NewV7().String()),
				membership.ID(ids.NewV7().String()),
				permission.FromConstant(permission.IdentityPermissions.Users.Create),
				tc.days,
				"need elevated access for the rollout this week",
				fixedClock(),
			)
			if err != nil {
				t.Fatalf("New(%d days): %v", tc.days, err)
			}
			if got := r.DurationDays(); got != tc.days {
				t.Errorf("DurationDays = %d, want %d", got, tc.days)
			}
		})
	}
}

// ----- Approve: invariant boundaries ----------------------------------------

func TestApprove_RejectsZeroApproverID(t *testing.T) {
	t.Parallel()
	r := validNew(t)
	_ = r.PullEvents()
	err := r.Approve(membership.ID(""), "", uuid.New(), fixedClock(), fixedClock())
	if !errors.Is(err, permissionrequest.ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestApprove_RejectsLongDecisionReason(t *testing.T) {
	t.Parallel()
	r := validNew(t)
	_ = r.PullEvents()
	err := r.Approve(
		membership.ID(ids.NewV7().String()),
		strings.Repeat("x", permissionrequest.MaxDecisionReasonLength+1),
		uuid.New(),
		fixedClock(),
		fixedClock(),
	)
	if !errors.Is(err, permissionrequest.ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestApprove_HappyPath_ApprovedEventCarriesExpiresAtAndAt(t *testing.T) {
	t.Parallel()
	r := validNew(t)
	_ = r.PullEvents()
	approver := membership.ID(ids.NewV7().String())
	expiresAt := fixedClock().Add(7 * 24 * time.Hour)
	at := fixedClock().Add(time.Minute)
	if err := r.Approve(approver, "looks fine", uuid.New(), expiresAt, at); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	events := r.PullEvents()
	if len(events) != 1 {
		t.Fatalf("events = %d, want 1", len(events))
	}
	ev, ok := events[0].(permissionrequest.ApprovedEvent)
	if !ok {
		t.Fatalf("event type = %T, want ApprovedEvent", events[0])
	}
	if !ev.ExpiresAt.Equal(expiresAt.UTC()) {
		t.Errorf("ev.ExpiresAt = %v, want %v", ev.ExpiresAt, expiresAt.UTC())
	}
	if !ev.At.Equal(at.UTC()) {
		t.Errorf("ev.At = %v, want %v", ev.At, at.UTC())
	}
}

// ----- Deny: invariant boundaries -------------------------------------------

func TestDeny_RejectsZeroApproverID(t *testing.T) {
	t.Parallel()
	r := validNew(t)
	_ = r.PullEvents()
	err := r.Deny(membership.ID(""), "scope too broad", fixedClock())
	if !errors.Is(err, permissionrequest.ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestDeny_RejectsSelfDeny(t *testing.T) {
	t.Parallel()
	r := validNew(t)
	_ = r.PullEvents()
	err := r.Deny(r.RequesterMembershipID(), "withdrawing", fixedClock())
	if !errors.Is(err, permissionrequest.ErrSelfApproval) {
		t.Fatalf("err = %v, want ErrSelfApproval", err)
	}
}

func TestDeny_RejectsLongDecisionReason(t *testing.T) {
	t.Parallel()
	r := validNew(t)
	_ = r.PullEvents()
	err := r.Deny(
		membership.ID(ids.NewV7().String()),
		strings.Repeat("x", permissionrequest.MaxDecisionReasonLength+1),
		fixedClock(),
	)
	if !errors.Is(err, permissionrequest.ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

// ----- State matrix: 6 missing cells ----------------------------------------

// transitionStateMachine drives a fresh request to the given terminal
// state. Centralised so the table-driven matrix test below is concise.
func driveToState(t *testing.T, target permissionrequest.State) *permissionrequest.Request {
	t.Helper()
	r := validNew(t)
	_ = r.PullEvents()
	approver := membership.ID(ids.NewV7().String())
	switch target {
	case permissionrequest.StateApproved:
		if err := r.Approve(approver, "ok", uuid.New(), fixedClock(), fixedClock()); err != nil {
			t.Fatalf("drive Approve: %v", err)
		}
	case permissionrequest.StateDenied:
		if err := r.Deny(approver, "scope too broad", fixedClock()); err != nil {
			t.Fatalf("drive Deny: %v", err)
		}
	case permissionrequest.StateCancelled:
		if err := r.Cancel(fixedClock()); err != nil {
			t.Fatalf("drive Cancel: %v", err)
		}
	}
	_ = r.PullEvents()
	return r
}

func TestStateMatrix_NonPendingTransitionsRejected(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		from permissionrequest.State
		act  func(*testing.T, *permissionrequest.Request) error
	}{
		{
			"Approved → Deny",
			permissionrequest.StateApproved,
			func(t *testing.T, r *permissionrequest.Request) error {
				t.Helper()
				return r.Deny(membership.ID(ids.NewV7().String()), "scope too broad", fixedClock())
			},
		},
		{
			"Approved → Cancel",
			permissionrequest.StateApproved,
			func(_ *testing.T, r *permissionrequest.Request) error {
				return r.Cancel(fixedClock())
			},
		},
		{
			"Denied → Approve",
			permissionrequest.StateDenied,
			func(t *testing.T, r *permissionrequest.Request) error {
				t.Helper()
				return r.Approve(
					membership.ID(ids.NewV7().String()),
					"reversing", uuid.New(), fixedClock(), fixedClock(),
				)
			},
		},
		{
			"Denied → Deny",
			permissionrequest.StateDenied,
			func(t *testing.T, r *permissionrequest.Request) error {
				t.Helper()
				return r.Deny(membership.ID(ids.NewV7().String()), "double-deny", fixedClock())
			},
		},
		{
			"Denied → Cancel",
			permissionrequest.StateDenied,
			func(_ *testing.T, r *permissionrequest.Request) error {
				return r.Cancel(fixedClock())
			},
		},
		{
			"Cancelled → Approve",
			permissionrequest.StateCancelled,
			func(t *testing.T, r *permissionrequest.Request) error {
				t.Helper()
				return r.Approve(
					membership.ID(ids.NewV7().String()),
					"reversing", uuid.New(), fixedClock(), fixedClock(),
				)
			},
		},
		{
			"Cancelled → Deny",
			permissionrequest.StateCancelled,
			func(t *testing.T, r *permissionrequest.Request) error {
				t.Helper()
				return r.Deny(membership.ID(ids.NewV7().String()), "scope too broad", fixedClock())
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := driveToState(t, tc.from)
			err := tc.act(t, r)
			if !errors.Is(err, permissionrequest.ErrNotPending) {
				t.Fatalf("err = %v, want ErrNotPending", err)
			}
		})
	}
}

// ----- Misc round-trip + drain ----------------------------------------------

func TestUnmarshalFromDB_RoundTripsEveryField(t *testing.T) {
	t.Parallel()
	overrideID := uuid.New()
	approver := membership.ID(ids.NewV7().String())
	requester := membership.ID(ids.NewV7().String())
	tID := tenant.ID(ids.NewV7().String())
	rID := permissionrequest.ID(ids.NewV7().String())
	createdAt := fixedClock()
	updatedAt := fixedClock().Add(2 * time.Minute)
	decidedAt := fixedClock().Add(time.Minute)
	expiresAt := fixedClock().Add(7 * 24 * time.Hour)

	snap := permissionrequest.Snapshot{
		ID:                    rID,
		TenantID:              tID,
		RequesterMembershipID: requester,
		Permission:            permission.FromConstant(permission.IdentityPermissions.Users.Create),
		DurationDays:          7,
		Reason:                "audit reason that meets the floor length",
		State:                 permissionrequest.StateApproved,
		ApproverMembershipID:  approver,
		DecidedAt:             decidedAt,
		DecisionReason:        "looks fine",
		GrantedOverrideID:     overrideID,
		ExpiresAt:             expiresAt,
		CreatedAt:             createdAt,
		UpdatedAt:             updatedAt,
	}
	r := permissionrequest.UnmarshalFromDB(snap)
	if r.ID() != rID {
		t.Errorf("ID = %v, want %v", r.ID(), rID)
	}
	if r.TenantID() != tID {
		t.Errorf("TenantID = %v", r.TenantID())
	}
	if r.RequesterMembershipID() != requester {
		t.Errorf("RequesterMembershipID = %v", r.RequesterMembershipID())
	}
	if r.Permission().Name() != permission.IdentityPermissions.Users.Create {
		t.Errorf("Permission = %q", r.Permission().Name())
	}
	if r.DurationDays() != 7 {
		t.Errorf("DurationDays = %d", r.DurationDays())
	}
	if r.Reason() != "audit reason that meets the floor length" {
		t.Errorf("Reason = %q", r.Reason())
	}
	if r.State() != permissionrequest.StateApproved {
		t.Errorf("State = %s", r.State())
	}
	if r.ApproverMembershipID() != approver {
		t.Errorf("ApproverMembershipID = %v", r.ApproverMembershipID())
	}
	if !r.DecidedAt().Equal(decidedAt) {
		t.Errorf("DecidedAt = %v, want %v", r.DecidedAt(), decidedAt)
	}
	if r.DecisionReason() != "looks fine" {
		t.Errorf("DecisionReason = %q", r.DecisionReason())
	}
	if r.GrantedOverrideID() != overrideID {
		t.Errorf("GrantedOverrideID = %v", r.GrantedOverrideID())
	}
	if !r.ExpiresAt().Equal(expiresAt) {
		t.Errorf("ExpiresAt = %v, want %v", r.ExpiresAt(), expiresAt)
	}
	if !r.CreatedAt().Equal(createdAt) {
		t.Errorf("CreatedAt = %v, want %v", r.CreatedAt(), createdAt)
	}
	if !r.UpdatedAt().Equal(updatedAt) {
		t.Errorf("UpdatedAt = %v, want %v", r.UpdatedAt(), updatedAt)
	}
	// Unmarshal MUST NOT replay events.
	if evs := r.PullEvents(); len(evs) != 0 {
		t.Errorf("PullEvents after Unmarshal = %d, want 0", len(evs))
	}
}

func TestPullEvents_DrainsAndClears(t *testing.T) {
	t.Parallel()
	r := validNew(t)
	first := r.PullEvents()
	if len(first) != 1 {
		t.Fatalf("first PullEvents = %d, want 1", len(first))
	}
	second := r.PullEvents()
	if second != nil {
		t.Errorf("second PullEvents = %v, want nil", second)
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
