package command_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Hierarchy round 4, I2: every authority command decides with the actor
// re-read inside the locked transaction (authorityWriter.locked). A token can
// only lose authority relative to the stored facts, never gain it, and a
// change to someone's roles, overrides or status ends their old access token.

// freshCase runs one command as `actor` against a prepared target; changed
// reports whether the target recorded any change.
type freshCase struct {
	name string
	run  func(t *testing.T, w *authzWorld, actor authority.Actor) (changed func() bool, err error)
}

func freshCases() []freshCase {
	p := permission.IdentityPermissions
	return []freshCase{
		{"assign role", func(t *testing.T, w *authzWorld, a authority.Actor) (func() bool, error) {
			target := w.member(t)
			err := w.handlers().assign.Handle(t.Context(), command.AssignUserRoleCommand{TenantID: awTenant, MembershipID: target.ID(), RoleID: w.exec.ID(), Actor: a})
			return func() bool { return len(target.RoleAssignments()) > 0 }, err
		}},
		{"revoke role", func(t *testing.T, w *authzWorld, a authority.Actor) (func() bool, error) {
			target := w.member(t, w.exec)
			err := w.handlers().revoke.Handle(t.Context(), command.RevokeUserRoleCommand{TenantID: awTenant, MembershipID: target.ID(), RoleID: w.exec.ID(), Actor: a})
			return func() bool { return len(target.RoleAssignments()) == 0 }, err
		}},
		{"replace overrides", func(t *testing.T, w *authzWorld, a authority.Actor) (func() bool, error) {
			target := w.member(t, w.exec)
			err := w.handlers().override.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: target.ID(),
				GrantedNames: []string{p.Crm.Leads.Read}, Actor: a})
			return func() bool { return len(target.GrantedPermissions()) > 0 }, err
		}},
		{"deactivate", func(t *testing.T, w *authzWorld, a authority.Actor) (func() bool, error) {
			target := w.member(t, w.exec)
			err := w.handlers().deact.Handle(t.Context(), command.DeactivateUserCommand{TenantID: awTenant, MembershipID: target.ID(), Reason: "fresh actor test", Actor: a})
			return func() bool { return target.Status() != membership.StatusActive }, err
		}},
		{"reactivate", func(t *testing.T, w *authzWorld, a authority.Actor) (func() bool, error) {
			target := w.memberSnap(t, membership.Snapshot{Status: membership.StatusInactive, RoleAssignments: []role.ID{w.exec.ID()}})
			err := w.handlers().react.Handle(t.Context(), command.ReactivateUserCommand{TenantID: awTenant, MembershipID: target.ID(), Actor: a})
			return func() bool { return target.Status() == membership.StatusActive }, err
		}},
		{"assign manager", func(t *testing.T, w *authzWorld, a authority.Actor) (func() bool, error) {
			mgr, sub := w.member(t, w.manager), w.member(t, w.exec)
			err := w.handlers().setMgr.Handle(t.Context(), command.AssignUserManagerCommand{TenantID: awTenant, MembershipID: sub.ID(), ManagerID: mgr.ID(), Actor: a})
			return func() bool { return !sub.ReportsTo().IsZero() }, err
		}},
		{"remove manager", func(t *testing.T, w *authzWorld, a authority.Actor) (func() bool, error) {
			mgr := w.member(t, w.manager)
			sub := w.memberReporting(t, mgr.ID(), w.exec)
			err := w.handlers().clearMgr.Handle(t.Context(), command.RemoveUserManagerCommand{TenantID: awTenant, MembershipID: sub.ID(), Actor: a})
			return func() bool { return sub.ReportsTo().IsZero() }, err
		}},
	}
}

// adminMember is a stored Administrator (tenant.admin) the tokens below name.
func adminMember(t *testing.T, w *authzWorld, status membership.Status) *membership.Membership {
	t.Helper()
	return w.memberSnap(t, membership.Snapshot{Status: status, RoleAssignments: []role.ID{w.admin.ID()}})
}

