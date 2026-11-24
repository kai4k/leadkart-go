package command_test

import (
	"context"
	"errors"
	"strconv"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/authority"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/rolehierarchy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/rolehierarchy/rolehierarchytest"
)

// newFakeEdgeRepo aliases the co-located rolehierarchytest fake.
func newFakeEdgeRepo() *rolehierarchytest.FakeRepository {
	return rolehierarchytest.NewFakeRepository()
}

// fakeUoW is a no-op UnitOfWork that calls fn directly. The fakes don't use
// TxFromContext; real-tx atomicity is covered by adapter integration tests.
type fakeUoW struct{}

func (fakeUoW) WithinTx(ctx context.Context, _ pg.TxScope, fn func(context.Context) error) error {
	return fn(ctx)
}

func newRoleID() role.ID { return role.ID(ids.NewV7().String()) }
func newEdgeID() rolehierarchy.ID {
	return rolehierarchy.ID(ids.NewV7().String())
}

// roleHandlers are the seven role commands over one authzWorld.
type roleHandlers struct {
	create  command.CreateRoleHandler
	update  command.UpdateRoleHandler
	replace command.ReplaceRolePermissionsHandler
	grant   command.GrantRolePermissionHandler
	revoke  command.RevokeRolePermissionHandler
	del     command.DeactivateRoleHandler
	react   command.ReactivateRoleHandler
	parent  command.SetRoleParentHandler
	edges   *rolehierarchytest.FakeRepository
}

func (w *authzWorld) roleHandlersWith(edges rolehierarchy.Repository, uow pg.UnitOfWork) roleHandlers {
	l := command.AuthorityLocker(w.lock)
	p := anyPersons{rotations: &w.rotations}
	fe, _ := edges.(*rolehierarchytest.FakeRepository)
	return roleHandlers{
		create:  command.NewCreateRoleHandler(uow, l, w.members, w.roles, p, edges, awClock, newRoleID, newEdgeID),
		update:  command.NewUpdateRoleHandler(uow, l, w.members, w.roles, p, awClock),
		replace: command.NewReplaceRolePermissionsHandler(uow, l, w.members, w.roles, p, awClock),
		grant:   command.NewGrantRolePermissionHandler(uow, l, w.members, w.roles, p, awClock),
		revoke:  command.NewRevokeRolePermissionHandler(uow, l, w.members, w.roles, p, awClock),
		del:     command.NewDeactivateRoleHandler(uow, l, w.members, w.roles, p, awClock),
		react:   command.NewReactivateRoleHandler(uow, l, w.members, w.roles, p, awClock),
		parent:  command.NewSetRoleParentHandler(uow, l, w.members, w.roles, p, edges, awClock, newEdgeID),
		edges:   fe,
	}
}

func (w *authzWorld) roleHandlers() roleHandlers {
	return w.roleHandlersWith(newFakeEdgeRepo(), fakeUoW{})
}

// customRole adds a live custom role at level to the world's repository.
func (w *authzWorld) customRole(t *testing.T, name string, level int, perms ...string) *role.Role {
	t.Helper()
	w.next++
	return w.addRoleSnap(t, role.Snapshot{ID: role.ID(awID(w.next)), TenantID: awTenant, Name: name, HierarchyLevel: level, CreatedAt: testNow, IsActive: true}, perms...)
}

func (w *authzWorld) adminActor(t *testing.T) authority.Actor {
	t.Helper()
	return actorAs(t, w.member(t, w.admin).ID(), authority.RankAdmin, tenantAdminEffective()...)
}

