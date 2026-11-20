package command_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership/membershiptest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role/roletest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

const awTenant = tenant.ID("33333333-3333-3333-3333-333333333333")

func awID(n int) string  { return fmt.Sprintf("0190b000-0000-7000-8000-%012d", n) }
func awClock() time.Time { return testNow }

type fakeAuthorityLock struct{ calls int }

func (l *fakeAuthorityLock) LockTenantAuthority(_ context.Context, _ tenant.ID) error {
	l.calls++
	return nil
}

// anyPersons answers GetByID with an active, unlocked person for any id: the
// command tests that do not care about person state need not seed one.
type anyPersons struct {
	person.Repository
	// rotations, when set, collects the reason of every stamp rotation applied through UpdateByID.
	rotations *[]string
}

// UpdateByID runs fn on a synthesised person (nothing is persisted) and
// records the reasons of the stamp rotations it caused.
func (a anyPersons) UpdateByID(ctx context.Context, id person.ID, fn func(*person.Person) (bool, error)) error {
	p, err := a.GetByID(ctx, id)
	if err != nil {
		return err
	}
	ok, err := fn(p)
	if err != nil || !ok {
		return err
	}
	for _, e := range p.PullEvents() {
		if ev, isRotation := e.(person.SecurityStampRotatedEvent); isRotation && a.rotations != nil {
			*a.rotations = append(*a.rotations, ev.Reason)
		}
	}
	return nil
}

// GetByIDs synthesises every requested person, like GetByID.
func (a anyPersons) GetByIDs(ctx context.Context, ids []person.ID) (map[person.ID]*person.Person, error) {
	out := make(map[person.ID]*person.Person, len(ids))
	for _, id := range ids {
		p, err := a.GetByID(ctx, id)
		if err != nil {
			return nil, err
		}
		out[id] = p
	}
	return out, nil
}

func (anyPersons) GetByID(_ context.Context, id person.ID) (*person.Person, error) {
	addr, err := email.New(strings.ReplaceAll(string(id), "-", "") + "@authz.test")
	if err != nil {
		return nil, err
	}
	hash, err := person.NewPasswordHash("$argon2id$v=19$m=65536,t=3,p=4$YWJjZGVmZ2g$ZS1zdHViLWhhc2gtdGVzdC1maXh0dXJlLW9ubHk")
	if err != nil {
		return nil, err
	}
	return person.New(id, addr, "A", "B", hash, testNow)
}

// authzScopeUoW records the tenant the transaction would be bound to.
type authzScopeUoW struct{ tenants *[]string }

func (u authzScopeUoW) WithinTx(ctx context.Context, _ pg.TxScope, fn func(context.Context) error) error {
	id, _ := tenancy.FromContext(ctx)
	*u.tenants = append(*u.tenants, id.String())
	return fn(ctx)
}

type authzWorld struct {
	members *membershiptest.FakeRepository
	// rotations collects the reason of every stamp rotation the handlers applied.
	rotations                             []string
	roles                                 *roletest.FakeRepository
	lock                                  *fakeAuthorityLock
	owner, admin, manager, teamLead, exec *role.Role
	next                                  int
}

func newAuthzWorld(t *testing.T) *authzWorld {
	t.Helper()
	p := permission.IdentityPermissions
	w := &authzWorld{members: membershiptest.NewFakeRepository(), roles: roletest.NewFakeRepository(), lock: &fakeAuthorityLock{}, next: 100}
	w.owner = w.addRole(t, 1, role.SystemRoles.Tenant.CompanyOwner, true, role.HierarchyLevelOwner, p.Meta.TenantAdmin)
	w.admin = w.addRole(t, 2, role.SystemRoles.Tenant.Administrator, false, role.HierarchyLevelAdmin, p.Meta.TenantAdmin)
	w.manager = w.addRole(t, 3, role.SystemRoles.Tenant.SalesManager, false, role.HierarchyLevelManager, p.Crm.Leads.Read, p.Crm.Leads.ReadAll, p.Roles.Approve)
	w.teamLead = w.addRole(t, 4, role.SystemRoles.Tenant.TeamLead, false, role.HierarchyLevelTeamLead, p.Crm.Leads.Read, p.Roles.Approve)
	w.exec = w.addRole(t, 5, role.SystemRoles.Tenant.SalesExecutive, false, role.HierarchyLevelDefault, p.Crm.Leads.Read)
	return w
}

