package command_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership/membershiptest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permissionrequest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permissionrequest/permissionrequesttest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// submitterActor is a valid, non-impersonating actor for the submit tests.
func submitterActor(t *testing.T) authority.Actor {
	t.Helper()
	return actorAs(t, membership.ID(awID(9001)), authority.RankUser, permission.IdentityPermissions.Meta.RequestPermissionElevation)
}

// prWorld is an authzWorld plus a permission-request store and the three
// approval handlers over it.
type prWorld struct {
	*authzWorld
	reqs    *permissionrequesttest.FakeRepository
	approve command.ApprovePermissionRequestHandler
	deny    command.DenyPermissionRequestHandler
	view    command.AuthorizePermissionRequestViewHandler
	queue   command.ListDecidablePermissionRequestsHandler
	nextReq int
	t       *testing.T
}

func newPRWorld(t *testing.T) *prWorld {
	t.Helper()
	w := &prWorld{authzWorld: newAuthzWorld(t), reqs: permissionrequesttest.NewFakeRepository(), t: t}
	l, p := command.AuthorityLocker(w.lock), anyPersons{rotations: &w.rotations}
	w.approve = command.NewApprovePermissionRequestHandler(fakeUoW{}, l, w.reqs, w.members, w.roles, p, awClock, func() uuid.UUID { return uuid.MustParse(awID(7000)) })
	w.deny = command.NewDenyPermissionRequestHandler(fakeUoW{}, l, w.reqs, w.members, w.roles, p, awClock)
	w.view = command.NewAuthorizePermissionRequestViewHandler(w.reqs, w.members, w.roles, p, awClock)
	w.reqs.ManagerLookup(func(id membership.ID) membership.ID {
		m, err := w.members.GetByID(t.Context(), awTenant, id)
		if err != nil {
			return ""
		}
		return m.ReportsTo()
	})
	w.queue = command.NewListDecidablePermissionRequestsHandler(fakeUoW{}, w.reqs, w.members, w.roles, p, awClock)
	return w
}

// pending files a pending request for requester (7 days unless days > 0).
func (w *prWorld) pending(t *testing.T, requester *membership.Membership, perm string, days int) *permissionrequest.Request {
	t.Helper()
	w.nextReq++
	if days == 0 {
		days = 7
	}
	r, err := permissionrequest.New(permissionrequest.ID(awID(3000+w.nextReq)), awTenant, requester.ID(), permission.FromConstant(perm), days, "needed for the quarterly close", testNow)
	if err != nil {
		t.Fatalf("permissionrequest.New: %v", err)
	}
	if err := w.reqs.Add(t.Context(), r); err != nil {
		t.Fatalf("add request: %v", err)
	}
	return r
}

func (w *prWorld) approveAs(a authority.Actor, r *permissionrequest.Request) error {
	return w.approve.Handle(w.t.Context(), command.ApprovePermissionRequestCommand{TenantID: awTenant, RequestID: r.ID(), DecisionReason: "ok", Actor: a})
}

func (w *prWorld) denyAs(a authority.Actor, r *permissionrequest.Request) error {
	return w.deny.Handle(w.t.Context(), command.DenyPermissionRequestCommand{TenantID: awTenant, RequestID: r.ID(), DecisionReason: "not now", Actor: a})
}

func grantOf(m *membership.Membership, name string) (membership.GrantedOverride, bool) {
	for _, g := range m.GrantedPermissions() {
		if g.Permission.Name() == name {
			return g, true
		}
	}
	return membership.GrantedOverride{}, false
}

// prCast is a Team Lead T with one report E (the chain's cast), a Sales Manager
// above both, an Administrator and an unrelated Sales Executive.
type prCast struct {
	tl, exec, mgr, admin, other *membership.Membership
}

func (w *prWorld) cast(t *testing.T) prCast {
	t.Helper()
	tl := w.member(t, w.teamLead)
	return prCast{
		tl:    tl,
		exec:  w.memberReporting(t, tl.ID(), w.exec),
		mgr:   w.member(t, w.manager),
		admin: w.member(t, w.admin),
		other: w.member(t, w.exec),
	}
}