// TestAuthorityCommands_FreshActorMatchesTheToken is the control: the token
// and the stored facts agree, so every command works.
func TestAuthorityCommands_FreshActorMatchesTheToken(t *testing.T) {
	t.Parallel()
	for _, c := range freshCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := newAuthzWorld(t)
			changed, err := c.run(t, w, actorAs(t, adminMember(t, w, membership.StatusActive).ID(), authority.RankAdmin, tenantAdminEffective()...))
			if err != nil || !changed() {
				t.Fatalf("control: err = %v, changed = %v, want success", err, changed())
			}
		})
	}
}

// TestAuthorityCommands_ActorIsDecidedOnFreshFactsNotTheToken: the token says
// owner / admin with tenant.admin; the stored facts say otherwise. Each command
// must refuse and change nothing.
func TestAuthorityCommands_ActorIsDecidedOnFreshFactsNotTheToken(t *testing.T) {
	t.Parallel()
	tokenOwner := func(t *testing.T, id membership.ID) authority.Actor {
		return actorAs(t, id, authority.RankOwner, tenantAdminEffective()...)
	}
	for who, mkActor := range map[string]func(t *testing.T, w *authzWorld) authority.Actor{
		"the acting member does not exist": func(t *testing.T, _ *authzWorld) authority.Actor {
			return tokenOwner(t, membership.ID(awID(77777)))
		},
		"the acting member is inactive": func(t *testing.T, w *authzWorld) authority.Actor {
			return tokenOwner(t, adminMember(t, w, membership.StatusInactive).ID())
		},
		"the acting member lost every role": func(t *testing.T, w *authzWorld) authority.Actor {
			return tokenOwner(t, w.member(t).ID())
		},
		"an admin demoted to an executive": func(t *testing.T, w *authzWorld) authority.Actor {
			return actorAs(t, w.member(t, w.exec).ID(), authority.RankAdmin, tenantAdminEffective()...)
		},
	} {
		for _, c := range freshCases() {
			t.Run(who+"/"+c.name, func(t *testing.T) {
				t.Parallel()
				w := newAuthzWorld(t)
				changed, err := c.run(t, w, mkActor(t, w))
				if err == nil {
					t.Fatalf("the command succeeded for a stale actor")
				}
				if changed() {
					t.Errorf("the refused command changed the target")
				}
				if len(w.rotations) != 0 {
					t.Errorf("a refused command rotated stamps: %v", w.rotations)
				}
			})
		}
	}
}

// TestAuthorityCommands_ATokenNeverGainsAuthority: the stored facts are more
// senior than the token (a fresh promotion). The token's lower rank and
// narrower permissions still bind until the member refreshes.
func TestAuthorityCommands_ATokenNeverGainsAuthority(t *testing.T) {
	t.Parallel()
	for _, c := range freshCases() {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := newAuthzWorld(t)
			// Stored: Administrator with tenant.admin. Token: an executive with nothing.
			changed, err := c.run(t, w, actorAs(t, adminMember(t, w, membership.StatusActive).ID(), authority.RankUser))
			if err == nil || changed() {
				t.Fatalf("a token minted before a promotion gained authority: err = %v, changed = %v", err, changed())
			}
		})
	}
}

// TestReplaceOverrides_APermissionRevokedSinceTheTokenCannotBeConferred is the
// permission intersection: the token still lists identity.users.view, the stored
// roles and overrides no longer do.
func TestReplaceOverrides_APermissionRevokedSinceTheTokenCannotBeConferred(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	w := newAuthzWorld(t)
	// A manager who may change overrides, holding identity.users.view only by an override that was then revoked.
	mgr := w.memberSnap(t, membership.Snapshot{RoleAssignments: []role.ID{w.manager.ID()},
		GrantedPermissions: []membership.GrantedOverride{{Permission: permission.FromConstant(p.Users.UpdatePermissions)}},
		RevokedPermissions: []*permission.Permission{permission.FromConstant(p.Users.View)}})
	target := w.member(t, w.exec)
	actor := actorAs(t, mgr.ID(), authority.RankManager, p.Users.UpdatePermissions, p.Users.View)
	err := w.handlers().override.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: target.ID(),
		GrantedNames: []string{p.Users.View}, Actor: actor})
	if !isErr(err, authority.ErrGrantExceedsAuthority) {
		t.Fatalf("err = %v, want ErrGrantExceedsAuthority", err)
	}
	// A permission the stored facts still grant is conferrable.
	if err := w.handlers().override.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: target.ID(),
		GrantedNames: []string{p.Crm.Leads.Read}, Actor: actorAs(t, mgr.ID(), authority.RankManager, p.Users.UpdatePermissions, p.Crm.Leads.Read)}); err != nil {
		t.Fatalf("conferring a held permission: %v", err)
	}
}

