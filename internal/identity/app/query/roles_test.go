package query_test

import (
	"context"
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/query"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role/roletest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/rolehierarchy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/rolehierarchy/rolehierarchytest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// ----- GetRoleHandler ------------------------------------------------------

func TestNewGetRoleHandler_PanicsOnNilRoles(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on nil roles")
		}
	}()
	_ = query.NewGetRoleHandler(nil, rolehierarchytest.NewFakeRepository()) // arch-test:ignore-err - test fixture setup
}

func TestNewGetRoleHandler_NilEdgesAllowed(t *testing.T) {
	t.Parallel()
	// Edges may be nil — covered by the lookupParent nil-guard. Asserts
	// the ctor does NOT panic + returns a usable handler whose Handle
	// path treats nil edges as "always root".
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("unexpected panic with nil edges: %v", r)
		}
	}()
	h := query.NewGetRoleHandler(roletest.NewFakeRepository(), nil)
	// Empty repo → ErrNotFound — proves Handle exercises the nil-edges
	// branch without crashing in lookupParent.
	_, err := h.Handle(t.Context(), query.GetRoleQuery{TenantID: testTenantID, RoleID: testRoleID})
	if !errors.Is(err, role.ErrNotFound) {
		t.Fatalf("err = %v, want role.ErrNotFound", err)
	}
}