func (w *authzWorld) addRole(t *testing.T, n int, name string, sys bool, level int, perms ...string) *role.Role {
	t.Helper()
	return w.addRoleSnap(t, role.Snapshot{ID: role.ID(awID(n)), TenantID: awTenant, Name: name, IsSystemDefault: sys, HierarchyLevel: level, CreatedAt: testNow}, perms...)
}

func (w *authzWorld) addRoleSnap(t *testing.T, s role.Snapshot, perms ...string) *role.Role {
	t.Helper()
	for _, name := range perms {
		s.Permissions = append(s.Permissions, permission.FromConstant(name))
	}
	r := role.UnmarshalFromDB(s)
	if err := w.roles.Add(t.Context(), r); err != nil {
		t.Fatalf("add role %s: %v", s.Name, err)
	}
	return r
}

func (w *authzWorld) memberSnap(t *testing.T, s membership.Snapshot) *membership.Membership {
	t.Helper()
	w.next++
	s.ID, s.PersonID, s.TenantID, s.JoinedAt = membership.ID(awID(w.next)), person.ID(awID(w.next+5000)), awTenant, testNow
	if s.Status == membership.StatusUnknown {
		s.Status = membership.StatusActive
	}
	m := membership.UnmarshalFromDB(s)
	if err := w.members.Add(t.Context(), m); err != nil {
		t.Fatalf("add member: %v", err)
	}
	return m
}

func roleIDs(roles []*role.Role) []role.ID {
	out := make([]role.ID, 0, len(roles))
	for _, r := range roles {
		out = append(out, r.ID())
	}
	return out
}

func (w *authzWorld) memberReporting(t *testing.T, reportsTo membership.ID, roles ...*role.Role) *membership.Membership {
	t.Helper()
	return w.memberSnap(t, membership.Snapshot{RoleAssignments: roleIDs(roles), ReportsTo: reportsTo})
}

func (w *authzWorld) member(t *testing.T, roles ...*role.Role) *membership.Membership {
	t.Helper()
	return w.memberReporting(t, "", roles...)
}

func tenantAdminEffective() []string {
	out := []string{permission.IdentityPermissions.Meta.TenantAdmin}
	for _, p := range permission.All() {
		if permission.ImpliedByTenantAdmin(p.Name()) {
			out = append(out, p.Name())
		}
	}
	return out
}

func actorAs(t *testing.T, id membership.ID, rank authority.Rank, perms ...string) authority.Actor {
	t.Helper()
	r := rank.Int()
	a, err := authority.NewActor(authority.ActorInput{MembershipID: id, TenantID: awTenant, Rank: &r, EffectivePermissions: perms})
	if err != nil {
		t.Fatalf("NewActor: %v", err)
	}
	return a
}

func superActor(t *testing.T) authority.Actor {
	t.Helper()
	r := authority.RankOwner.Int()
	a, err := authority.NewActor(authority.ActorInput{MembershipID: membership.ID(awID(9999)), TenantID: tenant.ID(awID(9998)), Rank: &r, IsSuperUser: true})
	if err != nil {
		t.Fatalf("NewActor: %v", err)
	}
	return a
}

// testAdminActor is a rank-Admin actor for tests whose targets hold no role.
func testAdminActor(t *testing.T) authority.Actor {
	t.Helper()
	return actorAs(t, membership.ID(awID(9997)), authority.RankAdmin, tenantAdminEffective()...)
}

type handlers struct {
	assign   command.AssignUserRoleHandler
	revoke   command.RevokeUserRoleHandler
	override command.ReplaceUserPermissionOverridesHandler
	setMgr   command.AssignUserManagerHandler
	clearMgr command.RemoveUserManagerHandler
	deact    command.DeactivateUserHandler
	react    command.ReactivateUserHandler
}

func (w *authzWorld) handlersAt(now func() time.Time) handlers {
	l := command.AuthorityLocker(w.lock)
	p := anyPersons{rotations: &w.rotations}
	return handlers{
		assign:   command.NewAssignUserRoleHandler(fakeUoW{}, l, w.members, w.roles, p, now),
		revoke:   command.NewRevokeUserRoleHandler(fakeUoW{}, l, w.members, w.roles, p, now),
		override: command.NewReplaceUserPermissionOverridesHandler(fakeUoW{}, l, w.members, w.roles, p, now),
		setMgr:   command.NewAssignUserManagerHandler(fakeUoW{}, l, w.members, w.roles, p, now),
		clearMgr: command.NewRemoveUserManagerHandler(fakeUoW{}, l, w.members, w.roles, p, now),
		deact:    command.NewDeactivateUserHandler(fakeUoW{}, l, w.members, w.roles, p, now),
		react:    command.NewReactivateUserHandler(fakeUoW{}, l, w.members, w.roles, p, now),
	}
}

