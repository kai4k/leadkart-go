package command_test


import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
)

func TestAssignUserRole_AddsAssignment(t *testing.T) {
	t.Parallel()
	repo := newFakeMembershipRepo()
	m := newMembership(t)
	_ = repo.Add(t.Context(), m) // arch-test:ignore-err - test fixture setup
	rid := role.ID("44444444-4444-4444-4444-444444444444")

	h := command.NewAssignUserRoleHandler(repo, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.AssignUserRoleCommand{
		MembershipID: m.ID(),
		RoleID:       rid,
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := m.RoleAssignments(); len(got) != 1 || got[0] != rid {
		t.Errorf("RoleAssignments = %v, want [%v]", got, rid)
	}
}

func TestRevokeUserRole_RemovesAssignment(t *testing.T) {
	t.Parallel()
	repo := newFakeMembershipRepo()
	m := newMembership(t)
	rid := role.ID("44444444-4444-4444-4444-444444444444")
	_ = m.AssignRole(rid, testNow) // arch-test:ignore-err - test fixture setup
	m.PullEvents()
	_ = repo.Add(t.Context(), m) // arch-test:ignore-err - test fixture setup

	h := command.NewRevokeUserRoleHandler(repo, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.RevokeUserRoleCommand{
		MembershipID: m.ID(),
		RoleID:       rid,
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := m.RoleAssignments(); len(got) != 0 {
		t.Errorf("RoleAssignments = %v, want empty", got)
	}
}

func TestReplaceUserPermissionOverrides_RejectsUnknownPermission(t *testing.T) {
	t.Parallel()
	repo := newFakeMembershipRepo()
	m := newMembership(t)
	_ = repo.Add(t.Context(), m) // arch-test:ignore-err - test fixture setup
	h := command.NewReplaceUserPermissionOverridesHandler(repo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{
		MembershipID: m.ID(),
		GrantedNames: []string{"identity.totally.fake"},
	})
	if !errors.Is(err, command.ErrPermissionUnknown) {
		t.Fatalf("err = %v, want ErrPermissionUnknown", err)
	}
}

func TestReplaceUserPermissionOverrides_HappyPath(t *testing.T) {
	t.Parallel()
	repo := newFakeMembershipRepo()
	m := newMembership(t)
	_ = repo.Add(t.Context(), m) // arch-test:ignore-err - test fixture setup
	h := command.NewReplaceUserPermissionOverridesHandler(repo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{
		MembershipID: m.ID(),
		GrantedNames: []string{permission.IdentityPermissions.Tenants.View},
		RevokedNames: []string{permission.IdentityPermissions.Users.Anonymise},
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := len(m.GrantedPermissions()); got != 1 {
		t.Errorf("GrantedPermissions count = %d, want 1", got)
	}
	if got := len(m.RevokedPermissions()); got != 1 {
		t.Errorf("RevokedPermissions count = %d, want 1", got)
	}
}

func TestAssignUserManager_Succeeds(t *testing.T) {
	t.Parallel()
	repo := newFakeMembershipRepo()
	m := newMembership(t)
	_ = repo.Add(t.Context(), m) // arch-test:ignore-err - test fixture setup
	managerID := membership.ID("55555555-5555-5555-5555-555555555555")

	h := command.NewAssignUserManagerHandler(repo, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.AssignUserManagerCommand{
		MembershipID: m.ID(),
		ManagerID:    managerID,
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if m.ReportsTo() != managerID {
		t.Errorf("ReportsTo = %v, want %v", m.ReportsTo(), managerID)
	}
}

func TestAssignUserManager_RejectsSelfManagement(t *testing.T) {
	t.Parallel()
	repo := newFakeMembershipRepo()
	m := newMembership(t)
	_ = repo.Add(t.Context(), m) // arch-test:ignore-err - test fixture setup
	h := command.NewAssignUserManagerHandler(repo, func() time.Time { return testNow })
	err := h.Handle(t.Context(), command.AssignUserManagerCommand{
		MembershipID: m.ID(),
		ManagerID:    m.ID(),
	})
	if !errors.Is(err, membership.ErrInvalid) {
		t.Fatalf("err = %v, want wraps membership.ErrInvalid (self-management)", err)
	}
}

func TestRemoveUserManager_RoundTrip(t *testing.T) {
	t.Parallel()
	repo := newFakeMembershipRepo()
	m := newMembership(t)
	managerID := membership.ID("55555555-5555-5555-5555-555555555555")
	_ = m.AssignManager(managerID, testNow) // arch-test:ignore-err - test fixture setup
	m.PullEvents()
	_ = repo.Add(t.Context(), m) // arch-test:ignore-err - test fixture setup

	h := command.NewRemoveUserManagerHandler(repo, func() time.Time { return testNow })
	if err := h.Handle(t.Context(), command.RemoveUserManagerCommand{MembershipID: m.ID()}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !m.ReportsTo().IsZero() {
		t.Errorf("ReportsTo = %v, want zero", m.ReportsTo())
	}
}

func TestAuthzHandlers_NotFound(t *testing.T) {
	t.Parallel()
	repo := newFakeMembershipRepo()
	bad := membership.ID("99999999-9999-9999-9999-999999999999")
	rid := role.ID("44444444-4444-4444-4444-444444444444")
	mid := membership.ID("55555555-5555-5555-5555-555555555555")

	cases := []struct {
		name string
		fn   func() error
	}{
		{"AssignRole", func() error {
			return command.NewAssignUserRoleHandler(repo, func() time.Time { return testNow }).Handle(t.Context(),
				command.AssignUserRoleCommand{MembershipID: bad, RoleID: rid})
		}},
		{"RevokeRole", func() error {
			return command.NewRevokeUserRoleHandler(repo, func() time.Time { return testNow }).Handle(t.Context(),
				command.RevokeUserRoleCommand{MembershipID: bad, RoleID: rid})
		}},
		{"ReplaceOverrides", func() error {
			return command.NewReplaceUserPermissionOverridesHandler(repo, func() time.Time { return testNow }).Handle(t.Context(),
				command.ReplaceUserPermissionOverridesCommand{MembershipID: bad})
		}},
		{"AssignManager", func() error {
			return command.NewAssignUserManagerHandler(repo, func() time.Time { return testNow }).Handle(t.Context(),
				command.AssignUserManagerCommand{MembershipID: bad, ManagerID: mid})
		}},
		{"RemoveManager", func() error {
			return command.NewRemoveUserManagerHandler(repo, func() time.Time { return testNow }).Handle(t.Context(),
				command.RemoveUserManagerCommand{MembershipID: bad})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			err := c.fn()
			if !errors.Is(err, command.ErrUserNotFound) {
				t.Errorf("err = %v, want ErrUserNotFound", err)
			}
		})
	}
}