func TestGetRole_RejectsZeroInputs(t *testing.T) {
	t.Parallel()
	h := query.NewGetRoleHandler(roletest.NewFakeRepository(), rolehierarchytest.NewFakeRepository())
	cases := []struct {
		name string
		q    query.GetRoleQuery
	}{
		{"zero tenant", query.GetRoleQuery{RoleID: testRoleID}},
		{"zero role", query.GetRoleQuery{TenantID: testTenantID}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := h.Handle(t.Context(), c.q)
			if err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestGetRole_RoleNotFound(t *testing.T) {
	t.Parallel()
	h := query.NewGetRoleHandler(roletest.NewFakeRepository(), rolehierarchytest.NewFakeRepository())
	_, err := h.Handle(t.Context(), query.GetRoleQuery{TenantID: testTenantID, RoleID: testRoleID})
	if !errors.Is(err, role.ErrNotFound) {
		t.Fatalf("err = %v, want role.ErrNotFound", err)
	}
}

func TestGetRole_HappyPath_ProjectsAllFieldsAndNoParent(t *testing.T) {
	t.Parallel()
	r := newRole(t, testRoleID, testTenantID, "Manager")
	roles := roletest.NewFakeRepository()
	if err := roles.Add(t.Context(), r); err != nil {
		t.Fatal(err)
	}

	h := query.NewGetRoleHandler(roles, rolehierarchytest.NewFakeRepository())
	got, err := h.Handle(t.Context(), query.GetRoleQuery{TenantID: testTenantID, RoleID: r.ID()})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got.ID != r.ID().String() {
		t.Errorf("ID = %q", got.ID)
	}
	if got.TenantID != testTenantID.String() {
		t.Errorf("TenantID = %q", got.TenantID)
	}
	if got.Name != "Manager" {
		t.Errorf("Name = %q", got.Name)
	}
	if got.IsSystemDefault {
		t.Errorf("IsSystemDefault = true")
	}
	if got.IsSuperAdmin {
		t.Errorf("IsSuperAdmin = true")
	}
	if got.HierarchyLevel != role.HierarchyLevelDefault {
		t.Errorf("HierarchyLevel = %d", got.HierarchyLevel)
	}
	if got.ParentRoleID != "" {
		t.Errorf("ParentRoleID = %q, want empty (no edges)", got.ParentRoleID)
	}
	if got.CreatedAt.IsZero() {
		t.Errorf("CreatedAt zero")
	}
}

func TestGetRole_PopulatesParentIDFromEdge(t *testing.T) {
	t.Parallel()
	r := newRole(t, testRoleID, testTenantID, "Manager")
	parent := newRole(t, testParentRoleID, testTenantID, "Director")
	edge := newEdge(t, testEdgeID, testTenantID, r.ID(), parent.ID())

	roles := roletest.NewFakeRepository()
	if err := roles.Add(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if err := roles.Add(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	edges := rolehierarchytest.NewFakeRepository()
	if err := edges.Add(t.Context(), edge); err != nil {
		t.Fatal(err)
	}

	h := query.NewGetRoleHandler(roles, edges)
	got, err := h.Handle(t.Context(), query.GetRoleQuery{TenantID: testTenantID, RoleID: r.ID()})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got.ParentRoleID != parent.ID().String() {
		t.Errorf("ParentRoleID = %q, want %q", got.ParentRoleID, parent.ID().String())
	}
}

func TestGetRole_NilEdgesReturnsEmptyParent(t *testing.T) {
	t.Parallel()
	r := newRole(t, testRoleID, testTenantID, "Manager")
	roles := roletest.NewFakeRepository()
	if err := roles.Add(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	h := query.NewGetRoleHandler(roles, nil)
	got, err := h.Handle(t.Context(), query.GetRoleQuery{TenantID: testTenantID, RoleID: r.ID()})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got.ParentRoleID != "" {
		t.Errorf("ParentRoleID = %q, want empty", got.ParentRoleID)
	}
}

// edgesErrRepo lets a test inject a non-ErrEdgeNotFound failure on
// GetActiveByChild.
type edgesErrRepo struct {
	rolehierarchy.Repository
	err error
}

func (r edgesErrRepo) GetActiveByChild(_ context.Context, _ tenant.ID, _ role.ID) (*rolehierarchy.Edge, error) {
	return nil, r.err
}

func TestGetRole_PropagatesEdgesError(t *testing.T) {
	t.Parallel()
	r := newRole(t, testRoleID, testTenantID, "Manager")
	roles := roletest.NewFakeRepository()
	if err := roles.Add(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("edges boom")
	edges := edgesErrRepo{Repository: rolehierarchytest.NewFakeRepository(), err: sentinel}
	h := query.NewGetRoleHandler(roles, edges)
	_, err := h.Handle(t.Context(), query.GetRoleQuery{TenantID: testTenantID, RoleID: r.ID()})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

// ----- ListRolesHandler ----------------------------------------------------

func TestNewListRolesHandler_PanicsOnNilRoles(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic on nil roles")
		}
	}()
	_ = query.NewListRolesHandler(nil, rolehierarchytest.NewFakeRepository()) // arch-test:ignore-err - test fixture setup
}

func TestNewListRolesHandler_NilEdgesAllowed(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("unexpected panic with nil edges: %v", r)
		}
	}()
	h := query.NewListRolesHandler(roletest.NewFakeRepository(), nil)
	got, err := h.Handle(t.Context(), query.ListRolesQuery{TenantID: testTenantID})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("len = %d, want 0", len(got))
	}
}

func TestListRoles_RejectsZeroTenant(t *testing.T) {
	t.Parallel()
	h := query.NewListRolesHandler(roletest.NewFakeRepository(), rolehierarchytest.NewFakeRepository())
	_, err := h.Handle(t.Context(), query.ListRolesQuery{})
	if err == nil {
		t.Fatal("expected error on zero tenant")
	}
}

// rolesListErrRepo injects failure on the ListByTenant path.
type rolesListErrRepo struct {
	role.Repository
	err error
}

func (r rolesListErrRepo) ListByTenant(_ context.Context, _ tenant.ID) ([]*role.Role, error) {
	return nil, r.err
}

func TestListRoles_PropagatesListError(t *testing.T) {
	t.Parallel()
	sentinel := errors.New("list boom")
	roles := rolesListErrRepo{Repository: roletest.NewFakeRepository(), err: sentinel}
	h := query.NewListRolesHandler(roles, rolehierarchytest.NewFakeRepository())
	_, err := h.Handle(t.Context(), query.ListRolesQuery{TenantID: testTenantID})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestListRoles_PropagatesPerRowEdgesError(t *testing.T) {
	t.Parallel()
	r := newRole(t, testRoleID, testTenantID, "Manager")
	roles := roletest.NewFakeRepository()
	if err := roles.Add(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("edges boom")
	edges := edgesErrRepo{Repository: rolehierarchytest.NewFakeRepository(), err: sentinel}
	h := query.NewListRolesHandler(roles, edges)
	_, err := h.Handle(t.Context(), query.ListRolesQuery{TenantID: testTenantID})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want sentinel", err)
	}
}

func TestListRoles_HappyPath_OrdersAndPopulatesParent(t *testing.T) {
	t.Parallel()
	child := newRole(t, testRoleID, testTenantID, "Manager")
	parent := newRole(t, testParentRoleID, testTenantID, "Director")
	edge := newEdge(t, testEdgeID, testTenantID, child.ID(), parent.ID())
	roles := roletest.NewFakeRepository()
	if err := roles.Add(t.Context(), child); err != nil {
		t.Fatal(err)
	}
	if err := roles.Add(t.Context(), parent); err != nil {
		t.Fatal(err)
	}
	edges := rolehierarchytest.NewFakeRepository()
	if err := edges.Add(t.Context(), edge); err != nil {
		t.Fatal(err)
	}
	h := query.NewListRolesHandler(roles, edges)
	got, err := h.Handle(t.Context(), query.ListRolesQuery{TenantID: testTenantID})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	// Find the manager (child) and assert it carries the parent reference.
	var mgr *query.RoleView
	for i := range got {
		if got[i].Name == "Manager" {
			mgr = &got[i]
		}
	}
	if mgr == nil {
		t.Fatal("Manager view absent")
	}
	if mgr.ParentRoleID != parent.ID().String() {
		t.Errorf("ParentRoleID = %q, want %q", mgr.ParentRoleID, parent.ID().String())
	}
}

func TestListRoles_NilEdgesReturnsViewsWithEmptyParent(t *testing.T) {
	t.Parallel()
	r := newRole(t, testRoleID, testTenantID, "Manager")
	roles := roletest.NewFakeRepository()
	if err := roles.Add(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	h := query.NewListRolesHandler(roles, nil)
	got, err := h.Handle(t.Context(), query.ListRolesQuery{TenantID: testTenantID})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1", len(got))
	}
	if got[0].ParentRoleID != "" {
		t.Errorf("ParentRoleID = %q, want empty", got[0].ParentRoleID)
	}
}