func (w *authzWorld) handlers() handlers { return w.handlersAt(awClock) }

func TestAuthorityCommands_ZeroActorDeniedBeforeLock(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	target := w.member(t)
	h := w.handlers()
	ctx := t.Context()
	cases := map[string]error{
		"assign":        h.assign.Handle(ctx, command.AssignUserRoleCommand{TenantID: awTenant, MembershipID: target.ID(), RoleID: w.exec.ID()}),
		"revoke":        h.revoke.Handle(ctx, command.RevokeUserRoleCommand{TenantID: awTenant, MembershipID: target.ID(), RoleID: w.exec.ID()}),
		"overrides":     h.override.Handle(ctx, command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: target.ID()}),
		"set manager":   h.setMgr.Handle(ctx, command.AssignUserManagerCommand{TenantID: awTenant, MembershipID: target.ID(), ManagerID: membership.ID(awID(900))}),
		"clear manager": h.clearMgr.Handle(ctx, command.RemoveUserManagerCommand{TenantID: awTenant, MembershipID: target.ID()}),
		"deactivate":    h.deact.Handle(ctx, command.DeactivateUserCommand{TenantID: awTenant, MembershipID: target.ID(), Reason: "x"}),
		"reactivate":    h.react.Handle(ctx, command.ReactivateUserCommand{TenantID: awTenant, MembershipID: target.ID()}),
	}
	for name, err := range cases {
		if !errors.Is(err, authority.ErrNoAuthority) {
			t.Errorf("%s: err=%v, want ErrNoAuthority", name, err)
		}
	}
	if w.lock.calls != 0 {
		t.Fatalf("lock taken %d times for a zero actor, want 0", w.lock.calls)
	}
}