func TestPermissionRequest_ZeroActorDeniedBeforeLock(t *testing.T) {
	t.Parallel()
	w := newPRWorld(t)
	c := w.cast(t)
	r := w.pending(t, c.exec, permission.IdentityPermissions.Crm.Leads.Read, 0)
	if err := w.approveAs(authority.Actor{}, r); !errors.Is(err, authority.ErrNoAuthority) {
		t.Errorf("approve: %v, want ErrNoAuthority", err)
	}
	if err := w.denyAs(authority.Actor{}, r); !errors.Is(err, authority.ErrNoAuthority) {
		t.Errorf("deny: %v, want ErrNoAuthority", err)
	}
	if _, err := w.queue.Handle(t.Context(), command.ListDecidablePermissionRequestsQuery{TenantID: awTenant}); !errors.Is(err, authority.ErrNoAuthority) {
		t.Errorf("queue: %v, want ErrNoAuthority", err)
	}
	if err := w.view.Handle(t.Context(), command.AuthorizePermissionRequestViewCommand{TenantID: awTenant, RequestID: r.ID()}); !errors.Is(err, command.ErrPermissionRequestNotFound) {
		t.Errorf("view: %v, want ErrPermissionRequestNotFound", err)
	}
	if w.lock.calls != 0 {
		t.Fatalf("lock taken %d times for a zero actor", w.lock.calls)
	}
	h := command.NewRequestPermissionElevationHandler(w.reqs, w.members, awClock, func() permissionrequest.ID { return permissionrequest.ID(awID(3999)) })
	if _, err := h.Handle(t.Context(), command.RequestPermissionElevationCommand{TenantID: awTenant, RequesterMembershipID: c.exec.ID(), Permission: permission.FromConstant(permission.IdentityPermissions.Crm.Leads.Read), Reason: "needed for the quarterly close"}); !errors.Is(err, authority.ErrNoAuthority) {
		t.Errorf("submit with a zero actor: %v, want ErrNoAuthority", err)
	}
}

func TestSubmit_ImpersonationSessionCannotSubmit(t *testing.T) {
	t.Parallel()
	w := newPRWorld(t)
	c := w.cast(t)
	rank := authority.RankOwner.Int()
	imp, err := authority.NewActor(authority.ActorInput{MembershipID: c.exec.ID(), TenantID: awTenant, Rank: &rank, IsImpersonation: true,
		EffectivePermissions: []string{permission.IdentityPermissions.Meta.TenantAdmin}})
	if err != nil {
		t.Fatal(err)
	}
	h := command.NewRequestPermissionElevationHandler(w.reqs, w.members, awClock, func() permissionrequest.ID { return permissionrequest.ID(awID(3998)) })
	_, err = h.Handle(t.Context(), command.RequestPermissionElevationCommand{TenantID: awTenant, RequesterMembershipID: c.exec.ID(), Permission: permission.FromConstant(permission.IdentityPermissions.Crm.Leads.Read), Reason: "needed for the quarterly close", Actor: imp})
	if !errors.Is(err, authority.ErrImpersonationNotAllowed) {
		t.Fatalf("submit during impersonation: %v, want ErrImpersonationNotAllowed", err)
	}
}

