package permissions_test

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/app/permissions"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// fakeMembershipRepo / fakeRoleRepo are minimal in-memory impls of the
// domain Repository contracts. They cover the specific call shapes the
// Resolver uses; full repo behaviour (AddInTx, etc.) lives in the
// adapter integration tests.
type fakeMembershipRepo struct {
	memberships map[membership.ID]*membership.Membership
	getErr      error
}

func (f *fakeMembershipRepo) GetByID(_ context.Context, id membership.ID) (*membership.Membership, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	m, ok := f.memberships[id]
	if !ok {
		return nil, membership.ErrNotFound
	}
	return m, nil
}

func (f *fakeMembershipRepo) Add(context.Context, *membership.Membership) error {
	return errors.New("fake: Add unused")
}
func (f *fakeMembershipRepo) UpdateByID(context.Context, membership.ID, func(*membership.Membership) (bool, error)) error {
	return errors.New("fake: UpdateByID unused")
}
func (f *fakeMembershipRepo) GetActiveForPerson(context.Context, person.ID) (*membership.Membership, error) {
	return nil, errors.New("fake: GetActiveForPerson unused")
}
func (f *fakeMembershipRepo) ListForTenant(context.Context, tenant.ID) ([]*membership.Membership, error) {
	return nil, errors.New("fake: ListForTenant unused")
}
func (f *fakeMembershipRepo) ListAllForPerson(context.Context, person.ID) ([]*membership.Membership, error) {
	return nil, errors.New("fake: ListAllForPerson unused")
}

type fakeRoleRepo struct {
	roles map[role.ID]*role.Role
}