func TestAuthorityCommands_TransactionIsBoundToTheTargetTenant(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	target := w.member(t)
	var bound []string
	h := command.NewAssignUserRoleHandler(authzScopeUoW{tenants: &bound}, w.lock, w.members, w.roles, anyPersons{}, awClock)
	// the request context carries ANOTHER tenant (an operator's home tenant)
	ctx := tenancy.WithID(t.Context(), tenancy.ID(awID(4242)))
	if err := h.Handle(ctx, command.AssignUserRoleCommand{TenantID: awTenant, MembershipID: target.ID(), RoleID: w.exec.ID(), Actor: w.adminActor(t)}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(bound) != 1 || bound[0] != awTenant.String() {
		t.Fatalf("transaction bound to %v, want [%s]", bound, awTenant)
	}
}

func TestAssignUserRole_RankAndCeiling(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	adminM := w.member(t, w.admin)
	target := w.member(t)
	h := w.handlers().assign
	admin := actorAs(t, adminM.ID(), authority.RankAdmin, tenantAdminEffective()...)
	if err := h.Handle(t.Context(), command.AssignUserRoleCommand{TenantID: awTenant, MembershipID: target.ID(), RoleID: w.manager.ID(), Actor: admin}); err != nil {
		t.Fatalf("admin assigns manager role: %v", err)
	}
	if w.lock.calls != 1 {
		t.Fatalf("lock calls = %d, want 1", w.lock.calls)
	}
	if err := h.Handle(t.Context(), command.AssignUserRoleCommand{TenantID: awTenant, MembershipID: target.ID(), RoleID: w.admin.ID(), Actor: admin}); !errors.Is(err, authority.ErrInsufficientRank) {
		t.Fatalf("admin assigns admin role: %v, want ErrInsufficientRank", err)
	}
	if err := h.Handle(t.Context(), command.AssignUserRoleCommand{TenantID: awTenant, MembershipID: target.ID(), RoleID: w.owner.ID(), Actor: admin}); !errors.Is(err, authority.ErrInsufficientRank) {
		t.Fatalf("admin assigns Company Owner: %v, want ErrInsufficientRank", err)
	}
	router := w.addRole(t, 6, "Lead Router", false, 40, permission.IdentityPermissions.Crm.Leads.Assign)
	mgrM := w.member(t, w.manager)
	mgr := actorAs(t, mgrM.ID(), authority.RankManager, permission.IdentityPermissions.Crm.Leads.Read)
	exec := w.member(t)
	if err := h.Handle(t.Context(), command.AssignUserRoleCommand{TenantID: awTenant, MembershipID: exec.ID(), RoleID: router.ID(), Actor: mgr}); !errors.Is(err, authority.ErrGrantExceedsAuthority) {
		t.Fatalf("manager confers unheld via role: %v", err)
	}
	if err := h.Handle(t.Context(), command.AssignUserRoleCommand{TenantID: awTenant, MembershipID: exec.ID(), RoleID: role.ID(awID(777)), Actor: admin}); !errors.Is(err, command.ErrRoleNotFound) {
		t.Fatalf("unknown role: %v", err)
	}
	gone := w.addRoleSnap(t, role.Snapshot{ID: role.ID(awID(778)), TenantID: awTenant, Name: "Gone", HierarchyLevel: 60, CreatedAt: testNow, IsDeleted: true})
	if err := h.Handle(t.Context(), command.AssignUserRoleCommand{TenantID: awTenant, MembershipID: exec.ID(), RoleID: gone.ID(), Actor: admin}); !errors.Is(err, command.ErrRoleNotFound) {
		t.Fatalf("soft-deleted role: %v, want ErrRoleNotFound", err)
	}
}

func TestAssignUserRole_SelfAndOperator(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	adminM := w.member(t, w.admin)
	h := w.handlers().assign
	self := actorAs(t, adminM.ID(), authority.RankAdmin, tenantAdminEffective()...)
	if err := h.Handle(t.Context(), command.AssignUserRoleCommand{TenantID: awTenant, MembershipID: adminM.ID(), RoleID: w.exec.ID(), Actor: self}); !errors.Is(err, authority.ErrSelfTarget) {
		t.Fatalf("self assign: %v, want ErrSelfTarget", err)
	}
	// an exempt platform operator whose HOME tenant is another one acts here
	rank := authority.RankOwner.Int()
	op, err := authority.NewActor(authority.ActorInput{MembershipID: membership.ID(awID(8001)), TenantID: tenant.ID(awID(8002)), Rank: &rank,
		EffectivePermissions: []string{permission.IdentityPermissions.Platform.UsersManage}, IsPlatformOperator: true})
	if err != nil {
		t.Fatal(err)
	}
	target := w.member(t)
	if err := h.Handle(t.Context(), command.AssignUserRoleCommand{TenantID: awTenant, MembershipID: target.ID(), RoleID: w.exec.ID(), Actor: op}); err != nil {
		t.Fatalf("exempt operator in another tenant: %v", err)
	}
}

func TestRevokeUserRole_RankAndLastOwner(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	owner1 := w.member(t, w.owner)
	h := w.handlers().revoke
	cmd := command.RevokeUserRoleCommand{TenantID: awTenant, MembershipID: owner1.ID(), RoleID: w.owner.ID(), Actor: superActor(t)}
	if err := h.Handle(t.Context(), cmd); !errors.Is(err, authority.ErrLastOwnerProtected) {
		t.Fatalf("last owner: %v", err)
	}
	owner2 := w.member(t, w.owner)
	adminM := w.member(t, w.admin)
	cmd.Actor = actorAs(t, adminM.ID(), authority.RankAdmin, tenantAdminEffective()...)
	if err := h.Handle(t.Context(), cmd); !errors.Is(err, authority.ErrInsufficientRank) {
		t.Fatalf("admin revokes owner role: %v, want ErrInsufficientRank", err)
	}
	cmd.Actor = superActor(t)
	if err := h.Handle(t.Context(), cmd); err != nil {
		t.Fatalf("with a second owner: %v", err)
	}
	// owner2 is now the last one again
	cmd.MembershipID = owner2.ID()
	if err := h.Handle(t.Context(), cmd); !errors.Is(err, authority.ErrLastOwnerProtected) {
		t.Fatalf("second revoke: %v, want ErrLastOwnerProtected", err)
	}
}

func TestRevokeUserRole_DeletedRoleIsRevokedOnRankAlone(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	gone := w.addRoleSnap(t, role.Snapshot{ID: role.ID(awID(779)), TenantID: awTenant, Name: "Gone", HierarchyLevel: 60, CreatedAt: testNow, IsDeleted: true},
		permission.IdentityPermissions.Crm.Leads.Assign)
	holder := w.member(t, w.exec, gone) // a soft-deleted role in the assigned set must still load
	h := w.handlers().revoke
	mgrM := w.member(t, w.manager)
	mgr := actorAs(t, mgrM.ID(), authority.RankManager) // holds nothing: no ceiling applies to a deleted role
	if err := h.Handle(t.Context(), command.RevokeUserRoleCommand{TenantID: awTenant, MembershipID: holder.ID(), RoleID: gone.ID(), Actor: mgr}); err != nil {
		t.Fatalf("revoke deleted role: %v", err)
	}
	if got := holder.RoleAssignments(); len(got) != 1 || got[0] != w.exec.ID() {
		t.Fatalf("assignments = %v, want only the exec role", got)
	}
}

func TestAuthorityCommands_DanglingRoleAssignmentIsInconsistentNotInternal(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	dangling := w.memberSnap(t, membership.Snapshot{RoleAssignments: []role.ID{role.ID(awID(31337))}})
	adminM := w.member(t, w.admin)
	admin := actorAs(t, adminM.ID(), authority.RankAdmin, tenantAdminEffective()...)
	h := w.handlers()
	err := h.deact.Handle(t.Context(), command.DeactivateUserCommand{TenantID: awTenant, MembershipID: dangling.ID(), Reason: "x", Actor: admin})
	if !errors.Is(err, command.ErrMemberRolesInconsistent) || errors.Is(err, authority.ErrNoAuthority) {
		t.Fatalf("err = %v, want ErrMemberRolesInconsistent only", err)
	}
	if dangling.Status() != membership.StatusActive {
		t.Fatal("a member with unloadable roles was changed")
	}
}

func TestDeactivateUser_RankAndLastOwner(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	owner1 := w.member(t, w.owner)
	h := w.handlers().deact
	err := h.Handle(t.Context(), command.DeactivateUserCommand{TenantID: awTenant, MembershipID: owner1.ID(), Reason: "offboarding test", Actor: superActor(t)})
	if !errors.Is(err, authority.ErrLastOwnerProtected) {
		t.Fatalf("err = %v", err)
	}
	owner2 := w.member(t, w.owner)
	adminM := w.member(t, w.admin)
	admin := actorAs(t, adminM.ID(), authority.RankAdmin, tenantAdminEffective()...)
	if err := h.Handle(t.Context(), command.DeactivateUserCommand{TenantID: awTenant, MembershipID: owner2.ID(), Reason: "rank test", Actor: admin}); !errors.Is(err, authority.ErrInsufficientRank) {
		t.Fatalf("admin deactivates owner: %v, want ErrInsufficientRank", err)
	}
	ownerActor := actorAs(t, owner1.ID(), authority.RankOwner, tenantAdminEffective()...)
	if err := h.Handle(t.Context(), command.DeactivateUserCommand{TenantID: awTenant, MembershipID: owner2.ID(), Reason: "owner removes owner", Actor: ownerActor}); err != nil {
		t.Fatalf("owner deactivates the other owner: %v", err)
	}
	if err := h.Handle(t.Context(), command.DeactivateUserCommand{TenantID: awTenant, MembershipID: owner1.ID(), Reason: "last", Actor: superActor(t)}); !errors.Is(err, authority.ErrLastOwnerProtected) {
		t.Fatalf("owner1 is the last active owner again: %v", err)
	}
}

func TestReactivateUser_Rank(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	owner := w.memberSnap(t, membership.Snapshot{RoleAssignments: []role.ID{w.owner.ID()}, Status: membership.StatusInactive})
	adminM := w.member(t, w.admin)
	admin := actorAs(t, adminM.ID(), authority.RankAdmin, tenantAdminEffective()...)
	h := w.handlers().react
	if err := h.Handle(t.Context(), command.ReactivateUserCommand{TenantID: awTenant, MembershipID: owner.ID(), Actor: admin}); !errors.Is(err, authority.ErrInsufficientRank) {
		t.Fatalf("admin reactivates owner: %v", err)
	}
	if err := h.Handle(t.Context(), command.ReactivateUserCommand{TenantID: awTenant, MembershipID: owner.ID(), Actor: superActor(t)}); err != nil {
		t.Fatalf("super reactivates owner: %v", err)
	}
}

func TestReplaceOverrides_OwnerProtectedAndDeltaOnly(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	p := permission.IdentityPermissions
	owner1, owner2 := w.member(t, w.owner), w.member(t, w.owner)
	h := w.handlers().override
	ownerActor := actorAs(t, owner1.ID(), authority.RankOwner, tenantAdminEffective()...)
	if err := h.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: owner2.ID(),
		RevokedNames: []string{p.Meta.TenantAdmin}, Actor: ownerActor}); !errors.Is(err, authority.ErrOwnerProtected) {
		t.Fatalf("strip owner: %v", err)
	}
	adminM := w.member(t, w.admin)
	admin := actorAs(t, adminM.ID(), authority.RankAdmin, tenantAdminEffective()...)
	if err := h.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: owner2.ID(),
		RevokedNames: []string{p.Crm.Leads.Read}, Actor: admin}); !errors.Is(err, authority.ErrInsufficientRank) {
		t.Fatalf("admin changes owner overrides: %v, want ErrInsufficientRank", err)
	}
	mgrM := w.member(t, w.manager)
	exec := w.member(t, w.exec)
	if err := w.members.UpdateByID(t.Context(), awTenant, exec.ID(), func(m *membership.Membership) (bool, error) {
		return true, m.GrantPermission(permission.FromConstant(p.Crm.Leads.Assign), time.Time{}, testNow)
	}); err != nil {
		t.Fatalf("setup grant: %v", err)
	}
	mgr := actorAs(t, mgrM.ID(), authority.RankManager, p.Crm.Leads.Read, p.Crm.Leads.ReadAll, p.Roles.Approve)
	if err := h.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: exec.ID(),
		GrantedNames: []string{p.Crm.Leads.Assign, p.Crm.Leads.ReadAll}, Actor: mgr}); err != nil {
		t.Fatalf("unchanged unheld grant must not be checked: %v", err)
	}
	if err := h.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: exec.ID(),
		GrantedNames: []string{p.Crm.Leads.ReadAll}, Actor: mgr}); !errors.Is(err, authority.ErrGrantExceedsAuthority) {
		t.Fatalf("removing an unheld grant: %v", err)
	}
}