func TestApprove_ManagerNeedsApproveAndTheHeldPermission(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	w := newPRWorld(t)
	c := w.cast(t)
	tl := actorAs(t, c.tl.ID(), authority.RankTeamLead, p.Roles.Approve, p.Crm.Leads.Read)

	// the chain: the Team Lead does not hold identity.roles.update
	chain := w.pending(t, c.exec, p.Roles.Update, 0)
	if err := w.approveAs(tl, chain); !errors.Is(err, authority.ErrGrantExceedsAuthority) {
		t.Fatalf("approve an unheld permission: %v, want ErrGrantExceedsAuthority", err)
	}
	if got, _ := w.reqs.GetByID(t.Context(), awTenant, chain.ID()); got.State() != permissionrequest.StatePending {
		t.Errorf("a refused approval changed the request to %s", got.State())
	}
	if _, ok := grantOf(c.exec, p.Roles.Update); ok {
		t.Fatal("the requester holds identity.roles.update after a refused approval")
	}

	// a held permission, for a report: approved, exactly that permission, the stored duration
	ok := w.pending(t, c.exec, p.Crm.Leads.Read, 14)
	if err := w.approveAs(tl, ok); err != nil {
		t.Fatalf("approve a held permission for a report: %v", err)
	}
	g, found := grantOf(c.exec, p.Crm.Leads.Read)
	if !found || !g.ExpiresAt.Equal(testNow.Add(14*24*time.Hour)) {
		t.Fatalf("grant = %+v (found %v), want leads.read expiring at now+14d", g, found)
	}
	if got, _ := w.reqs.GetByID(t.Context(), awTenant, ok.ID()); got.State() != permissionrequest.StateApproved || got.ApproverMembershipID() != c.tl.ID() {
		t.Errorf("request after approval = %s by %s", got.State(), got.ApproverMembershipID())
	}
	if len(c.exec.GrantedPermissions()) != 1 {
		t.Errorf("approval conferred %d grants, want exactly 1", len(c.exec.GrantedPermissions()))
	}

	// without identity.roles.approve the Team Lead is not an approver at all
	noApprove := actorAs(t, c.tl.ID(), authority.RankTeamLead, p.Crm.Leads.Read)
	again := w.pending(t, c.exec, p.Crm.Leads.ReadAll, 0)
	if err := w.approveAs(noApprove, again); !errors.Is(err, command.ErrPermissionRequestNotFound) {
		t.Errorf("manager without roles.approve: %v, want ErrPermissionRequestNotFound (a caller who may not even deny it sees no request)", err)
	}
}

func TestApprove_WhoMay(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	w := newPRWorld(t)
	c := w.cast(t)
	r := w.pending(t, c.exec, p.Crm.Leads.Read, 0)

	mgrActor := actorAs(t, c.mgr.ID(), authority.RankManager, p.Roles.Approve, p.Crm.Leads.Read)
	if err := w.approveAs(mgrActor, r); !errors.Is(err, command.ErrPermissionRequestNotFound) {
		t.Errorf("a manager who is not the requester's manager: %v, want ErrPermissionRequestNotFound", err)
	}
	peerTL := actorAs(t, w.member(t, w.teamLead).ID(), authority.RankTeamLead, p.Roles.Approve, p.Crm.Leads.Read)
	if err := w.approveAs(peerTL, w.pending(t, c.tl, p.Crm.Leads.Read, 0)); !errors.Is(err, command.ErrPermissionRequestNotFound) {
		t.Errorf("approve a peer's request: %v, want ErrPermissionRequestNotFound", err)
	}
	self := actorAs(t, c.exec.ID(), authority.RankUser, p.Roles.Approve, p.Crm.Leads.Read)
	if err := w.approveAs(self, r); !errors.Is(err, authority.ErrSelfTarget) {
		t.Errorf("self approval: %v, want ErrSelfTarget", err)
	}
	rank := authority.RankOwner.Int()
	imp, _ := authority.NewActor(authority.ActorInput{MembershipID: c.mgr.ID(), TenantID: awTenant, Rank: &rank, IsImpersonation: true, EffectivePermissions: tenantAdminEffective()})
	if err := w.approveAs(imp, r); !errors.Is(err, authority.ErrImpersonationNotAllowed) {
		t.Errorf("approve during impersonation: %v, want ErrImpersonationNotAllowed (about the session, answered before any read)", err)
	}
	// an Administrator without tenant.admin approves day-to-day work, never the authority-and-money set
	admin := actorAs(t, c.admin.ID(), authority.RankAdmin, p.Crm.Leads.Read)
	if err := w.approveAs(admin, r); err != nil {
		t.Errorf("admin approves a day-to-day permission for a user: %v", err)
	}
	if err := w.approveAs(admin, w.pending(t, c.exec, p.Users.Update, 0)); !errors.Is(err, authority.ErrGrantExceedsAuthority) {
		t.Errorf("admin approves a must-hold permission they lack: %v, want ErrGrantExceedsAuthority", err)
	}
	// the owner approves anything requestable
	owner := actorAs(t, w.member(t, w.owner).ID(), authority.RankOwner, tenantAdminEffective()...)
	if err := w.approveAs(owner, w.pending(t, c.other, p.Users.Update, 0)); err != nil {
		t.Errorf("owner approves a must-hold permission: %v", err)
	}
	// a read-only platform operator in the platform tenant follows rank like anyone else
	platRank := authority.RankNone.Int()
	plat, _ := authority.NewActor(authority.ActorInput{MembershipID: w.member(t).ID(), TenantID: awTenant, Rank: &platRank,
		IsPlatformOperator: true, EffectivePermissions: []string{p.Platform.TenantsView}})
	if err := w.approveAs(plat, w.pending(t, c.other, p.Crm.Leads.Read, 0)); !errors.Is(err, command.ErrPermissionRequestNotFound) {
		t.Errorf("read-only operator in its own tenant: %v, want ErrPermissionRequestNotFound", err)
	}
}

