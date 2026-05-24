package seed_test

import (
	"time"

	"context"
	"errors"
	"slices"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/app/seed"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// testNow is the deterministic instant test fixtures pass to domain
// factories + mutators per the clock-injection refactor.
var testNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

func TestDefaultRoleCatalog_ContainsExpectedNames(t *testing.T) {
	t.Parallel()
	specs := seed.DefaultRoleCatalog()
	if len(specs) != 13 {
		t.Fatalf("catalog size: got %d want 13 (mirrors .NET parent)", len(specs))
	}
	want := []string{
		role.SystemRoles.Tenant.CompanyOwner,
		role.SystemRoles.Tenant.Administrator,
		role.SystemRoles.Tenant.SeniorManager,
		role.SystemRoles.Tenant.OfficeAdministrator,
		role.SystemRoles.Tenant.OfficeExecutive,
		role.SystemRoles.Tenant.SalesManager,
		role.SystemRoles.Tenant.SalesExecutive,
		role.SystemRoles.Tenant.PurchaseManager,
		role.SystemRoles.Tenant.PurchaseExecutive,
		role.SystemRoles.Tenant.DispatchManager,
		role.SystemRoles.Tenant.DispatchExecutive,
		role.SystemRoles.Tenant.HrManager,
		role.SystemRoles.Tenant.HrExecutive,
	}
	for _, w := range want {
		if !slices.ContainsFunc(specs, func(s seed.RoleSpec) bool { return s.Name == w }) {
			t.Fatalf("catalog missing %q", w)
		}
	}
}

func TestDefaultRoleCatalog_CompanyOwnerCarriesTenantAdmin(t *testing.T) {
	t.Parallel()
	specs := seed.DefaultRoleCatalog()
	idx := slices.IndexFunc(specs, func(s seed.RoleSpec) bool {
		return s.Name == role.SystemRoles.Tenant.CompanyOwner
	})
	if idx < 0 {
		t.Fatal("CompanyOwner spec missing")
	}
	owner := specs[idx]
	if !owner.IsSystemDefault {
		t.Fatalf("CompanyOwner.IsSystemDefault: got false want true")
	}
	if owner.HierarchyLevel != 0 {
		t.Fatalf("CompanyOwner.HierarchyLevel: got %d want 0", owner.HierarchyLevel)
	}
	if !slices.Contains(owner.Permissions, permission.IdentityPermissions.Meta.TenantAdmin) {
		t.Fatalf("CompanyOwner missing Meta.TenantAdmin permission: got %v", owner.Permissions)
	}
}

// TestDefaultRoleCatalog_OperationalRolesCarryInventoryGrants pins the
// ADR 0061 amendment 1 grant policy: PurchaseManager gets Inventory
// Manage, SalesManager + OfficeAdministrator get Inventory Read.
// Non-operational roles (Executives, HR, Dispatch, the top-tier
// Administrator + SeniorManager) ship empty placeholders for product
// UX to populate per-membership.
func TestDefaultRoleCatalog_OperationalRolesCarryInventoryGrants(t *testing.T) {
	t.Parallel()
	specs := seed.DefaultRoleCatalog()
	want := map[string][]string{
		role.SystemRoles.Tenant.PurchaseManager: {
			permission.IdentityPermissions.Inventory.Catalog.Manage,
			permission.IdentityPermissions.Inventory.Stock.Manage,
		},
		role.SystemRoles.Tenant.SalesManager: {
			permission.IdentityPermissions.Inventory.Catalog.Read,
			permission.IdentityPermissions.Inventory.Stock.Read,
		},
		role.SystemRoles.Tenant.OfficeAdministrator: {
			permission.IdentityPermissions.Inventory.Catalog.Read,
			permission.IdentityPermissions.Inventory.Stock.Read,
		},
	}
	for _, s := range specs {
		if s.Name == role.SystemRoles.Tenant.CompanyOwner {
			continue
		}
		if s.IsSystemDefault {
			t.Errorf("non-Owner role %q is system-default — only CompanyOwner should be", s.Name)
		}
		if wantPerms, ok := want[s.Name]; ok {
			if !slices.Equal(s.Permissions, wantPerms) {
				t.Errorf("role %q permissions: got %v want %v", s.Name, s.Permissions, wantPerms)
			}
			continue
		}
		if len(s.Permissions) != 0 {
			t.Errorf("non-operational role %q ships with permissions: %v "+
				"(only PurchaseManager/SalesManager/OfficeAdministrator carry inventory grants per ADR 0061)",
				s.Name, s.Permissions)
		}
	}
}

// ----- ApplyDefaultRoles — pure-Go fake repo -------------------------------

// fakeRoleRepo is a minimal in-memory role.Repository for catalog-apply
// tests. Tests Apply's idempotency + permission grant + ID minting
// without spinning Postgres. Adapter integration tests live in
// adapters/role_repository_pg_test.go.
type fakeRoleRepo struct {
	roles map[role.ID]*role.Role
}

func newFakeRoleRepo() *fakeRoleRepo {
	return &fakeRoleRepo{roles: map[role.ID]*role.Role{}}
}

func (f *fakeRoleRepo) Add(_ context.Context, r *role.Role) error {
	for _, existing := range f.roles {
		if existing.TenantID() == r.TenantID() && existing.Name() == r.Name() && !existing.IsDeleted() {
			return role.ErrNameTaken
		}
	}
	// Drain events so subsequent Apply calls don't re-emit them.
	_ = r.PullEvents()
	f.roles[r.ID()] = r
	return nil
}

func (f *fakeRoleRepo) UpdateByID(_ context.Context, _ role.ID, _ func(*role.Role) (bool, error)) error {
	return errors.New("fake: UpdateByID not used by ApplyDefaultRoles tests")
}

func (f *fakeRoleRepo) GetByID(_ context.Context, id role.ID) (*role.Role, error) {
	r, ok := f.roles[id]
	if !ok || r.IsDeleted() {
		return nil, role.ErrNotFound
	}
	return r, nil
}

func (f *fakeRoleRepo) GetByTenantAndName(
	_ context.Context, tenantID tenant.ID, name string,
) (*role.Role, error) {
	for _, r := range f.roles {
		if r.TenantID() == tenantID && r.Name() == name && !r.IsDeleted() {
			return r, nil
		}
	}
	return nil, role.ErrNotFound
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

func (f *fakeRoleRepo) ListByTenant(_ context.Context, tenantID tenant.ID) ([]*role.Role, error) {
	out := []*role.Role{}
	for _, r := range f.roles {
		if r.TenantID() == tenantID && !r.IsDeleted() {
			out = append(out, r)
		}
	}
	return out, nil
}

func freshTenantID(t *testing.T) tenant.ID {
	t.Helper()
	return tenant.ID(ids.NewV7().String())
}

func TestApplyDefaultRoles_FreshTenant_CreatesAllSpecs(t *testing.T) {
	t.Parallel()
	repo := newFakeRoleRepo()
	tid := freshTenantID(t)

	roles, err := seed.ApplyDefaultRoles(t.Context(), repo, tid, testNow)
	if err != nil {
		t.Fatalf("ApplyDefaultRoles: %v", err)
	}
	if len(roles) != len(seed.DefaultRoleCatalog()) {
		t.Fatalf("returned roles: got %d want %d",
			len(roles), len(seed.DefaultRoleCatalog()))
	}
	listed, _ := repo.ListByTenant(t.Context(), tid)
	if len(listed) != len(roles) {
		t.Fatalf("repo state: got %d want %d", len(listed), len(roles))
	}
	// CompanyOwner must carry Meta.TenantAdmin after apply.
	for _, r := range roles {
		if r.Name() == role.SystemRoles.Tenant.CompanyOwner {
			if len(r.Permissions()) != 1 ||
				r.Permissions()[0].Name() != permission.IdentityPermissions.Meta.TenantAdmin {
				t.Fatalf("CompanyOwner permissions after apply: got %v", r.Permissions())
			}
			return
		}
	}
	t.Fatal("CompanyOwner not in apply result")
}

func TestApplyDefaultRoles_Idempotent_SecondCallNoOps(t *testing.T) {
	t.Parallel()
	repo := newFakeRoleRepo()
	tid := freshTenantID(t)

	first, err := seed.ApplyDefaultRoles(t.Context(), repo, tid, testNow)
	if err != nil {
		t.Fatalf("first apply: %v", err)
	}
	second, err := seed.ApplyDefaultRoles(t.Context(), repo, tid, testNow)
	if err != nil {
		t.Fatalf("second apply (idempotent): %v", err)
	}
	if len(first) != len(second) {
		t.Fatalf("idempotency: first %d second %d", len(first), len(second))
	}
	listed, _ := repo.ListByTenant(t.Context(), tid)
	if len(listed) != len(seed.DefaultRoleCatalog()) {
		t.Fatalf("repo grew on second apply: got %d want %d",
			len(listed), len(seed.DefaultRoleCatalog()))
	}
}

func TestApplyDefaultRoles_RejectsZeroTenantID(t *testing.T) {
	t.Parallel()
	repo := newFakeRoleRepo()
	if _, err := seed.ApplyDefaultRoles(t.Context(), repo, tenant.ID(""), testNow); err == nil {
		t.Fatal("ApplyDefaultRoles with zero tenantID: expected error")
	}
}