// ONE clock: the policy snapshot and the aggregate write use the same
// instant, and the aggregate writes exactly the expiries the policy approved.
func TestReplaceOverrides_OneClockReading(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	x := p.Crm.Leads.Assign
	setup := func(t *testing.T, expires time.Time) (*authzWorld, *membership.Membership, authority.Actor) {
		t.Helper()
		w := newAuthzWorld(t)
		exec := w.memberSnap(t, membership.Snapshot{RoleAssignments: []role.ID{w.exec.ID()},
			GrantedPermissions: []membership.GrantedOverride{{Permission: permission.FromConstant(x), ExpiresAt: expires}}})
		mgrM := w.member(t, w.manager)
		return w, exec, actorAs(t, mgrM.ID(), authority.RankManager, p.Crm.Leads.Read) // does not hold X
	}
	t.Run("grant expiring between two readings keeps its expiry (one reading taken)", func(t *testing.T) {
		t.Parallel()
		expires := testNow.Add(time.Hour)
		w, exec, mgr := setup(t, expires)
		calls := 0
		clock := func() time.Time { // a second reading would land after the expiry
			calls++
			return testNow.Add(time.Duration(calls-1) * 2 * time.Hour)
		}
		h := w.handlersAt(clock).override
		if err := h.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: exec.ID(), GrantedNames: []string{x}, Actor: mgr}); err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if calls != 1 {
			t.Fatalf("clock read %d times, want exactly 1", calls)
		}
		got := exec.GrantedPermissions()
		if len(got) != 1 || !got[0].ExpiresAt.Equal(expires) {
			t.Fatalf("grants after replace = %+v, want X with its original expiry %v (never permanent)", got, expires)
		}
	})
	t.Run("grant expiring exactly now is expired: listing it again is a new grant", func(t *testing.T) {
		t.Parallel()
		w, exec, mgr := setup(t, testNow)
		h := w.handlers().override
		err := h.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: exec.ID(), GrantedNames: []string{x}, Actor: mgr})
		if !errors.Is(err, authority.ErrGrantExceedsAuthority) {
			t.Fatalf("err = %v, want ErrGrantExceedsAuthority", err)
		}
	})
	t.Run("a zero clock reading decides nothing", func(t *testing.T) {
		t.Parallel()
		w, exec, mgr := setup(t, testNow.Add(time.Hour))
		h := w.handlersAt(func() time.Time { return time.Time{} }).override
		err := h.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: exec.ID(), GrantedNames: []string{x}, Actor: mgr})
		if !errors.Is(err, authority.ErrNoAuthority) {
			t.Fatalf("err = %v, want ErrNoAuthority", err)
		}
		if got := exec.GrantedPermissions(); len(got) != 1 || got[0].ExpiresAt.IsZero() {
			t.Fatalf("grants changed: %+v", got)
		}
	})
}