func TestApprove_CheckOrderAuthorityBeforeState(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	w := newPRWorld(t)
	c := w.cast(t)
	tl := actorAs(t, c.tl.ID(), authority.RankTeamLead, p.Roles.Approve, p.Crm.Leads.Read)
	stranger := actorAs(t, c.other.ID(), authority.RankUser, p.Roles.Approve)

	r := w.pending(t, c.exec, p.Crm.Leads.Read, 0)
	if err := w.denyAs(tl, r); err != nil {
		t.Fatalf("deny: %v", err)
	}
	// non-pending: the eligible approver learns 409; a stranger learns only 403 (no state oracle)
	if err := w.approveAs(tl, r); !errors.Is(err, command.ErrPermissionRequestNotPending) {
		t.Errorf("eligible approver, decided request: %v, want ErrPermissionRequestNotPending", err)
	}
	if err := w.approveAs(stranger, r); !errors.Is(err, command.ErrPermissionRequestNotFound) {
		t.Errorf("stranger, decided request: %v, want ErrPermissionRequestNotFound", err)
	}
	if err := w.denyAs(stranger, r); errors.Is(err, command.ErrPermissionRequestNotPending) {
		t.Errorf("stranger denies a decided request and learns its state: %v", err)
	}
	// a stored non-requestable row: the eligible approver gets 422 and the row stays pending; a stranger 403
	old := w.pendingForged(t, c.exec.ID(), p.Meta.TenantAdmin)
	if err := w.approveAs(tl, old); !errors.Is(err, permissionrequest.ErrPermissionNotRequestable) {
		t.Errorf("stored tenant.admin request, eligible approver: %v, want ErrPermissionNotRequestable", err)
	}
	if err := w.approveAs(stranger, old); errors.Is(err, permissionrequest.ErrPermissionNotRequestable) {
		t.Errorf("stranger learns the stored permission name: %v", err)
	}
	if got, _ := w.reqs.GetByID(t.Context(), awTenant, old.ID()); got.State() != permissionrequest.StatePending {
		t.Errorf("stored non-requestable request is %s, want pending", got.State())
	}
	if err := w.denyAs(tl, old); err != nil {
		t.Errorf("the approver can still deny it: %v", err)
	}
	// not found
	if err := w.approve.Handle(t.Context(), command.ApprovePermissionRequestCommand{TenantID: awTenant, RequestID: permissionrequest.ID(awID(3500)), Actor: tl}); !errors.Is(err, command.ErrPermissionRequestNotFound) {
		t.Errorf("unknown request: %v, want ErrPermissionRequestNotFound", err)
	}
}