// TestAuthorityCommands_ExemptActorsNeedNoMembershipInTheTenant: super users and
// operators under the cross-tenant exemption have no membership in the tenant
// they act in; the fresh read does not apply to them.
func TestAuthorityCommands_ExemptActorsNeedNoMembershipInTheTenant(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	target := w.member(t)
	if err := w.handlers().assign.Handle(t.Context(), command.AssignUserRoleCommand{TenantID: awTenant, MembershipID: target.ID(), RoleID: w.exec.ID(), Actor: superActor(t)}); err != nil {
		t.Fatalf("super user: %v", err)
	}
	rank := authority.RankNone.Int()
	op, err := authority.NewActor(authority.ActorInput{MembershipID: membership.ID(awID(9996)), TenantID: tenant.ID(awID(9995)), Rank: &rank,
		IsPlatformOperator: true, EffectivePermissions: []string{permission.IdentityPermissions.Platform.UsersManage}})
	if err != nil {
		t.Fatal(err)
	}
	target2 := w.member(t)
	if err := w.handlers().assign.Handle(t.Context(), command.AssignUserRoleCommand{TenantID: awTenant, MembershipID: target2.ID(), RoleID: w.exec.ID(), Actor: op}); err != nil {
		t.Fatalf("exempt operator in another tenant: %v", err)
	}
}

// TestAuthorityCommands_FreshReadHappensUnderTheLockAtTheOneClockReading: the
// fresh actor is loaded after the lock is taken and uses the command's clock.
func TestAuthorityCommands_FreshReadHappensUnderTheLockAtTheOneClockReading(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	reads := 0
	h := w.handlersAt(func() time.Time { reads++; return testNow })
	target := w.member(t)
	actor := actorAs(t, adminMember(t, w, membership.StatusActive).ID(), authority.RankAdmin, tenantAdminEffective()...)
	if err := h.assign.Handle(t.Context(), command.AssignUserRoleCommand{TenantID: awTenant, MembershipID: target.ID(), RoleID: w.exec.ID(), Actor: actor}); err != nil {
		t.Fatal(err)
	}
	if reads != 1 {
		t.Errorf("clock read %d times, want exactly 1", reads)
	}
	if w.lock.calls != 1 {
		t.Errorf("lock taken %d times, want 1", w.lock.calls)
	}
}

// ----- stamp rotation -------------------------------------------------------

// TestAuthorityCommands_RotateTheTargetsStampOnlyWhenSomethingChanged: role,
// override and status changes end the target's old token; an idempotent repeat,
// a manager change and a refused command rotate nothing.
func TestAuthorityCommands_RotateTheTargetsStampOnlyWhenSomethingChanged(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	h := w.handlers()
	ctx := t.Context()
	admin := actorAs(t, adminMember(t, w, membership.StatusActive).ID(), authority.RankAdmin, tenantAdminEffective()...)
	target := w.member(t)
	mgr := w.member(t, w.manager)
	step := func(name string, err error, want ...string) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !slices.Equal(w.rotations, want) {
			t.Fatalf("%s: rotations = %v, want %v", name, w.rotations, want)
		}
		w.rotations = nil
		// The real repository drains the aggregate's events when it persists; the fake does not.
		target.PullEvents()
	}
	assign := command.AssignUserRoleCommand{TenantID: awTenant, MembershipID: target.ID(), RoleID: w.exec.ID(), Actor: admin}
	step("assign", h.assign.Handle(ctx, assign), "role_assigned")
	step("assign again (idempotent)", h.assign.Handle(ctx, assign))
	step("overrides", h.override.Handle(ctx, command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: target.ID(),
		GrantedNames: []string{permission.IdentityPermissions.Crm.Leads.Read}, Actor: admin}), "overrides_replaced")
	step("set manager", h.setMgr.Handle(ctx, command.AssignUserManagerCommand{TenantID: awTenant, MembershipID: target.ID(), ManagerID: mgr.ID(), Actor: admin}))
	step("clear manager", h.clearMgr.Handle(ctx, command.RemoveUserManagerCommand{TenantID: awTenant, MembershipID: target.ID(), Actor: admin}))
	step("revoke", h.revoke.Handle(ctx, command.RevokeUserRoleCommand{TenantID: awTenant, MembershipID: target.ID(), RoleID: w.exec.ID(), Actor: admin}), "role_revoked")
	step("revoke again (idempotent)", h.revoke.Handle(ctx, command.RevokeUserRoleCommand{TenantID: awTenant, MembershipID: target.ID(), RoleID: w.exec.ID(), Actor: admin}))
	step("deactivate", h.deact.Handle(ctx, command.DeactivateUserCommand{TenantID: awTenant, MembershipID: target.ID(), Reason: "rotation test", Actor: admin}), "deactivated")
	step("deactivate again (idempotent)", h.deact.Handle(ctx, command.DeactivateUserCommand{TenantID: awTenant, MembershipID: target.ID(), Reason: "rotation test", Actor: admin}))
	step("reactivate", h.react.Handle(ctx, command.ReactivateUserCommand{TenantID: awTenant, MembershipID: target.ID(), Actor: admin}), "reactivated")
	step("reactivate again (idempotent)", h.react.Handle(ctx, command.ReactivateUserCommand{TenantID: awTenant, MembershipID: target.ID(), Actor: admin}))
}