func TestAssignUserManager_Rules(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	adminM := w.member(t, w.admin)
	admin := actorAs(t, adminM.ID(), authority.RankAdmin, tenantAdminEffective()...)
	mgr1, mgr2 := w.member(t, w.manager), w.member(t, w.manager)
	tlM := w.member(t, w.teamLead)
	exec := w.member(t, w.exec)
	h := w.handlers().setMgr
	run := func(a authority.Actor, sub, mgr membership.ID) error {
		return h.Handle(t.Context(), command.AssignUserManagerCommand{TenantID: awTenant, MembershipID: sub, ManagerID: mgr, Actor: a})
	}
	if err := run(admin, exec.ID(), mgr1.ID()); err != nil {
		t.Fatalf("admin links exec to manager: %v", err)
	}
	if err := run(admin, mgr1.ID(), mgr2.ID()); !errors.Is(err, authority.ErrManagerNotEligible) {
		t.Fatalf("lateral: %v", err)
	}
	if err := run(admin, mgr1.ID(), exec.ID()); !errors.Is(err, authority.ErrManagerNotEligible) {
		t.Fatalf("upward: %v", err)
	}
	tl := actorAs(t, tlM.ID(), authority.RankTeamLead, permission.IdentityPermissions.Users.Update)
	if err := run(tl, exec.ID(), mgr2.ID()); !errors.Is(err, authority.ErrInsufficientRank) {
		t.Fatalf("team lead appoints senior / unlinks senior: %v", err)
	}
	if err := run(admin, exec.ID(), membership.ID(strings.ToUpper(string(exec.ID())))); !errors.Is(err, membership.ErrInvalid) {
		t.Fatalf("uppercase self link: %v", err)
	}
	// a manager appointing themselves over a senior member or a peer is refused
	mgrActor := actorAs(t, mgr1.ID(), authority.RankManager, permission.IdentityPermissions.Users.Update)
	if err := run(mgrActor, adminM.ID(), mgr1.ID()); !errors.Is(err, authority.ErrInsufficientRank) {
		t.Fatalf("manager over an admin: %v, want ErrInsufficientRank", err)
	}
	if err := run(mgrActor, mgr2.ID(), mgr1.ID()); !errors.Is(err, authority.ErrInsufficientRank) {
		t.Fatalf("manager over a peer: %v, want ErrInsufficientRank", err)
	}
	// stale upward link mgrStale -> tlStale, then tlStale -> mgrStale would close a cycle
	tlStale := w.member(t, w.teamLead)
	mgrStale := w.memberReporting(t, tlStale.ID(), w.manager)
	if err := run(admin, tlStale.ID(), mgrStale.ID()); !errors.Is(err, authority.ErrManagerNotEligible) {
		t.Fatalf("cycle: %v", err)
	}
	// a member whose manager cannot be loaded is not decided without it
	orphan := w.memberReporting(t, membership.ID(awID(31338)), w.exec)
	if err := run(admin, orphan.ID(), mgr1.ID()); !errors.Is(err, authority.ErrNoAuthority) {
		t.Fatalf("unloadable current manager: %v, want ErrNoAuthority", err)
	}
}