// pendingForged files a pending request that names a permission the aggregate
// would refuse today (an old row from before requestability was enforced).
func (w *prWorld) pendingForged(t *testing.T, requester membership.ID, name string) *permissionrequest.Request {
	t.Helper()
	w.nextReq++
	r := permissionrequest.UnmarshalFromDB(permissionrequest.Snapshot{
		ID: permissionrequest.ID(awID(3000 + w.nextReq)), TenantID: awTenant, RequesterMembershipID: requester,
		Permission: permission.FromConstant(name), DurationDays: 7, Reason: "needed for the quarterly close",
		State: permissionrequest.StatePending, CreatedAt: testNow, UpdatedAt: testNow,
	})
	if err := w.reqs.Add(t.Context(), r); err != nil {
		t.Fatalf("add request: %v", err)
	}
	return r
}

func TestApprove_RequesterNoLongerActiveIsAConflict(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	w := newPRWorld(t)
	c := w.cast(t)
	tl := actorAs(t, c.tl.ID(), authority.RankTeamLead, p.Roles.Approve, p.Crm.Leads.Read)
	r := w.pending(t, c.exec, p.Crm.Leads.Read, 0)
	if err := w.members.UpdateByID(t.Context(), awTenant, c.exec.ID(), func(m *membership.Membership) (bool, error) {
		return true, m.Deactivate("left the company", testNow)
	}); err != nil {
		t.Fatal(err)
	}
	if err := w.approveAs(tl, r); !errors.Is(err, command.ErrPermissionRequestRequesterInactive) {
		t.Fatalf("approve for a deactivated requester: %v, want ErrPermissionRequestRequesterInactive", err)
	}
	if got, _ := w.reqs.GetByID(t.Context(), awTenant, r.ID()); got.State() != permissionrequest.StatePending {
		t.Errorf("request is %s, want pending", got.State())
	}
	if len(c.exec.GrantedPermissions()) != 0 {
		t.Error("a grant was written for a deactivated requester")
	}
	if err := w.denyAs(tl, r); err != nil {
		t.Errorf("deny for a deactivated requester: %v", err)
	}
	// the requester's membership gone entirely: not found
	gone := w.pendingForged(t, membership.ID(awID(4444)), p.Crm.Leads.Read)
	if err := w.approveAs(tl, gone); !errors.Is(err, command.ErrPermissionRequestNotFound) {
		t.Errorf("requester membership missing: %v, want ErrPermissionRequestNotFound", err)
	}
}