// TestApprove_RotatesTheRequestersStamp: an approval writes an override, so the
// requester's old token must stop working.
func TestApprove_RotatesTheRequestersStamp(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	w := newPRWorld(t)
	c := w.cast(t)
	r := w.pending(t, c.exec, p.Crm.Leads.Read, 0)
	tlActor := actorAs(t, c.tl.ID(), authority.RankTeamLead, p.Roles.Approve, p.Crm.Leads.Read)
	if err := w.approveAs(tlActor, r); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if !slices.Equal(w.rotations, []string{"permission_approved"}) {
		t.Errorf("rotations = %v, want [permission_approved]", w.rotations)
	}
	w.rotations = nil
	// A denial writes no override and rotates nothing.
	if err := w.denyAs(tlActor, w.pending(t, c.exec, p.Crm.Leads.Read, 0)); err != nil {
		t.Fatalf("deny: %v", err)
	}
	if len(w.rotations) != 0 {
		t.Errorf("a denial rotated stamps: %v", w.rotations)
	}
}

// TestApprove_TheApproverIsReReadToo: a team lead demoted to an executive since
// the token was minted cannot approve.
func TestApprove_TheApproverIsReReadToo(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	w := newPRWorld(t)
	c := w.cast(t)
	r := w.pending(t, c.exec, p.Crm.Leads.Read, 0)
	demoted := w.member(t, w.exec)
	stale := actorAs(t, demoted.ID(), authority.RankTeamLead, p.Roles.Approve, p.Crm.Leads.Read)
	if err := w.approveAs(stale, r); err == nil {
		t.Fatal("a demoted approver approved")
	}
	if len(c.exec.GrantedPermissions()) != 0 || len(w.rotations) != 0 {
		t.Errorf("a refused approval changed state: grants %v, rotations %v", c.exec.GrantedPermissions(), w.rotations)
	}
}

func isErr(err, target error) bool { return errors.Is(err, target) }

// TestAssignRole_AnOwnerDemotedToManagerKeepsOnlyManagerRank: the token still
// says owner (rank 0); the stored facts say Sales Manager (rank 20). A role of
// level 10 is within the token's rank and outside the stored one.
func TestAssignRole_AnOwnerDemotedToManagerKeepsOnlyManagerRank(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	demoted := w.member(t, w.manager)
	target := w.member(t)
	stale := actorAs(t, demoted.ID(), authority.RankOwner, tenantAdminEffective()...)
	err := w.handlers().assign.Handle(t.Context(), command.AssignUserRoleCommand{TenantID: awTenant, MembershipID: target.ID(), RoleID: w.admin.ID(), Actor: stale})
	if !isErr(err, authority.ErrInsufficientRank) {
		t.Fatalf("err = %v, want ErrInsufficientRank", err)
	}
	if len(target.RoleAssignments()) != 0 {
		t.Error("the refused assignment changed the target")
	}
}