func (f *fakeRoleRepo) GetByIDs(_ context.Context, ids []role.ID) ([]*role.Role, error) {
	out := make([]*role.Role, 0, len(ids))
	for _, id := range ids {
		if r, ok := f.roles[id]; ok && !r.IsDeleted() {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeRoleRepo) Add(context.Context, *role.Role) error {
	return errors.New("fake: Add unused")
}
func (f *fakeRoleRepo) UpdateByID(context.Context, role.ID, func(*role.Role) (bool, error)) error {
	return errors.New("fake: UpdateByID unused")
}
func (f *fakeRoleRepo) GetByID(context.Context, role.ID) (*role.Role, error) {
	return nil, errors.New("fake: GetByID unused")
}
func (f *fakeRoleRepo) GetByTenantAndName(context.Context, tenant.ID, string) (*role.Role, error) {
	return nil, errors.New("fake: GetByTenantAndName unused")
}
func (f *fakeRoleRepo) ListByTenant(context.Context, tenant.ID) ([]*role.Role, error) {
	return nil, errors.New("fake: ListByTenant unused")
}

// ----- helpers --------------------------------------------------------------

func freshTenantID(t *testing.T) tenant.ID {
	t.Helper()
	return tenant.ID(ids.NewV7().String())
}

func newMembership(t *testing.T, tid tenant.ID) *membership.Membership {
	t.Helper()
	pid := person.ID(ids.NewV7().String())
	mid := membership.ID(ids.NewV7().String())
	m, err := membership.New(mid, pid, tid)
	if err != nil {
		t.Fatalf("membership.New: %v", err)
	}
	return m
}

func newRoleWith(
	t *testing.T,
	tid tenant.ID,
	name string,
	perms ...*permission.Permission,
) *role.Role {
	t.Helper()
	r, err := role.New(
		role.ID(ids.NewV7().String()), tid, name,
		false, role.HierarchyLevelDefault, false)
	if err != nil {
		t.Fatalf("role.New: %v", err)
	}
	for _, p := range perms {
		if err := r.GrantPermission(p); err != nil {
			t.Fatalf("GrantPermission: %v", err)
		}
	}
	return r
}

func names(perms []*permission.Permission) []string {
	out := make([]string, 0, len(perms))
	for _, p := range perms {
		out = append(out, p.Name())
	}
	slices.Sort(out)
	return out
}

// ----- Resolve --------------------------------------------------------------

func TestResolve_NoRoles_NoOverlay_ReturnsEmpty(t *testing.T) {
	t.Parallel()
	tid := freshTenantID(t)
	m := newMembership(t, tid)
	mems := &fakeMembershipRepo{memberships: map[membership.ID]*membership.Membership{m.ID(): m}}
	roles := &fakeRoleRepo{roles: map[role.ID]*role.Role{}}
	r := permissions.NewResolver(mems, roles)

	got, err := r.Resolve(t.Context(), m.ID())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Resolve: got %d want 0", len(got))
	}
}

func TestResolve_UnionsRolePermissions(t *testing.T) {
	t.Parallel()
	tid := freshTenantID(t)
	m := newMembership(t, tid)

	view := permission.FromConstant(permission.IdentityPermissions.Roles.View)
	assign := permission.FromConstant(permission.IdentityPermissions.Roles.Assign)
	r1 := newRoleWith(t, tid, "Viewer", view)
	r2 := newRoleWith(t, tid, "Assigner", assign)
	if err := m.AssignRole(r1.ID()); err != nil {
		t.Fatalf("AssignRole r1: %v", err)
	}
	if err := m.AssignRole(r2.ID()); err != nil {
		t.Fatalf("AssignRole r2: %v", err)
	}

	mems := &fakeMembershipRepo{memberships: map[membership.ID]*membership.Membership{m.ID(): m}}
	roles := &fakeRoleRepo{roles: map[role.ID]*role.Role{r1.ID(): r1, r2.ID(): r2}}
	res := permissions.NewResolver(mems, roles)

	got, err := res.Resolve(t.Context(), m.ID())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []string{
		permission.IdentityPermissions.Roles.Assign,
		permission.IdentityPermissions.Roles.View,
	}
	if !slices.Equal(names(got), want) {
		t.Fatalf("Resolve names: got %v want %v", names(got), want)
	}
}

func TestResolve_OverlayGrantedExtendsBaseline(t *testing.T) {
	t.Parallel()
	tid := freshTenantID(t)
	m := newMembership(t, tid)
	view := permission.FromConstant(permission.IdentityPermissions.Roles.View)
	r1 := newRoleWith(t, tid, "Viewer", view)
	if err := m.AssignRole(r1.ID()); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	overlayP := permission.FromConstant(permission.IdentityPermissions.Users.Anonymise)
	if err := m.GrantPermission(overlayP); err != nil {
		t.Fatalf("GrantPermission overlay: %v", err)
	}

	mems := &fakeMembershipRepo{memberships: map[membership.ID]*membership.Membership{m.ID(): m}}
	roles := &fakeRoleRepo{roles: map[role.ID]*role.Role{r1.ID(): r1}}
	res := permissions.NewResolver(mems, roles)

	got, err := res.Resolve(t.Context(), m.ID())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []string{
		permission.IdentityPermissions.Roles.View,
		permission.IdentityPermissions.Users.Anonymise,
	}
	slices.Sort(want)
	if !slices.Equal(names(got), want) {
		t.Fatalf("Resolve overlay: got %v want %v", names(got), want)
	}
}

func TestResolve_OverlayRevokedSuppressesRoleGrant(t *testing.T) {
	t.Parallel()
	tid := freshTenantID(t)
	m := newMembership(t, tid)
	view := permission.FromConstant(permission.IdentityPermissions.Roles.View)
	assign := permission.FromConstant(permission.IdentityPermissions.Roles.Assign)
	r1 := newRoleWith(t, tid, "Both", view, assign)
	if err := m.AssignRole(r1.ID()); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}
	if err := m.RevokePermission(view); err != nil {
		t.Fatalf("RevokePermission overlay: %v", err)
	}

	mems := &fakeMembershipRepo{memberships: map[membership.ID]*membership.Membership{m.ID(): m}}
	roles := &fakeRoleRepo{roles: map[role.ID]*role.Role{r1.ID(): r1}}
	res := permissions.NewResolver(mems, roles)

	got, err := res.Resolve(t.Context(), m.ID())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !slices.Equal(names(got), []string{permission.IdentityPermissions.Roles.Assign}) {
		t.Fatalf("Resolve revoke: got %v want [Roles.Assign]", names(got))
	}
}

func TestResolve_PropagatesGetByIDError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("repo down")
	mems := &fakeMembershipRepo{
		memberships: map[membership.ID]*membership.Membership{},
		getErr:      sentinel,
	}
	res := permissions.NewResolver(mems, &fakeRoleRepo{})
	_, err := res.Resolve(t.Context(), membership.ID(ids.NewV7().String()))
	if !errors.Is(err, sentinel) {
		t.Fatalf("Resolve: got %v want sentinel", err)
	}
}