func TestApprove_KeepsExistingOverridesAndNeverShortensAGrant(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	w := newPRWorld(t)
	// The approver holds, by override, what they approve: the policy decides on the
	// permissions resolved from the stored facts, not on the token's claim alone.
	tl := w.memberSnap(t, membership.Snapshot{RoleAssignments: []role.ID{w.teamLead.ID()}, GrantedPermissions: []membership.GrantedOverride{
		{Permission: permission.FromConstant(p.Crm.Leads.ReadAll)}, {Permission: permission.FromConstant(p.Crm.Leads.Assign)}}})
	permanent := permission.FromConstant(p.Crm.Leads.ReadAll)
	timed := permission.FromConstant(p.Crm.Leads.Assign)
	revoked := permission.FromConstant(p.Crm.Leads.Read)
	exec := w.memberSnap(t, membership.Snapshot{
		RoleAssignments: []role.ID{w.exec.ID()}, ReportsTo: tl.ID(),
		GrantedPermissions: []membership.GrantedOverride{
			{Permission: permanent},
			{Permission: timed, ExpiresAt: testNow.Add(2 * 24 * time.Hour)},
			{Permission: permission.FromConstant(p.Inventory.Stock.Read), ExpiresAt: testNow.Add(-time.Hour)}, // expired: dropped
		},
		RevokedPermissions: []*permission.Permission{revoked, permission.FromConstant(p.Orders.Orders.Read)},
	})
	actor := actorAs(t, tl.ID(), authority.RankTeamLead, p.Roles.Approve, p.Crm.Leads.ReadAll, p.Crm.Leads.Assign, p.Crm.Leads.Read)
	for _, perm := range []string{p.Crm.Leads.ReadAll, p.Crm.Leads.Assign} {
		if err := w.approveAs(actor, w.pending(t, exec, perm, 7)); err != nil {
			t.Fatalf("approve %s: %v", perm, err)
		}
	}
	// A name on the revoked list is an un-revoke: a Team Lead may not lift it
	// even while holding the name (hierarchy round 5); an Administrator may.
	rq := w.pending(t, exec, revoked.Name(), 7)
	if err := w.approveAs(actor, rq); !errors.Is(err, authority.ErrGrantExceedsAuthority) {
		t.Fatalf("Team Lead approves a revoked name: %v, want ErrGrantExceedsAuthority", err)
	}
	adm := w.member(t, w.admin)
	if err := w.approveAs(actorAs(t, adm.ID(), authority.RankAdmin, tenantAdminEffective()...), rq); err != nil {
		t.Fatalf("Administrator approves the revoked name: %v", err)
	}
	if g, _ := grantOf(exec, permanent.Name()); !g.ExpiresAt.IsZero() {
		t.Errorf("a permanent grant became time-bound: %v", g.ExpiresAt)
	}
	if g, _ := grantOf(exec, timed.Name()); !g.ExpiresAt.Equal(testNow.Add(7 * 24 * time.Hour)) {
		t.Errorf("a 2-day grant approved for 7 days expires at %v, want now+7d", g.ExpiresAt)
	}
	// Someone else's approval leaves the expired row exactly as it was (history is not lost).
	if g, ok := grantOf(exec, p.Inventory.Stock.Read); !ok || !g.ExpiresAt.Equal(testNow.Add(-time.Hour)) {
		t.Errorf("the expired grant after an approval = %+v (found %v), want it untouched", g, ok)
	}
	if g, ok := grantOf(exec, revoked.Name()); !ok || g.ExpiresAt.IsZero() {
		t.Errorf("approved leads.read = %+v (%v), want a timed grant", g, ok)
	}
	if got := exec.RevokedPermissions(); len(got) != 1 || got[0].Name() != p.Orders.Orders.Read {
		t.Errorf("revoked after approval = %v, want only orders.orders.read (the approved name leaves the list)", got)
	}
}

func TestDeny_Authority(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	w := newPRWorld(t)
	c := w.cast(t)
	r := w.pending(t, c.exec, p.Roles.Update, 0)
	// denying needs eligibility only, not the permission
	tl := actorAs(t, c.tl.ID(), authority.RankTeamLead, p.Roles.Approve)
	mgr := actorAs(t, c.mgr.ID(), authority.RankManager, p.Roles.Approve)
	if err := w.denyAs(mgr, r); !errors.Is(err, command.ErrPermissionRequestNotFound) {
		t.Errorf("deny by a non-manager manager: %v, want ErrPermissionRequestNotFound", err)
	}
	if err := w.denyAs(tl, r); err != nil {
		t.Fatalf("deny by the manager: %v", err)
	}
	if err := w.denyAs(tl, r); !errors.Is(err, command.ErrPermissionRequestNotPending) {
		t.Errorf("deny twice: %v, want ErrPermissionRequestNotPending", err)
	}
	self := actorAs(t, c.exec.ID(), authority.RankUser, p.Roles.Approve)
	if err := w.denyAs(self, w.pending(t, c.exec, p.Crm.Leads.Read, 0)); !errors.Is(err, authority.ErrSelfTarget) {
		t.Errorf("self deny: %v, want ErrSelfTarget", err)
	}
}