func TestRemoveUserManager_JuniorCannotUnlinkSenior(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	mgrM := w.member(t, w.manager)
	mgrPeer := w.member(t, w.manager)
	tlM := w.member(t, w.teamLead)
	exec := w.memberReporting(t, mgrM.ID(), w.exec)
	h := w.handlers().clearMgr
	cmd := command.RemoveUserManagerCommand{TenantID: awTenant, MembershipID: exec.ID()}
	cmd.Actor = actorAs(t, tlM.ID(), authority.RankTeamLead, permission.IdentityPermissions.Users.Update)
	if err := h.Handle(t.Context(), cmd); !errors.Is(err, authority.ErrInsufficientRank) {
		t.Fatalf("team lead: %v", err)
	}
	cmd.Actor = actorAs(t, mgrPeer.ID(), authority.RankManager, permission.IdentityPermissions.Users.Update)
	if err := h.Handle(t.Context(), cmd); !errors.Is(err, authority.ErrInsufficientRank) {
		t.Fatalf("equal-rank manager: %v", err)
	}
	cmd.Actor = actorAs(t, mgrM.ID(), authority.RankManager, permission.IdentityPermissions.Users.Update)
	if err := h.Handle(t.Context(), cmd); err != nil {
		t.Fatalf("the manager themself: %v", err)
	}
}