func TestResolveForLoaded_SkipsMembershipFetch(t *testing.T) {
	t.Parallel()
	tid := freshTenantID(t)
	m := newMembership(t, tid)
	view := permission.FromConstant(permission.IdentityPermissions.Roles.View)
	r1 := newRoleWith(t, tid, "Viewer", view)
	if err := m.AssignRole(r1.ID()); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}

	// No GetByID needed — pass mems=nil-equivalent (would panic if used).
	roles := &fakeRoleRepo{roles: map[role.ID]*role.Role{r1.ID(): r1}}
	res := permissions.NewResolver(&fakeMembershipRepo{
		memberships: map[membership.ID]*membership.Membership{},
	}, roles)

	got, err := res.ResolveForLoaded(t.Context(), m)
	if err != nil {
		t.Fatalf("ResolveForLoaded: %v", err)
	}
	if !slices.Equal(names(got), []string{permission.IdentityPermissions.Roles.View}) {
		t.Fatalf("ResolveForLoaded: got %v", names(got))
	}
}

func TestResolveForLoaded_RejectsNil(t *testing.T) {
	t.Parallel()
	res := permissions.NewResolver(&fakeMembershipRepo{}, &fakeRoleRepo{})
	_, err := res.ResolveForLoaded(t.Context(), nil)
	if err == nil {
		t.Fatal("ResolveForLoaded(nil) expected error")
	}
}

// ----- ResolveAuth (Task 22 — JWT claim wiring) -----------------------------

// newSuperAdminRole constructs a role with isSuperAdmin=true.
// Mirror of role.New's constructor — only platform-tenant seed code
// minted these; the test fakes that out to assert the flag flows
// through ResolveAuth.
func newSuperAdminRole(t *testing.T, tid tenant.ID) *role.Role {
	t.Helper()
	r, err := role.New(
		role.ID(ids.NewV7().String()), tid,
		role.SystemRoles.Platform.SuperAdmin,
		true,                       // IsSystemDefault
		role.HierarchyLevelMin,     // top of tree
		true,                       // IsSuperAdmin — the load-bearing flag
	)
	if err != nil {
		t.Fatalf("role.New super-admin: %v", err)
	}
	return r
}

func TestResolveAuth_NoSuperRole_IsSuperUserFalse(t *testing.T) {
	t.Parallel()
	tid := freshTenantID(t)
	m := newMembership(t, tid)
	view := permission.FromConstant(permission.IdentityPermissions.Roles.View)
	r1 := newRoleWith(t, tid, "Viewer", view)
	if err := m.AssignRole(r1.ID()); err != nil {
		t.Fatalf("AssignRole: %v", err)
	}

	mems := &fakeMembershipRepo{memberships: map[membership.ID]*membership.Membership{m.ID(): m}}
	roles := &fakeRoleRepo{roles: map[role.ID]*role.Role{r1.ID(): r1}}
	res := permissions.NewResolver(mems, roles)

	got, err := res.ResolveAuth(t.Context(), m)
	if err != nil {
		t.Fatalf("ResolveAuth: %v", err)
	}
	if got.IsSuperUser {
		t.Fatalf("IsSuperUser: got true, want false (no super-admin role assigned)")
	}
	if !slices.Equal(names(got.Permissions), []string{permission.IdentityPermissions.Roles.View}) {
		t.Fatalf("permissions: got %v want [Roles.View]", names(got.Permissions))
	}
}

func TestResolveAuth_AnySuperRoleAssigned_IsSuperUserTrue(t *testing.T) {
	t.Parallel()
	tid := freshTenantID(t)
	m := newMembership(t, tid)
	regular := newRoleWith(t, tid, "Sales",
		permission.FromConstant(permission.IdentityPermissions.Roles.View))
	super := newSuperAdminRole(t, tid)
	if err := m.AssignRole(regular.ID()); err != nil {
		t.Fatalf("AssignRole regular: %v", err)
	}
	if err := m.AssignRole(super.ID()); err != nil {
		t.Fatalf("AssignRole super: %v", err)
	}

	mems := &fakeMembershipRepo{memberships: map[membership.ID]*membership.Membership{m.ID(): m}}
	roles := &fakeRoleRepo{roles: map[role.ID]*role.Role{regular.ID(): regular, super.ID(): super}}
	res := permissions.NewResolver(mems, roles)

	got, err := res.ResolveAuth(t.Context(), m)
	if err != nil {
		t.Fatalf("ResolveAuth: %v", err)
	}
	if !got.IsSuperUser {
		t.Fatalf("IsSuperUser: got false, want true (super-admin role assigned)")
	}
}

func TestResolveAuth_RejectsNil(t *testing.T) {
	t.Parallel()
	res := permissions.NewResolver(&fakeMembershipRepo{}, &fakeRoleRepo{})
	_, err := res.ResolveAuth(t.Context(), nil)
	if err == nil {
		t.Fatal("ResolveAuth(nil) expected error")
	}
}