func TestPermissionRequestVisibilityAndQueue(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	w := newPRWorld(t)
	c := w.cast(t)
	tlOther := w.member(t, w.teamLead)
	execOfOther := w.memberReporting(t, tlOther.ID(), w.exec)
	mine := w.pending(t, c.exec, p.Crm.Leads.Read, 0)
	theirs := w.pending(t, execOfOther, p.Crm.Leads.Read, 0)
	ownTL := w.pending(t, c.tl, p.Crm.Leads.Read, 0)

	tl := actorAs(t, c.tl.ID(), authority.RankTeamLead, p.Roles.Approve)
	exec := actorAs(t, c.exec.ID(), authority.RankUser)
	admin := actorAs(t, c.admin.ID(), authority.RankAdmin)
	other := actorAs(t, c.other.ID(), authority.RankUser, p.Roles.Approve)
	canSee := func(a authority.Actor, r *permissionrequest.Request) bool {
		err := w.view.Handle(t.Context(), command.AuthorizePermissionRequestViewCommand{TenantID: awTenant, RequestID: r.ID(), Actor: a})
		if err != nil && !errors.Is(err, command.ErrPermissionRequestNotFound) {
			t.Fatalf("view: %v", err)
		}
		return err == nil
	}
	for name, want := range map[string]struct {
		a   authority.Actor
		r   *permissionrequest.Request
		see bool
	}{
		"requester sees their own":           {exec, mine, true},
		"the manager sees a report's":        {tl, mine, true},
		"an Administrator sees it":           {admin, mine, true},
		"a peer manager does not":            {actorAs(t, tlOther.ID(), authority.RankTeamLead, p.Roles.Approve), mine, false},
		"an unrelated executive does not":    {other, mine, false},
		"the manager does not see theirs":    {tl, theirs, false},
		"a manager does not see their own's": {tl, ownTL, true}, // own request: the requester
	} {
		if got := canSee(want.a, want.r); got != want.see {
			t.Errorf("%s: visible=%v, want %v", name, got, want.see)
		}
	}
	if err := w.view.Handle(t.Context(), command.AuthorizePermissionRequestViewCommand{TenantID: awTenant, RequestID: permissionrequest.ID(awID(3777)), Actor: admin}); !errors.Is(err, command.ErrPermissionRequestNotFound) {
		t.Errorf("missing id: %v, want not found (same answer as a hidden request)", err)
	}

	queueOf := func(a authority.Actor) map[permissionrequest.ID]bool {
		res, err := w.queue.Handle(t.Context(), command.ListDecidablePermissionRequestsQuery{TenantID: awTenant, Actor: a})
		if err != nil {
			t.Fatalf("queue: %v", err)
		}
		out := map[permissionrequest.ID]bool{}
		for _, it := range res.Page.Items {
			out[it.Request.ID()] = true
		}
		return out
	}
	if q := queueOf(tl); len(q) != 1 || !q[mine.ID()] {
		t.Errorf("Team Lead's queue = %v, want only their report's request", q)
	}
	if q := queueOf(admin); len(q) != 3 {
		t.Errorf("Administrator's queue holds %d requests, want all 3 (outranks every requester)", len(q))
	}
	if q := queueOf(exec); len(q) != 0 {
		t.Errorf("an executive's queue = %v, want empty", q)
	}
}

// countingMembers counts membership loads, to prove the queue loads each
// distinct requester once however many requests that requester filed.
type countingMembers struct {
	*membershiptest.FakeRepository
	gets    int
	batches int
	batched int
}

func (c *countingMembers) GetByIDs(ctx context.Context, tid tenant.ID, ids []membership.ID) ([]*membership.Membership, error) {
	c.batches++
	c.batched += len(ids)
	return c.FakeRepository.GetByIDs(ctx, tid, ids)
}

func (c *countingMembers) GetByID(ctx context.Context, tid tenant.ID, id membership.ID) (*membership.Membership, error) {
	c.gets++
	return c.FakeRepository.GetByID(ctx, tid, id)
}

