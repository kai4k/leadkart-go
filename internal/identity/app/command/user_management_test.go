package command_test


import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership/membershiptest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)


// The membership-side fake lives in
// internal/identity/domain/membership/membershiptest/ per TDL Wild
// Workouts canon — co-located with the aggregate it fakes.
// newFakeMembershipRepo is preserved as a one-line alias so existing
// tests don't need rewriting.
func newFakeMembershipRepo() *membershiptest.FakeRepository { return membershiptest.NewFakeRepository() }

func newMembership(t *testing.T) *membership.Membership {
	t.Helper()
	m, err := membership.New(
		membership.ID("11111111-1111-1111-1111-111111111111"),
		person.ID("22222222-2222-2222-2222-222222222222"),
		tenant.ID("33333333-3333-3333-3333-333333333333"),
		membership.ID(""),
		testNow,
	)
	if err != nil {
		t.Fatalf("membership.New: %v", err)
	}
	m.PullEvents()
	return m
}

func TestUpdateUserProfile_Succeeds(t *testing.T) {
	t.Parallel()
	repo := newFakeMembershipRepo()
	m := newMembership(t)
	_ = repo.Add(t.Context(), m) // arch-test:ignore-err - test fixture setup

	h := command.NewUpdateUserProfileHandler(repo, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.UpdateUserProfileCommand{
			TenantID:      tenant.ID("33333333-3333-3333-3333-333333333333"),
		MembershipID:  m.ID(),
		Designation:   "Sales Manager",
		Department:    "North Zone",
		StatusMessage: "On the road",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if m.Designation() != "Sales Manager" {
		t.Errorf("Designation = %q", m.Designation())
	}
	if m.Department() != "North Zone" {
		t.Errorf("Department = %q", m.Department())
	}
}

func TestUpdateUserProfile_NotFound(t *testing.T) {
	t.Parallel()
	repo := newFakeMembershipRepo()
	h := command.NewUpdateUserProfileHandler(repo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.UpdateUserProfileCommand{
			TenantID:      tenant.ID("33333333-3333-3333-3333-333333333333"),
		MembershipID: membership.ID("99999999-9999-9999-9999-999999999999"),
	})
	if !errors.Is(err, command.ErrUserNotFound) {
		t.Fatalf("err = %v, want ErrUserNotFound", err)
	}
}

func TestDeactivateUser_RequiresReason(t *testing.T) {
	t.Parallel()
	repo := newFakeMembershipRepo()
	m := newMembership(t)
	_ = repo.Add(t.Context(), m) // arch-test:ignore-err - test fixture setup
	h := command.NewDeactivateUserHandler(repo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.DeactivateUserCommand{
			TenantID:     tenant.ID("33333333-3333-3333-3333-333333333333"),MembershipID: m.ID()})
	if !errors.Is(err, membership.ErrInvalid) {
		t.Fatalf("err = %v, want wraps membership.ErrInvalid (empty reason)", err)
	}
}

func TestDeactivateUser_Succeeds(t *testing.T) {
	t.Parallel()
	repo := newFakeMembershipRepo()
	m := newMembership(t)
	_ = repo.Add(t.Context(), m) // arch-test:ignore-err - test fixture setup
	h := command.NewDeactivateUserHandler(repo, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.DeactivateUserCommand{
			TenantID:     tenant.ID("33333333-3333-3333-3333-333333333333"),
		MembershipID: m.ID(),
		Reason:       "left-the-company",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if m.Status() != membership.StatusInactive {
		t.Errorf("Status = %v, want Inactive", m.Status())
	}
}

func TestReactivateUser_RoundTrip(t *testing.T) {
	t.Parallel()
	repo := newFakeMembershipRepo()
	m := newMembership(t)
	_ = m.Deactivate("temporary-leave", testNow) // arch-test:ignore-err - test fixture setup
	m.PullEvents()
	_ = repo.Add(t.Context(), m) // arch-test:ignore-err - test fixture setup

	h := command.NewReactivateUserHandler(repo, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.ReactivateUserCommand{
			TenantID:     tenant.ID("33333333-3333-3333-3333-333333333333"),MembershipID: m.ID()}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if m.Status() != membership.StatusActive {
		t.Errorf("Status = %v, want Active", m.Status())
	}
}

func TestReactivateUser_NotFound(t *testing.T) {
	t.Parallel()
	repo := newFakeMembershipRepo()
	h := command.NewReactivateUserHandler(repo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.ReactivateUserCommand{
			TenantID:     tenant.ID("33333333-3333-3333-3333-333333333333"),
		MembershipID: membership.ID("99999999-9999-9999-9999-999999999999"),
	})
	if !errors.Is(err, command.ErrUserNotFound) {
		t.Fatalf("err = %v, want ErrUserNotFound", err)
	}
}

func TestNewHandlers_PanicOnNilRepo(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		fn   func()
	}{
		{"UpdateUserProfile", func() { _ = command.NewUpdateUserProfileHandler(nil, func() time.Time { return testNow }) }},
		{"DeactivateUser", func() { _ = command.NewDeactivateUserHandler(nil, func() time.Time { return testNow }) }},
		{"ReactivateUser", func() { _ = command.NewReactivateUserHandler(nil, func() time.Time { return testNow }) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Error("expected panic on nil repo")
				}
			}()
			c.fn()
		})
	}
}

// TestUserManagementHandlers_InputRejections — boundary-input guards
// for every handler. Zero TenantID and zero MembershipID short-circuit
// before any repo is touched.
func TestUserManagementHandlers_InputRejections(t *testing.T) {
	t.Parallel()
	zeroTID := tenant.ID("")
	zeroMID := membership.ID("")
	someTID := tenant.ID("33333333-3333-3333-3333-333333333333")
	someMID := membership.ID("11111111-1111-1111-1111-111111111111")
	cases := []struct {
		name string
		fn   func() error
	}{
		{"UpdateUserProfile zero tenant", func() error {
			return command.NewUpdateUserProfileHandler(newFakeMembershipRepo(), func() time.Time { return testNow }).Handle(
				t.Context(), command.UpdateUserProfileCommand{TenantID: zeroTID, MembershipID: someMID})
		}},
		{"UpdateUserProfile zero membership", func() error {
			return command.NewUpdateUserProfileHandler(newFakeMembershipRepo(), func() time.Time { return testNow }).Handle(
				t.Context(), command.UpdateUserProfileCommand{TenantID: someTID, MembershipID: zeroMID})
		}},
		{"DeactivateUser zero tenant", func() error {
			return command.NewDeactivateUserHandler(newFakeMembershipRepo(), func() time.Time { return testNow }).Handle(
				t.Context(), command.DeactivateUserCommand{TenantID: zeroTID, MembershipID: someMID, Reason: "x"})
		}},
		{"DeactivateUser zero membership", func() error {
			return command.NewDeactivateUserHandler(newFakeMembershipRepo(), func() time.Time { return testNow }).Handle(
				t.Context(), command.DeactivateUserCommand{TenantID: someTID, MembershipID: zeroMID, Reason: "x"})
		}},
		{"ReactivateUser zero tenant", func() error {
			return command.NewReactivateUserHandler(newFakeMembershipRepo(), func() time.Time { return testNow }).Handle(
				t.Context(), command.ReactivateUserCommand{TenantID: zeroTID, MembershipID: someMID})
		}},
		{"ReactivateUser zero membership", func() error {
			return command.NewReactivateUserHandler(newFakeMembershipRepo(), func() time.Time { return testNow }).Handle(
				t.Context(), command.ReactivateUserCommand{TenantID: someTID, MembershipID: zeroMID})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := c.fn()
			if err == nil {
				t.Fatal("expected error, got nil")
			}
		})
	}
}

// TestDeactivateUser_AlreadyInactive_Idempotent exercises the aggregate-
// idempotent arm: Deactivate on an already-Inactive Membership returns
// nil + emits no event. The handler's closure path therefore commits
// no mutation — same shape as a first-time Deactivate from the handler
// caller's perspective (returns nil), but no DeactivatedEvent fires.
func TestDeactivateUser_AlreadyInactive_Idempotent(t *testing.T) {
	t.Parallel()
	repo := newFakeMembershipRepo()
	m := newMembership(t)
	if err := m.Deactivate("initial-leave", testNow); err != nil {
		t.Fatalf("setup Deactivate: %v", err)
	}
	_ = m.PullEvents()
	_ = repo.Add(t.Context(), m) // arch-test:ignore-err

	h := command.NewDeactivateUserHandler(repo, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.DeactivateUserCommand{
		TenantID:     tenant.ID("33333333-3333-3333-3333-333333333333"),
		MembershipID: m.ID(),
		Reason:       "second-deactivate-attempt",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if m.Status() != membership.StatusInactive {
		t.Errorf("Status = %v, want Inactive (unchanged)", m.Status())
	}
	if got := m.PullEvents(); len(got) != 0 {
		t.Errorf("PullEvents len = %d, want 0 (idempotent — no event)", len(got))
	}
}

// TestReactivateUser_AlreadyActive_Idempotent — Reactivate on an
// already-Active Membership returns nil + emits no event per the
// aggregate's no-op contract.
func TestReactivateUser_AlreadyActive_Idempotent(t *testing.T) {
	t.Parallel()
	repo := newFakeMembershipRepo()
	m := newMembership(t) // StatusActive by default
	_ = m.PullEvents()
	_ = repo.Add(t.Context(), m) // arch-test:ignore-err

	h := command.NewReactivateUserHandler(repo, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.ReactivateUserCommand{
		TenantID:     tenant.ID("33333333-3333-3333-3333-333333333333"),
		MembershipID: m.ID(),
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if m.Status() != membership.StatusActive {
		t.Errorf("Status = %v, want Active (unchanged)", m.Status())
	}
	if got := m.PullEvents(); len(got) != 0 {
		t.Errorf("PullEvents len = %d, want 0 (idempotent — no event)", len(got))
	}
}

// TestUpdateUserProfile_HappyPath_NoLengthRejection documents that
// Membership.UpdateProfile has NO length invariant on the input fields
// — long status messages, long designations, etc. all pass through.
// Per the BRD line N (per-tenant profile) admin UI should clamp; the
// aggregate intentionally stays permissive so retroactive policy
// tightening doesn't reject existing rows.
//
// (Branch documented; the more-substantial coverage lives in
// the existing TestUpdateUserProfile_Succeeds.)
func TestUpdateUserProfile_HappyPath_NoLengthRejection(t *testing.T) {
	t.Parallel()
	repo := newFakeMembershipRepo()
	m := newMembership(t)
	_ = repo.Add(t.Context(), m) // arch-test:ignore-err

	longMsg := strings.Repeat("a", 500)
	h := command.NewUpdateUserProfileHandler(repo, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.UpdateUserProfileCommand{
		TenantID:      tenant.ID("33333333-3333-3333-3333-333333333333"),
		MembershipID:  m.ID(),
		Designation:   "Sales Manager",
		Department:    "North Zone",
		StatusMessage: longMsg,
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
}