// emptyGuard is a PersonChangeGuard over repositories that hold no
// memberships: for handler tests that do not exercise the last-owner guard.
func emptyGuard() command.PersonChangeGuard {
	return command.NewPersonChangeGuard(fakeUoW{}, &fakeAuthorityLock{}, membershiptest.NewFakeRepository(), roletest.NewFakeRepository(), anyPersons{}, awClock)
}

// seedAdmin adds an Administrator role and an active member holding it to the
// given repositories (the tenant of awTenant) and returns the actor a login for
// that member would mint: rank Admin, tenant.admin expanded.
func seedAdmin(t *testing.T, members *membershiptest.FakeRepository, roles *roletest.FakeRepository) authority.Actor {
	t.Helper()
	r := role.UnmarshalFromDB(role.Snapshot{ID: role.ID(awID(8001)), TenantID: awTenant, Name: role.SystemRoles.Tenant.Administrator,
		HierarchyLevel: role.HierarchyLevelAdmin, Permissions: []*permission.Permission{permission.FromConstant(permission.IdentityPermissions.Meta.TenantAdmin)}, CreatedAt: testNow})
	if err := roles.Add(t.Context(), r); err != nil {
		t.Fatalf("seed admin role: %v", err)
	}
	m := membership.UnmarshalFromDB(membership.Snapshot{ID: membership.ID(awID(8002)), PersonID: person.ID(awID(8003)), TenantID: awTenant,
		Status: membership.StatusActive, JoinedAt: testNow, RoleAssignments: []role.ID{r.ID()}})
	if err := members.Add(t.Context(), m); err != nil {
		t.Fatalf("seed admin member: %v", err)
	}
	return actorAs(t, m.ID(), authority.RankAdmin, tenantAdminEffective()...)
}

// A name listed twice must not make a removal look like "no change": granted
// ["read","read"] for a member holding {read, assign} removes assign.
func TestReplaceOverrides_DuplicateGrantedNamesStillApplyTheRemoval(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	w := newAuthzWorld(t)
	exec := w.memberSnap(t, membership.Snapshot{RoleAssignments: []role.ID{w.exec.ID()},
		GrantedPermissions: []membership.GrantedOverride{
			{Permission: permission.FromConstant(p.Crm.Leads.Read)},
			{Permission: permission.FromConstant(p.Crm.Leads.Assign)},
		}})
	mgrM := w.memberSnap(t, membership.Snapshot{RoleAssignments: []role.ID{w.manager.ID()},
		GrantedPermissions: []membership.GrantedOverride{
			{Permission: permission.FromConstant(p.Crm.Leads.Read)},
			{Permission: permission.FromConstant(p.Crm.Leads.Assign)},
		}})
	mgr := actorAs(t, mgrM.ID(), authority.RankManager, p.Crm.Leads.Read, p.Crm.Leads.Assign)
	h := w.handlers().override
	if err := h.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: exec.ID(),
		GrantedNames: []string{p.Crm.Leads.Read, p.Crm.Leads.Read}, Actor: mgr}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got := exec.GrantedPermissions()
	if len(got) != 1 || got[0].Permission.Name() != p.Crm.Leads.Read {
		t.Fatalf("grants after the replace = %+v, want only %s (the removed grant must be gone, not silently kept)", got, p.Crm.Leads.Read)
	}
	// and a real no-op still writes nothing
	if err := h.Handle(t.Context(), command.ReplaceUserPermissionOverridesCommand{TenantID: awTenant, MembershipID: exec.ID(),
		GrantedNames: []string{p.Crm.Leads.Read, p.Crm.Leads.Read}, Actor: mgr}); err != nil {
		t.Fatalf("repeat: %v", err)
	}
}
