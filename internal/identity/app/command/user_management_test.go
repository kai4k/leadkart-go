package command_test


import (
	"errors"
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
	err := h.Handle(t.Context(), command.DeactivateUserCommand{MembershipID: m.ID()})
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
	if err := h.Handle(t.Context(), command.ReactivateUserCommand{MembershipID: m.ID()}); err != nil {
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