func TestApproverQueue_LoadsEachRequesterOnce(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	w := newPRWorld(t)
	c := w.cast(t)
	cm := &countingMembers{FakeRepository: w.members}
	queue := command.NewListDecidablePermissionRequestsHandler(fakeUoW{}, w.reqs, cm, w.roles, anyPersons{}, awClock)
	// one requester, many requests; a second requester with one
	for _, perm := range []string{p.Crm.Leads.Read, p.Crm.Leads.ReadAll, p.Crm.Leads.Assign, p.Inventory.Stock.Read, p.Orders.Orders.Read} {
		w.pending(t, c.exec, perm, 0)
	}
	w.pending(t, c.other, p.Crm.Leads.Read, 0)
	admin := actorAs(t, c.admin.ID(), authority.RankAdmin)
	res, err := queue.Handle(t.Context(), command.ListDecidablePermissionRequestsQuery{TenantID: awTenant, Actor: admin})
	if err != nil || len(res.Page.Items) != 6 {
		t.Fatalf("queue = %d items, %v; want 6", len(res.Page.Items), err)
	}
	if cm.gets != 1 || cm.batches != 1 || cm.batched != 2 {
		t.Errorf("membership reads = %d single + %d batch of %d ids, want the caller once and ONE batch of the 2 distinct requesters", cm.gets, cm.batches, cm.batched)
	}
	if res.Page.HasMore {
		t.Error("HasMore with a window larger than the queue")
	}
}

// ----- approving a name that is on the requester's revoked list ---------------

// A revoked override is a deliberate "this member must not have X". Approving X
// for them is an un-revoke, so it needs the same authority as changing their
// overrides (authority.MayChangeOverrides: act on the member and hold X under
// the ceiling rules) on top of MayApprove.
func TestApprove_UnrevokeNeedsTheAuthorityToChangeOverrides(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	w := newPRWorld(t)
	tl := w.member(t, w.teamLead)
	revokedExec := func() *membership.Membership {
		return w.memberSnap(t, membership.Snapshot{RoleAssignments: []role.ID{w.exec.ID()}, ReportsTo: tl.ID(),
			RevokedPermissions: []*permission.Permission{permission.FromConstant(p.Crm.Leads.Assign)}})
	}
	// An Administrator without tenant.admin approves day-to-day permissions without holding them...
	adminM := w.member(t, w.admin)
	admin := actorAs(t, adminM.ID(), authority.RankAdmin, p.Crm.Leads.Read)
	plain := w.memberSnap(t, membership.Snapshot{RoleAssignments: []role.ID{w.exec.ID()}, ReportsTo: tl.ID()})
	if err := w.approveAs(admin, w.pending(t, plain, p.Crm.Leads.Assign, 0)); err != nil {
		t.Fatalf("control: the Administrator approves a day-to-day permission that is not revoked: %v", err)
	}
	// ...but cannot un-revoke one, because changing overrides needs the permission itself.
	exec := revokedExec()
	r := w.pending(t, exec, p.Crm.Leads.Assign, 0)
	w.rotations = nil
	if err := w.approveAs(admin, r); !isErr(err, authority.ErrGrantExceedsAuthority) {
		t.Fatalf("an approver who does not hold the name un-revokes it: %v, want ErrGrantExceedsAuthority", err)
	}
	if got, _ := w.reqs.GetByID(t.Context(), awTenant, r.ID()); got.State() != permissionrequest.StatePending {
		t.Errorf("the refused approval left the request %s, want pending", got.State())
	}
	if len(exec.RevokedPermissions()) != 1 || len(exec.GrantedPermissions()) != 0 || len(w.rotations) != 0 {
		t.Errorf("the refused approval changed state: revoked %v, granted %v, rotations %v", exec.RevokedPermissions(), exec.GrantedPermissions(), w.rotations)
	}
	// The owner holds everything: the approval un-revokes.
	owner := actorAs(t, w.member(t, w.owner).ID(), authority.RankOwner, tenantAdminEffective()...)
	if err := w.approveAs(owner, r); err != nil {
		t.Fatalf("owner approves the un-revoke: %v", err)
	}
	if len(exec.RevokedPermissions()) != 0 {
		t.Errorf("the revoke is still there after the owner's approval: %v", exec.RevokedPermissions())
	}
	if _, ok := grantOf(exec, p.Crm.Leads.Assign); !ok {
		t.Error("the approved permission was not granted")
	}
}
