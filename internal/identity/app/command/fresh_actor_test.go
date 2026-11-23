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
			err := w.handlers().override.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: target.ID(), OverridesVersion: w.ver(t, target),
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
	err := w.handlers().override.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: target.ID(), OverridesVersion: w.ver(t, target),
		GrantedNames: []string{p.Users.View}, Actor: actor})
	if !isErr(err, authority.ErrGrantExceedsAuthority) {
		t.Fatalf("err = %v, want ErrGrantExceedsAuthority", err)
	}
	// A permission the stored facts still grant is conferrable.
	if err := w.handlers().override.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: target.ID(), OverridesVersion: w.ver(t, target),
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
	step("overrides", h.override.Handle(ctx, command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: target.ID(), OverridesVersion: w.ver(t, target),
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

// ----- profile edits of other members ---------------------------------------

// Editing ANOTHER member's profile is an authority decision (MayActOn); editing
// one's own is unchanged.
func TestUpdateUserProfile_OtherMembersAreRankGated(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	edit := func(actor authority.Actor, target *membership.Membership) error {
		return command.NewUpdateUserProfileHandler(fakeUoW{}, w.lock, w.members, w.roles, anyPersons{rotations: &w.rotations}, awClock).Handle(t.Context(),
			command.UpdateUserProfileCommand{TenantID: awTenant, MembershipID: target.ID(), Designation: "edited", Actor: actor})
	}
	hr := w.member(t, w.manager) // identity.users.update by role or override, rank 20
	hrActor := actorAs(t, hr.ID(), authority.RankManager, permission.IdentityPermissions.Users.Update)
	owner := w.member(t, w.owner)
	admin := w.member(t, w.admin)
	peer := w.member(t, w.manager)
	exec := w.member(t, w.exec)

	for name, target := range map[string]*membership.Membership{"an owner": owner, "an Administrator": admin, "a peer manager": peer} {
		if err := edit(hrActor, target); !isErr(err, authority.ErrInsufficientRank) {
			t.Errorf("a manager edits %s's profile: %v, want ErrInsufficientRank", name, err)
		}
		if target.Designation() == "edited" {
			t.Errorf("the refused edit of %s's profile was written", name)
		}
	}
	if err := edit(hrActor, exec); err != nil || exec.Designation() != "edited" {
		t.Errorf("a manager edits an executive's profile: %v (designation %q), want allowed", err, exec.Designation())
	}
	// Self-edit is unchanged, for any rank.
	self := w.member(t, w.exec)
	if err := edit(actorAs(t, self.ID(), authority.RankUser), self); err != nil || self.Designation() != "edited" {
		t.Errorf("an executive edits their own profile: %v", err)
	}
	// The edit runs under the tenant lock and re-reads the acting member.
	if w.lock.calls == 0 {
		t.Error("profile edits do not take the tenant authority lock")
	}
	gone := actorAs(t, membership.ID(awID(88888)), authority.RankOwner, tenantAdminEffective()...)
	if err := edit(gone, exec); !isErr(err, authority.ErrNoAuthority) {
		t.Errorf("a stale actor edits a profile: %v, want ErrNoAuthority", err)
	}
	if err := edit(authority.Actor{}, exec); !isErr(err, authority.ErrNoAuthority) {
		t.Errorf("zero actor: %v, want ErrNoAuthority", err)
	}
	if len(w.rotations) != 0 {
		t.Errorf("a profile edit rotated stamps: %v", w.rotations)
	}
}

// ----- AssignUserManager: authority is decided before eligibility ------------

// A caller who may not act on the subordinate must not learn anything about the
// manager or the reporting chain: they get the authority answer, not
// manager_not_eligible.
func TestAssignUserManager_AuthorityIsDecidedBeforeEligibility(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	h := w.handlers().setMgr
	mgrActorMember := w.member(t, w.manager)
	actor := actorAs(t, mgrActorMember.ID(), authority.RankManager, tenantAdminEffective()...)
	admin := w.member(t, w.admin) // the actor does not outrank an Administrator
	reportsToAdmin := w.memberReporting(t, admin.ID(), w.exec)

	for name, mgrID := range map[string]membership.ID{
		"the manager does not exist":        membership.ID(awID(99999)),
		"the manager would form a cycle":    reportsToAdmin.ID(), // reports to admin, so admin -> reportsToAdmin closes a loop
		"the manager is less senior (rank)": w.member(t, w.exec).ID(),
	} {
		err := h.Handle(t.Context(), command.AssignUserManagerCommand{TenantID: awTenant, MembershipID: admin.ID(), ManagerID: mgrID, Actor: actor})
		if !isErr(err, authority.ErrInsufficientRank) {
			t.Errorf("%s: %v, want ErrInsufficientRank (authority first)", name, err)
		}
		if isErr(err, authority.ErrManagerNotEligible) {
			t.Errorf("%s: the answer leaks eligibility information: %v", name, err)
		}
	}
	// An authorised caller still gets the eligibility answers.
	owner := actorAs(t, w.member(t, w.owner).ID(), authority.RankOwner, tenantAdminEffective()...)
	sub := w.member(t, w.exec)
	if err := h.Handle(t.Context(), command.AssignUserManagerCommand{TenantID: awTenant, MembershipID: sub.ID(), ManagerID: membership.ID(awID(99999)), Actor: owner}); !isErr(err, authority.ErrManagerNotEligible) {
		t.Errorf("authorised caller, missing manager: %v, want ErrManagerNotEligible", err)
	}
	if err := h.Handle(t.Context(), command.AssignUserManagerCommand{TenantID: awTenant, MembershipID: admin.ID(), ManagerID: reportsToAdmin.ID(), Actor: owner}); !isErr(err, authority.ErrManagerNotEligible) {
		t.Errorf("authorised caller, cycle: %v, want ErrManagerNotEligible", err)
	}
}

// A legitimate reporting chain of several levels is accepted; only a chain
// deeper than the walk bound is refused.
func TestAssignUserManager_LongChainsAreAcceptedUpToTheBound(t *testing.T) {
	t.Parallel()
	owner := func(w *authzWorld) authority.Actor {
		return actorAs(t, w.member(t, w.owner).ID(), authority.RankOwner, tenantAdminEffective()...)
	}
	for _, depth := range []int{1, 3, 5, 20, 63} {
		w := newAuthzWorld(t)
		// top <- ... <- manager at the bottom of a chain of `depth` links, all admin-or-above ranks.
		cur := w.member(t, w.admin)
		for range depth {
			cur = w.memberReporting(t, cur.ID(), w.manager)
		}
		sub := w.member(t, w.exec)
		if err := w.handlers().setMgr.Handle(t.Context(), command.AssignUserManagerCommand{TenantID: awTenant, MembershipID: sub.ID(), ManagerID: cur.ID(), Actor: owner(w)}); err != nil {
			t.Errorf("chain of %d levels above the manager: %v, want accepted", depth, err)
		}
		if sub.ReportsTo() != cur.ID() {
			t.Errorf("chain of %d levels: reports_to = %q, want %q", depth, sub.ReportsTo(), cur.ID())
		}
	}
	w := newAuthzWorld(t)
	cur := w.member(t, w.admin)
	for range 64 {
		cur = w.memberReporting(t, cur.ID(), w.manager)
	}
	sub := w.member(t, w.exec)
	if err := w.handlers().setMgr.Handle(t.Context(), command.AssignUserManagerCommand{TenantID: awTenant, MembershipID: sub.ID(), ManagerID: cur.ID(), Actor: owner(w)}); !isErr(err, authority.ErrManagerNotEligible) {
		t.Errorf("a chain of 64 links above the manager (the walk bound): %v, want ErrManagerNotEligible", err)
	}
}

// TestAuthorityCommands_ASuperUserInTheirOwnTenantIsReRead: a super user acting in
// the tenant their membership lives in (the platform tenant) is decided on the
// stored membership like everyone else: the super-admin role must still be held,
// and the membership active. Acting in another tenant stays bounded by the stamp
// (TestAuthorityCommands_ExemptActorsNeedNoMembershipInTheTenant).
func TestAuthorityCommands_ASuperUserInTheirOwnTenantIsReRead(t *testing.T) {
	t.Parallel()
	superToken := func(t *testing.T, id membership.ID) authority.Actor {
		r := authority.RankOwner.Int()
		a, err := authority.NewActor(authority.ActorInput{MembershipID: id, TenantID: awTenant, Rank: &r, IsSuperUser: true})
		if err != nil {
			t.Fatalf("NewActor: %v", err)
		}
		return a
	}
	setup := func(t *testing.T) (*authzWorld, *role.Role) {
		w := newAuthzWorld(t)
		return w, w.addRoleSnap(t, role.Snapshot{ID: role.ID(awID(60)), TenantID: awTenant, Name: "Super Admin", IsSystemDefault: true, IsSuperAdmin: true,
			HierarchyLevel: role.HierarchyLevelOwner, CreatedAt: testNow})
	}
	for name, c := range map[string]struct {
		member func(w *authzWorld, super *role.Role) *membership.Membership
		ok     bool
	}{
		"still holds the super-admin role": {func(w *authzWorld, super *role.Role) *membership.Membership {
			return w.memberSnap(t, membership.Snapshot{RoleAssignments: []role.ID{super.ID()}})
		}, true},
		"the super-admin role was removed": {func(w *authzWorld, _ *role.Role) *membership.Membership { return w.member(t, w.exec) }, false},
		"the membership is inactive": {func(w *authzWorld, super *role.Role) *membership.Membership {
			return w.memberSnap(t, membership.Snapshot{Status: membership.StatusInactive, RoleAssignments: []role.ID{super.ID()}})
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w, super := setup(t)
			actor := superToken(t, c.member(w, super).ID())
			target := w.member(t)
			err := w.handlers().assign.Handle(t.Context(), command.AssignUserRoleCommand{TenantID: awTenant, MembershipID: target.ID(), RoleID: w.exec.ID(), Actor: actor})
			if c.ok && err != nil {
				t.Fatalf("a super admin acting in their own tenant: %v", err)
			}
			if !c.ok && !isErr(err, authority.ErrNoAuthority) {
				t.Fatalf("err = %v, want ErrNoAuthority", err)
			}
			if !c.ok && len(target.RoleAssignments()) != 0 {
				t.Error("the refused command changed the target")
			}
		})
	}
}

// TestViewPermissionRequest_TheViewerIsReReadToo: the single-request view decides
// on the stored facts like approve and the queue. A team lead demoted to an
// executive since the token was minted no longer sees their ex-report's request,
// and a requester who was deactivated no longer sees their own.
func TestViewPermissionRequest_TheViewerIsReReadToo(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	w := newPRWorld(t)
	c := w.cast(t)
	r := w.pending(t, c.exec, p.Crm.Leads.Read, 0)
	view := func(a authority.Actor) error {
		return w.view.Handle(t.Context(), command.AuthorizePermissionRequestViewCommand{TenantID: awTenant, RequestID: r.ID(), Actor: a})
	}
	if err := view(actorAs(t, c.tl.ID(), authority.RankTeamLead, p.Roles.Approve)); err != nil {
		t.Fatalf("control: the team lead sees their report's request: %v", err)
	}
	if err := view(actorAs(t, c.exec.ID(), authority.RankUser)); err != nil {
		t.Fatalf("control: the requester sees their own request: %v", err)
	}
	// A team lead demoted to an executive since the token was minted, still the requester's manager on record.
	demoted := w.member(t, w.exec)
	reportOfDemoted := w.memberReporting(t, demoted.ID(), w.exec)
	r3 := w.pending(t, reportOfDemoted, p.Crm.Leads.Read, 0)
	stale := actorAs(t, demoted.ID(), authority.RankTeamLead, p.Roles.Approve)
	if err := w.view.Handle(t.Context(), command.AuthorizePermissionRequestViewCommand{TenantID: awTenant, RequestID: r3.ID(), Actor: stale}); !errors.Is(err, command.ErrPermissionRequestNotFound) {
		t.Errorf("a demoted viewer: err = %v, want not found", err)
	}
	gone := w.memberSnap(t, membership.Snapshot{Status: membership.StatusInactive, RoleAssignments: []role.ID{w.exec.ID()}})
	r2 := w.pending(t, gone, p.Crm.Leads.Read, 0)
	if err := w.view.Handle(t.Context(), command.AuthorizePermissionRequestViewCommand{TenantID: awTenant, RequestID: r2.ID(), Actor: actorAs(t, gone.ID(), authority.RankUser)}); !errors.Is(err, command.ErrPermissionRequestNotFound) {
		t.Errorf("a deactivated requester viewing their own request: err = %v, want not found", err)
	}
}

// TestReplaceOverrides_AnUnchangedSaveWritesNothingAndRotatesNothing: the same
// overrides again (in any order) leave the member untouched and do not rotate their security stamp; a real change still rotates once.
func TestReplaceOverrides_AnUnchangedSaveWritesNothingAndRotatesNothing(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	w := newAuthzWorld(t)
	admin := adminMember(t, w, membership.StatusActive)
	actor := actorAs(t, admin.ID(), authority.RankAdmin, tenantAdminEffective()...)
	target := w.member(t, w.exec)
	save := func(granted, revoked []string) error {
		return w.handlers().override.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: target.ID(), OverridesVersion: w.ver(t, target),
			GrantedNames: granted, RevokedNames: revoked, Actor: actor})
	}
	if err := save([]string{p.Crm.Leads.Read, p.Crm.Leads.Assign}, []string{p.Inventory.Stock.Read}); err != nil {
		t.Fatalf("first save: %v", err)
	}
	if len(w.rotations) != 1 {
		t.Fatalf("a real change rotated %d times, want 1", len(w.rotations))
	}
	for _, again := range [][2][]string{
		{{p.Crm.Leads.Read, p.Crm.Leads.Assign}, {p.Inventory.Stock.Read}},
		{{p.Crm.Leads.Assign, p.Crm.Leads.Read}, {p.Inventory.Stock.Read}}, // other order
	} {
		if err := save(again[0], again[1]); err != nil {
			t.Fatalf("repeat save: %v", err)
		}
		if len(w.rotations) != 1 {
			t.Fatalf("an unchanged save rotated the stamp (%d rotations)", len(w.rotations))
		}
	}
	if err := save([]string{p.Crm.Leads.Read}, []string{p.Inventory.Stock.Read}); err != nil || len(w.rotations) != 2 {
		t.Fatalf("a real change: err %v, rotations %d, want 2", err, len(w.rotations))
	}
	// An empty change on an empty member is a no-op as well.
	fresh := w.member(t, w.exec)
	if err := w.handlers().override.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: fresh.ID(), OverridesVersion: w.ver(t, fresh), Actor: actor}); err != nil || len(w.rotations) != 2 {
		t.Fatalf("empty save on an empty member: err %v, rotations %d, want no rotation", err, len(w.rotations))
	}
}