func TestRoleCommands_ZeroActorDeniedBeforeLock(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	r := w.customRole(t, "Custom", 60)
	h := w.roleHandlers()
	ctx := t.Context()
	_, createErr := h.create.Handle(ctx, command.CreateRoleCommand{TenantID: awTenant, Name: "X Role", HierarchyLevel: 40})
	cases := map[string]error{
		"create":     createErr,
		"update":     h.update.Handle(ctx, command.UpdateRoleCommand{TenantID: awTenant, RoleID: r.ID(), Name: "Y Role", HierarchyLevel: -1}),
		"replace":    h.replace.Handle(ctx, command.ReplaceRolePermissionsCommand{TenantID: awTenant, RoleID: r.ID(), PermissionNames: []string{}}),
		"grant":      h.grant.Handle(ctx, command.GrantRolePermissionCommand{TenantID: awTenant, RoleID: r.ID(), PermissionName: permission.IdentityPermissions.Crm.Leads.Read}),
		"revoke":     h.revoke.Handle(ctx, command.RevokeRolePermissionCommand{TenantID: awTenant, RoleID: r.ID(), PermissionName: permission.IdentityPermissions.Crm.Leads.Read}),
		"deactivate": h.del.Handle(ctx, command.ChangeRoleStatusCommand{TenantID: awTenant, RoleID: r.ID()}),
		"parent":     h.parent.Handle(ctx, command.SetRoleParentCommand{TenantID: awTenant, RoleID: r.ID()}),
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

func TestRoleCommands_RunUnderTheTenantLock(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	r := w.customRole(t, "Custom", 60, permission.IdentityPermissions.Crm.Leads.Read)
	h := w.roleHandlers()
	admin := w.adminActor(t)
	ctx := t.Context()
	steps := []func() error{
		func() error {
			_, err := h.create.Handle(ctx, command.CreateRoleCommand{TenantID: awTenant, Name: "Lead", HierarchyLevel: 40, Actor: admin})
			return err
		},
		func() error {
			return h.update.Handle(ctx, command.UpdateRoleCommand{TenantID: awTenant, RoleID: r.ID(), Name: "Renamed", HierarchyLevel: -1, Actor: admin})
		},
		func() error {
			return h.replace.Handle(ctx, command.ReplaceRolePermissionsCommand{TenantID: awTenant, RoleID: r.ID(), PermissionNames: []string{permission.IdentityPermissions.Crm.Leads.Read}, Actor: admin})
		},
		func() error {
			return h.grant.Handle(ctx, command.GrantRolePermissionCommand{TenantID: awTenant, RoleID: r.ID(), PermissionName: permission.IdentityPermissions.Crm.Leads.ReadAll, Actor: admin})
		},
		func() error {
			return h.revoke.Handle(ctx, command.RevokeRolePermissionCommand{TenantID: awTenant, RoleID: r.ID(), PermissionName: permission.IdentityPermissions.Crm.Leads.ReadAll, Actor: admin})
		},
		func() error {
			return h.parent.Handle(ctx, command.SetRoleParentCommand{TenantID: awTenant, RoleID: r.ID(), NewParentID: w.manager.ID(), Actor: admin})
		},
		func() error {
			return h.del.Handle(ctx, command.ChangeRoleStatusCommand{TenantID: awTenant, RoleID: r.ID(), Actor: admin})
		},
	}
	for i, step := range steps {
		if err := step(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if w.lock.calls != i+1 {
			t.Fatalf("after step %d lock calls = %d, want %d", i, w.lock.calls, i+1)
		}
	}
	if r.IsActive() {
		t.Error("custom role not deactivated by the Administrator")
	}
}

func TestCreateRole_PolicyDecides(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	h := w.roleHandlers()
	mgrM := w.member(t, w.manager)
	mgr := actorAs(t, mgrM.ID(), authority.RankManager, permission.IdentityPermissions.Crm.Leads.Read)
	create := func(a authority.Actor, level int, parent role.ID) error {
		_, err := h.create.Handle(t.Context(), command.CreateRoleCommand{TenantID: awTenant, Name: "Brand New", HierarchyLevel: level, ParentRoleID: parent, Actor: a})
		return err
	}
	if err := create(mgr, role.HierarchyLevelManager, ""); !errors.Is(err, authority.ErrInsufficientRank) {
		t.Errorf("manager creates at their own level: %v, want ErrInsufficientRank", err)
	}
	if err := create(mgr, role.HierarchyLevelAdmin, ""); !errors.Is(err, authority.ErrInsufficientRank) {
		t.Errorf("manager creates above themselves: %v, want ErrInsufficientRank", err)
	}
	if err := create(mgr, 0, ""); !errors.Is(err, authority.ErrInsufficientRank) {
		t.Errorf("level 0: %v, want ErrInsufficientRank", err)
	}
	if err := create(mgr, role.HierarchyLevelTeamLead, role.ID(awID(4040))); !errors.Is(err, authority.ErrRoleParentNotEligible) {
		t.Errorf("missing parent: %v, want ErrRoleParentNotEligible", err)
	}
	// a parent that is not strictly more senior than the child
	peer := w.customRole(t, "Peer", 40)
	if err := create(mgr, role.HierarchyLevelTeamLead, peer.ID()); !errors.Is(err, authority.ErrRoleParentNotEligible) {
		t.Errorf("parent at level 40 over a level-30 child: %v, want ErrRoleParentNotEligible", err)
	}
	if err := create(mgr, role.HierarchyLevelTeamLead, w.manager.ID()); err != nil {
		t.Errorf("manager creates a team-lead-level role under the manager role: %v", err)
	}
}

func TestUpdateRole_PolicyDecides(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	h := w.roleHandlers()
	r := w.customRole(t, "Custom", 60)
	mgrM := w.member(t, w.manager)
	mgr := actorAs(t, mgrM.ID(), authority.RankManager)
	up := func(a authority.Actor, id role.ID, name string, level int) error {
		return h.update.Handle(t.Context(), command.UpdateRoleCommand{TenantID: awTenant, RoleID: id, Name: name, HierarchyLevel: level, Actor: a})
	}
	if err := up(mgr, r.ID(), "", role.HierarchyLevelAdmin); !errors.Is(err, authority.ErrInsufficientRank) {
		t.Errorf("re-level above the actor: %v, want ErrInsufficientRank", err)
	}
	if r.HierarchyLevel() != 60 {
		t.Errorf("level changed to %d by a refused call", r.HierarchyLevel())
	}
	if err := up(superActor(t), w.owner.ID(), "Pwned Owner", -1); !errors.Is(err, role.ErrSystemDefault) {
		t.Errorf("super user renames a system role: %v, want ErrSystemDefault", err)
	}
	if got := w.owner.Name(); got != role.SystemRoles.Tenant.CompanyOwner {
		t.Errorf("system role renamed to %q", got)
	}
	if err := up(mgr, r.ID(), "Renamed", 70); err != nil {
		t.Fatalf("manager renames and re-levels a role below them: %v", err)
	}
	if r.Name() != "Renamed" || r.HierarchyLevel() != 70 {
		t.Errorf("role = %q at %d, want Renamed at 70", r.Name(), r.HierarchyLevel())
	}
	if err := up(w.adminActor(t), role.ID(awID(404)), "x", -1); !errors.Is(err, command.ErrRoleNotFound) {
		t.Errorf("unknown role: %v, want ErrRoleNotFound", err)
	}
}

func TestRolePermissions_PolicyDecides(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	w := newAuthzWorld(t)
	h := w.roleHandlers()
	ctx := t.Context()
	custom := w.customRole(t, "Custom", 60, p.Crm.Leads.Read, p.Roles.Update)
	mgrM := w.member(t, w.manager)
	mgr := actorAs(t, mgrM.ID(), authority.RankManager, p.Crm.Leads.Read, p.Crm.Leads.ReadAll)

	// the Company Owner role is immutable for every actor, whatever they hold
	for name, err := range map[string]error{
		"admin revokes tenant.admin": h.revoke.Handle(ctx, command.RevokeRolePermissionCommand{TenantID: awTenant, RoleID: w.owner.ID(), PermissionName: p.Meta.TenantAdmin, Actor: w.adminActor(t)}),
		"super revokes tenant.admin": h.revoke.Handle(ctx, command.RevokeRolePermissionCommand{TenantID: awTenant, RoleID: w.owner.ID(), PermissionName: p.Meta.TenantAdmin, Actor: superActor(t)}),
		"admin PUT []":               h.replace.Handle(ctx, command.ReplaceRolePermissionsCommand{TenantID: awTenant, RoleID: w.owner.ID(), PermissionNames: []string{}, Actor: w.adminActor(t)}),
		"super PUT []":               h.replace.Handle(ctx, command.ReplaceRolePermissionsCommand{TenantID: awTenant, RoleID: w.owner.ID(), PermissionNames: []string{}, Actor: superActor(t)}),
		"super grants":               h.grant.Handle(ctx, command.GrantRolePermissionCommand{TenantID: awTenant, RoleID: w.owner.ID(), PermissionName: p.Crm.Leads.Read, Actor: superActor(t)}),
		"super deletes":              h.del.Handle(ctx, command.ChangeRoleStatusCommand{TenantID: awTenant, RoleID: w.owner.ID(), Actor: superActor(t)}),
	} {
		if !errors.Is(err, role.ErrSystemDefault) {
			t.Errorf("%s: %v, want ErrSystemDefault", name, err)
		}
	}
	if !w.owner.HasPermission(permission.FromConstant(p.Meta.TenantAdmin)) {
		t.Fatal("Company Owner lost tenant.admin")
	}
	// a manager cannot confer what they do not hold, nor remove what they do not hold
	if err := h.grant.Handle(ctx, command.GrantRolePermissionCommand{TenantID: awTenant, RoleID: custom.ID(), PermissionName: p.Crm.Leads.Assign, Actor: mgr}); !errors.Is(err, authority.ErrGrantExceedsAuthority) {
		t.Errorf("grant unheld: %v, want ErrGrantExceedsAuthority", err)
	}
	if err := h.revoke.Handle(ctx, command.RevokeRolePermissionCommand{TenantID: awTenant, RoleID: custom.ID(), PermissionName: p.Roles.Update, Actor: mgr}); !errors.Is(err, authority.ErrGrantExceedsAuthority) {
		t.Errorf("revoke unheld: %v, want ErrGrantExceedsAuthority", err)
	}
	if err := h.replace.Handle(ctx, command.ReplaceRolePermissionsCommand{TenantID: awTenant, RoleID: custom.ID(), PermissionNames: []string{}, Actor: mgr}); !errors.Is(err, authority.ErrGrantExceedsAuthority) {
		t.Errorf("empty the role (removes unheld roles.update): %v, want ErrGrantExceedsAuthority", err)
	}
	if got := len(custom.Permissions()); got != 2 {
		t.Errorf("refused calls changed the role: %d permissions, want 2", got)
	}
	if err := h.grant.Handle(ctx, command.GrantRolePermissionCommand{TenantID: awTenant, RoleID: custom.ID(), PermissionName: p.Crm.Leads.ReadAll, Actor: mgr}); err != nil {
		t.Errorf("grant held: %v", err)
	}
	if err := h.replace.Handle(ctx, command.ReplaceRolePermissionsCommand{TenantID: awTenant, RoleID: custom.ID(), PermissionNames: []string{"identity.totally.fake"}, Actor: mgr}); !errors.Is(err, command.ErrPermissionUnknown) {
		t.Errorf("unknown name: %v, want ErrPermissionUnknown", err)
	}
}

// ADR 0073: deactivating asks exactly what deleting asked (authority and ceiling first, then the
// role-in-use rule), keeps every assignment (nothing is deleted), and reactivating asks the same
// authority again because it gives the permissions back to every holder.
func TestDeactivateRole_RoleInUseAndCeiling(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	w := newAuthzWorld(t)
	h := w.roleHandlers()
	ctx := t.Context()

	carrier := w.customRole(t, "Carries Admin", 60, p.Meta.TenantAdmin)
	noAdmin := actorAs(t, w.member(t, w.admin).ID(), authority.RankAdmin, p.Crm.Leads.Read)
	if err := h.del.Handle(ctx, command.ChangeRoleStatusCommand{TenantID: awTenant, RoleID: carrier.ID(), Actor: noAdmin}); !errors.Is(err, authority.ErrGrantExceedsAuthority) {
		t.Errorf("Admin without tenant.admin deletes a role carrying it: %v, want ErrGrantExceedsAuthority", err)
	}
	if !carrier.IsActive() {
		t.Fatal("role deactivated by a refused call")
	}

	custom := w.customRole(t, "Held", 60)
	active := w.member(t, custom)
	inactive := w.memberSnap(t, membership.Snapshot{RoleAssignments: []role.ID{custom.ID(), w.exec.ID()}, Status: membership.StatusInactive})
	admin := w.adminActor(t)
	err := h.del.Handle(ctx, command.ChangeRoleStatusCommand{TenantID: awTenant, RoleID: custom.ID(), Actor: admin})
	var inUse *command.RoleInUseError
	if !errors.Is(err, command.ErrRoleInUse) || !errors.As(err, &inUse) || inUse.ActiveHolders != 1 {
		t.Fatalf("deactivate with an active holder: %v, want RoleInUseError with 1 holder", err)
	}
	if !custom.IsActive() || len(inactive.RoleAssignments()) != 2 {
		t.Fatalf("a refused deactivation changed state: active=%v inactive roles=%v", custom.IsActive(), inactive.RoleAssignments())
	}

	// the active holder moves on; only an inactive holder remains
	if err := w.members.UpdateByID(ctx, awTenant, active.ID(), func(m *membership.Membership) (bool, error) {
		return true, m.RevokeRole(custom.ID(), testNow)
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.del.Handle(ctx, command.ChangeRoleStatusCommand{TenantID: awTenant, RoleID: custom.ID(), Actor: admin}); err != nil {
		t.Fatalf("deactivate with only an inactive holder: %v", err)
	}
	if custom.IsActive() {
		t.Error("role not deactivated")
	}
	if got := inactive.RoleAssignments(); len(got) != 2 {
		t.Errorf("inactive holder's assignments = %v, want both kept (nothing is deleted)", got)
	}
	// Idempotent: deactivating an inactive role is a no-op.
	if err := h.del.Handle(ctx, command.ChangeRoleStatusCommand{TenantID: awTenant, RoleID: custom.ID(), Actor: admin}); err != nil {
		t.Fatalf("second deactivate: %v", err)
	}

	// Reactivating asks the same authority (it gives the role's permissions back to every holder).
	if err := h.react.Handle(ctx, command.ChangeRoleStatusCommand{TenantID: awTenant, RoleID: custom.ID()}); !errors.Is(err, authority.ErrNoAuthority) {
		t.Errorf("reactivate with a zero actor: %v, want ErrNoAuthority", err)
	}
	if custom.IsActive() {
		t.Fatal("role reactivated by a refused call")
	}
	if err := h.react.Handle(ctx, command.ChangeRoleStatusCommand{TenantID: awTenant, RoleID: custom.ID(), Actor: admin}); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	if !custom.IsActive() {
		t.Error("role not reactivated")
	}
	if err := h.react.Handle(ctx, command.ChangeRoleStatusCommand{TenantID: awTenant, RoleID: custom.ID(), Actor: admin}); err != nil {
		t.Fatalf("second reactivate: %v", err)
	}
}

func TestReactivateRole_CeilingAppliesLikeDeactivate(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	w := newAuthzWorld(t)
	h := w.roleHandlers()
	ctx := t.Context()
	carrier := w.customRole(t, "Carries Admin", 60, p.Meta.TenantAdmin)
	if err := carrier.Deactivate("p", testNow); err != nil {
		t.Fatal(err)
	}
	noAdmin := actorAs(t, w.member(t, w.admin).ID(), authority.RankAdmin, p.Crm.Leads.Read)
	if err := h.react.Handle(ctx, command.ChangeRoleStatusCommand{TenantID: awTenant, RoleID: carrier.ID(), Actor: noAdmin}); !errors.Is(err, authority.ErrGrantExceedsAuthority) {
		t.Errorf("an Administrator without tenant.admin reactivates a role carrying it: %v, want ErrGrantExceedsAuthority", err)
	}
	if carrier.IsActive() {
		t.Fatal("role reactivated by a refused call")
	}
}

func TestSetRoleParent_PolicyDecides(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	h := w.roleHandlers()
	ctx := t.Context()
	admin := w.adminActor(t)
	child := w.customRole(t, "Child", 60)
	peer := w.customRole(t, "Peer", 60)
	if err := h.parent.Handle(ctx, command.SetRoleParentCommand{TenantID: awTenant, RoleID: child.ID(), NewParentID: peer.ID(), Actor: admin}); !errors.Is(err, authority.ErrRoleParentNotEligible) {
		t.Errorf("equal-level parent: %v, want ErrRoleParentNotEligible", err)
	}
	if err := h.parent.Handle(ctx, command.SetRoleParentCommand{TenantID: awTenant, RoleID: child.ID(), NewParentID: role.ID(awID(404)), Actor: admin}); !errors.Is(err, authority.ErrRoleParentNotEligible) {
		t.Errorf("missing parent: %v, want ErrRoleParentNotEligible", err)
	}
	if err := h.parent.Handle(ctx, command.SetRoleParentCommand{TenantID: awTenant, RoleID: w.owner.ID(), NewParentID: w.exec.ID(), Actor: superActor(t)}); !errors.Is(err, role.ErrSystemDefault) {
		t.Errorf("edit a system role's parent: %v, want ErrSystemDefault", err)
	}
	if err := h.parent.Handle(ctx, command.SetRoleParentCommand{TenantID: awTenant, RoleID: child.ID(), NewParentID: w.manager.ID(), Actor: admin}); err != nil {
		t.Fatalf("senior parent: %v", err)
	}
	got, err := h.edges.GetActiveByChild(ctx, awTenant, child.ID())
	if err != nil || got.ParentRoleID() != w.manager.ID() {
		t.Fatalf("edge = %v, %v; want parent %s", got, err, w.manager.ID())
	}
	// replace, then clear
	if err := h.parent.Handle(ctx, command.SetRoleParentCommand{TenantID: awTenant, RoleID: child.ID(), NewParentID: w.teamLead.ID(), Actor: admin}); err != nil {
		t.Fatalf("replace parent: %v", err)
	}
	if got, _ := h.edges.GetActiveByChild(ctx, awTenant, child.ID()); got == nil || got.ParentRoleID() != w.teamLead.ID() {
		t.Errorf("edge after replace = %v, want parent %s", got, w.teamLead.ID())
	}
	if err := h.parent.Handle(ctx, command.SetRoleParentCommand{TenantID: awTenant, RoleID: child.ID(), Reason: "promotion to root role for org restructure", Actor: admin}); err != nil {
		t.Fatalf("clear parent: %v", err)
	}
	if _, err := h.edges.GetActiveByChild(ctx, awTenant, child.ID()); !errors.Is(err, rolehierarchy.ErrEdgeNotFound) {
		t.Errorf("edge after clear: %v, want ErrEdgeNotFound", err)
	}
	// self reference is refused by the aggregate (the child is not "more senior than itself" either)
	if err := h.parent.Handle(ctx, command.SetRoleParentCommand{TenantID: awTenant, RoleID: child.ID(), NewParentID: child.ID(), Actor: admin}); err == nil {
		t.Error("self-parent accepted")
	}
}

func TestCreateRole_InputAndEdgeErrors(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	admin := w.adminActor(t)
	h := w.roleHandlers()
	if _, err := h.create.Handle(t.Context(), command.CreateRoleCommand{Name: "Sales Lead", HierarchyLevel: 40, Actor: admin}); err == nil {
		t.Error("zero tenant accepted")
	}
	if _, err := h.create.Handle(t.Context(), command.CreateRoleCommand{TenantID: awTenant, Name: "", HierarchyLevel: 40, Actor: admin}); !errors.Is(err, role.ErrInvalid) {
		t.Errorf("empty name: %v, want role.ErrInvalid", err)
	}
	if _, err := h.create.Handle(t.Context(), command.CreateRoleCommand{TenantID: awTenant, Name: "Sales Lead", HierarchyLevel: 9999, Actor: admin}); !errors.Is(err, authority.ErrInsufficientRank) {
		t.Errorf("level 9999: %v, want ErrInsufficientRank", err)
	}
	w.customRole(t, "Sales Manager Two", 60)
	if _, err := h.create.Handle(t.Context(), command.CreateRoleCommand{TenantID: awTenant, Name: "Sales Manager Two", HierarchyLevel: 50, Actor: admin}); !errors.Is(err, command.ErrRoleNameTaken) {
		t.Errorf("duplicate name: %v, want ErrRoleNameTaken", err)
	}
	for i, injected := range []error{rolehierarchy.ErrCycle, rolehierarchy.ErrCrossTenant, rolehierarchy.ErrEdgeAlreadyExists} {
		edges := &failingEdgesRepo{FakeRepository: rolehierarchytest.NewFakeRepository(), addErr: injected}
		hh := w.roleHandlersWith(edges, fakeUoW{})
		_, err := hh.create.Handle(t.Context(), command.CreateRoleCommand{TenantID: awTenant, Name: "Child Role " + strconv.Itoa(i), HierarchyLevel: 70, ParentRoleID: w.manager.ID(), Actor: admin})
		if !errors.Is(err, injected) {
			t.Errorf("edge error %v: got %v", injected, err)
		}
	}
	hh := w.roleHandlersWith(newFakeEdgeRepo(), failingTxUoW{err: errBoom})
	if _, err := hh.create.Handle(t.Context(), command.CreateRoleCommand{TenantID: awTenant, Name: "Child Role", HierarchyLevel: 70, ParentRoleID: w.manager.ID(), Actor: admin}); !errors.Is(err, errBoom) {
		t.Errorf("tx error: %v, want errBoom in the chain", err)
	}
}

func TestRoleHandlers_WiringIsRequired(t *testing.T) {
	t.Parallel()
	w := newAuthzWorld(t)
	l := command.AuthorityLocker(w.lock)
	for name, fn := range map[string]func(){
		"create without edges": func() {
			command.NewCreateRoleHandler(fakeUoW{}, l, w.members, w.roles, anyPersons{}, nil, awClock, newRoleID, newEdgeID)
		},
		"set parent without edges": func() {
			command.NewSetRoleParentHandler(fakeUoW{}, l, w.members, w.roles, anyPersons{}, nil, awClock, newEdgeID)
		},
		"update without the lock":        func() { command.NewUpdateRoleHandler(fakeUoW{}, nil, w.members, w.roles, anyPersons{}, awClock) },
		"deactivate without memberships": func() { command.NewDeactivateRoleHandler(fakeUoW{}, l, nil, w.roles, anyPersons{}, awClock) },
		"reactivate without memberships": func() { command.NewReactivateRoleHandler(fakeUoW{}, l, nil, w.roles, anyPersons{}, awClock) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s: no panic", name)
				}
			}()
			fn()
		}()
	}
}

// ----- test doubles ----------------------------------------------------------

// failingEdgesRepo overrides Add to inject rolehierarchy.Err* values.
type failingEdgesRepo struct {
	*rolehierarchytest.FakeRepository
	addErr error
}

func (r *failingEdgesRepo) Add(ctx context.Context, e *rolehierarchy.Edge) error {
	if r.addErr != nil {
		return r.addErr
	}
	return r.FakeRepository.Add(ctx, e)
}

// failingTxUoW returns a fixed error from WithinTx (tx-error scenarios).
type failingTxUoW struct {
	err error
}

func (u failingTxUoW) WithinTx(_ context.Context, _ pg.TxScope, _ func(context.Context) error) error {
	return u.err
}
