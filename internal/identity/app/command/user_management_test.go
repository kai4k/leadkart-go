package command_test


import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)


// fakeMembershipRepo is the minimum [membership.Repository] surface
// the user_management handlers exercise.
type fakeMembershipRepo struct {
	memberships map[membership.ID]*membership.Membership
}

func newFakeMembershipRepo() *fakeMembershipRepo {
	return &fakeMembershipRepo{memberships: make(map[membership.ID]*membership.Membership)}
}

func (r *fakeMembershipRepo) Add(_ context.Context, m *membership.Membership) error {
	r.memberships[m.ID()] = m
	return nil
}

func (r *fakeMembershipRepo) UpdateByID(_ context.Context, id membership.ID, fn func(*membership.Membership) (bool, error)) error {
	m, ok := r.memberships[id]
	if !ok {
		return membership.ErrNotFound
	}
	commit, err := fn(m)
	if err != nil {
		return err
	}
	_ = commit
	return nil
}

func (r *fakeMembershipRepo) GetByID(_ context.Context, id membership.ID) (*membership.Membership, error) {
	m, ok := r.memberships[id]
	if !ok {
		return nil, membership.ErrNotFound
	}
	return m, nil
}

func (r *fakeMembershipRepo) GetActiveForPerson(_ context.Context, _ person.ID) (*membership.Membership, error) {
	return nil, membership.ErrNotFound
}

func (r *fakeMembershipRepo) ListForTenant(_ context.Context, tid tenant.ID) ([]*membership.Membership, error) {
	var out []*membership.Membership
	for _, m := range r.memberships {
		if m.TenantID() == tid {
			out = append(out, m)
		}
	}
	return out, nil
}

func (r *fakeMembershipRepo) ListAllForPerson(_ context.Context, pid person.ID) ([]*membership.Membership, error) {
	var out []*membership.Membership
	for _, m := range r.memberships {
		if m.PersonID() == pid {
			out = append(out, m)
		}
	}
	return out, nil
}

func (r *fakeMembershipRepo) HasActiveSuperAdmin(_ context.Context, _ tenant.ID) (bool, error) {
	return false, nil
}

func (r *fakeMembershipRepo) ListForTenantPage(_ context.Context, _ time.Time, _ string, _ int) ([]*membership.Membership, error) {
	// Tests in this package exercise the command path, not the paginated
	// list query. Empty page is the safe stub — query-layer tests live
	// in the query package against a real testcontainers DB.
	return nil, nil
}

var _ membership.Repository = (*fakeMembershipRepo)(nil)

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
