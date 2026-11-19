package command_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Ported from the pre-authority user_authz_test.go: input rejection, not-found,
// idempotency and the happy paths, now through the authority-aware handlers
// with an Administrator actor over members that hold no role.

func TestAssignUserRole_AddsAssignment(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	target := w.member(t)
	if err := w.handlers().assign.Handle(t.Context(), command.AssignUserRoleCommand{
		TenantID: awTenant, MembershipID: target.ID(), RoleID: w.exec.ID(), Actor: testAdminActor(t),
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := target.RoleAssignments(); len(got) != 1 || got[0] != w.exec.ID() {
		t.Errorf("RoleAssignments = %v, want [%v]", got, w.exec.ID())
	}
}

func TestRevokeUserRole_RemovesAssignment(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	target := w.member(t, w.exec)
	if err := w.handlers().revoke.Handle(t.Context(), command.RevokeUserRoleCommand{
		TenantID: awTenant, MembershipID: target.ID(), RoleID: w.exec.ID(), Actor: testAdminActor(t),
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := target.RoleAssignments(); len(got) != 0 {
		t.Errorf("RoleAssignments = %v, want empty", got)
	}
}

func TestRevokeUserRole_NonExistentAssignment_Idempotent(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	target := w.member(t)
	if err := w.handlers().revoke.Handle(t.Context(), command.RevokeUserRoleCommand{
		TenantID: awTenant, MembershipID: target.ID(), RoleID: role.ID("0190a000-0000-7000-8000-0000000000ee"), Actor: testAdminActor(t),
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := target.PullEvents(); len(got) != 0 {
		t.Errorf("PullEvents len = %d, want 0 (idempotent revoke)", len(got))
	}
}

func TestReplaceUserPermissionOverrides_RejectsUnknownPermission(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	target := w.member(t)
	err := w.handlers().override.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{
		TenantID: awTenant, MembershipID: target.ID(), GrantedNames: []string{"not.a.permission"}, Actor: testAdminActor(t),
	})
	if !errors.Is(err, command.ErrPermissionUnknown) {
		t.Fatalf("err = %v, want ErrPermissionUnknown", err)
	}
}

func TestReplaceUserPermissionOverrides_GrantsAndRevokes_BothApplied(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	target := w.member(t)
	if err := w.handlers().override.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{
		TenantID: awTenant, MembershipID: target.ID(), Actor: testAdminActor(t),
		GrantedNames: []string{permission.IdentityPermissions.Tenants.View, permission.IdentityPermissions.Users.Create},
		RevokedNames: []string{permission.IdentityPermissions.Users.Unlock, permission.IdentityPermissions.Roles.Delete},
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := len(target.GrantedPermissions()); got != 2 {
		t.Errorf("GrantedPermissions count = %d, want 2", got)
	}
	if got := len(target.RevokedPermissions()); got != 2 {
		t.Errorf("RevokedPermissions count = %d, want 2", got)
	}
}

func TestRemoveUserManager_AlreadyNoManager_Idempotent(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	target := w.member(t)
	if err := w.handlers().clearMgr.Handle(t.Context(), command.RemoveUserManagerCommand{
		TenantID: awTenant, MembershipID: target.ID(), Actor: testAdminActor(t),
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := target.PullEvents(); len(got) != 0 {
		t.Errorf("PullEvents len = %d, want 0 (idempotent)", len(got))
	}
}

func TestAuthzHandlers_InputRejections(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	h := w.handlers()
	actor := testAdminActor(t)
	someM, someR := membership.ID("11111111-1111-1111-1111-111111111111"), role.ID("44444444-4444-4444-4444-444444444444")
	cases := map[string]error{
		"AssignUserRole zero tenant":      h.assign.Handle(t.Context(), command.AssignUserRoleCommand{MembershipID: someM, RoleID: someR, Actor: actor}),
		"AssignUserRole zero membership":  h.assign.Handle(t.Context(), command.AssignUserRoleCommand{TenantID: awTenant, RoleID: someR, Actor: actor}),
		"AssignUserRole zero role":        h.assign.Handle(t.Context(), command.AssignUserRoleCommand{TenantID: awTenant, MembershipID: someM, Actor: actor}),
		"RevokeUserRole zero tenant":      h.revoke.Handle(t.Context(), command.RevokeUserRoleCommand{MembershipID: someM, RoleID: someR, Actor: actor}),
		"RevokeUserRole zero membership":  h.revoke.Handle(t.Context(), command.RevokeUserRoleCommand{TenantID: awTenant, RoleID: someR, Actor: actor}),
		"RevokeUserRole zero role":        h.revoke.Handle(t.Context(), command.RevokeUserRoleCommand{TenantID: awTenant, MembershipID: someM, Actor: actor}),
		"AssignUserManager zero manager":  h.setMgr.Handle(t.Context(), command.AssignUserManagerCommand{TenantID: awTenant, MembershipID: someM, Actor: actor}),
		"AssignUserManager zero tenant":   h.setMgr.Handle(t.Context(), command.AssignUserManagerCommand{MembershipID: someM, ManagerID: someM, Actor: actor}),
		"RemoveUserManager zero tenant":   h.clearMgr.Handle(t.Context(), command.RemoveUserManagerCommand{MembershipID: someM, Actor: actor}),
		"ReplaceOverrides zero tenant":    h.override.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{MembershipID: someM, Actor: actor}),
		"ReplaceOverrides zero member":    h.override.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, Actor: actor}),
		"AssignUserManager self (raw id)": h.setMgr.Handle(t.Context(), command.AssignUserManagerCommand{TenantID: awTenant, MembershipID: someM, ManagerID: someM, Actor: actor}),
	}
	for name, err := range cases {
		if err == nil {
			t.Errorf("%s: expected an error, got nil", name)
		}
	}
	if w.lock.calls != 0 {
		t.Errorf("lock taken %d times for rejected input, want 0", w.lock.calls)
	}
}

func TestAuthzHandlers_NotFound(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	h := w.handlers()
	actor := testAdminActor(t)
	bad := membership.ID("99999999-9999-9999-9999-999999999999")
	mgr := w.member(t, w.manager)
	cases := map[string]error{
		"AssignRole":       h.assign.Handle(t.Context(), command.AssignUserRoleCommand{TenantID: awTenant, MembershipID: bad, RoleID: w.exec.ID(), Actor: actor}),
		"RevokeRole":       h.revoke.Handle(t.Context(), command.RevokeUserRoleCommand{TenantID: awTenant, MembershipID: bad, RoleID: w.exec.ID(), Actor: actor}),
		"ReplaceOverrides": h.override.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: bad, Actor: actor}),
		"AssignManager":    h.setMgr.Handle(t.Context(), command.AssignUserManagerCommand{TenantID: awTenant, MembershipID: bad, ManagerID: mgr.ID(), Actor: actor}),
		"RemoveManager":    h.clearMgr.Handle(t.Context(), command.RemoveUserManagerCommand{TenantID: awTenant, MembershipID: bad, Actor: actor}),
		"Deactivate":       h.deact.Handle(t.Context(), command.DeactivateUserCommand{TenantID: awTenant, MembershipID: bad, Reason: "x", Actor: actor}),
		"Reactivate":       h.react.Handle(t.Context(), command.ReactivateUserCommand{TenantID: awTenant, MembershipID: bad, Actor: actor}),
	}
	for name, err := range cases {
		if !errors.Is(err, command.ErrUserNotFound) {
			t.Errorf("%s: err = %v, want ErrUserNotFound", name, err)
		}
	}
	// a membership of ANOTHER tenant is invisible, not forbidden
	if err := h.react.Handle(t.Context(), command.ReactivateUserCommand{TenantID: tenant.ID("55555555-5555-5555-5555-555555555555"), MembershipID: mgr.ID(), Actor: actor}); !errors.Is(err, command.ErrUserNotFound) && !errors.Is(err, authority.ErrNoAuthority) {
		t.Errorf("cross-tenant target: %v", err)
	}
}
