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
	if len(specs) != 14 {
		t.Fatalf("catalog size: got %d want 14 (13 .NET roles + Team Lead, ADR 0069)", len(specs))
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
		role.SystemRoles.Tenant.TeamLead,
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
// ADR 0061 amendment 1 Inventory grant policy + the Phase C.2 Tasks
// grant policy (BRD §6.8): every tenant role gets baseline Read +
// Manage on tasks.work_items; manager-tier + OfficeAdministrator +
// CompanyOwner additionally get ReadAll + Reassign.
//
// Inventory grants:
//   - PurchaseManager → Catalog.Manage + Stock.Manage
//   - SalesManager + OfficeAdministrator → Catalog.Read + Stock.Read
//   - all others: none
//
// Tasks grants:
//   - tasksMember = Read + Manage (every non-Owner role)
//   - tasksManager = Read + ReadAll + Manage + Reassign (Administrator,
//     SeniorManager, OfficeAdministrator, SalesManager, PurchaseManager,
//     DispatchManager, HrManager — i.e. every *Manager + Administrator +
//     OfficeAdministrator)
func TestDefaultRoleCatalog_OperationalRolesCarryInventoryGrants(t *testing.T) {
	t.Parallel()
	specs := seed.DefaultRoleCatalog()
	p := permission.IdentityPermissions
	tasksMember := []string{p.Tasks.WorkItems.Read, p.Tasks.WorkItems.Manage}
	tasksManager := []string{
		p.Tasks.WorkItems.Read, p.Tasks.WorkItems.ReadAll,
		p.Tasks.WorkItems.Manage, p.Tasks.WorkItems.Reassign,
	}
	dispatchOps := []string{p.Dispatch.ConsignmentNotes.Read, p.Dispatch.ConsignmentNotes.Manage}
	ordersFull := []string{
		p.Orders.Quotations.Read, p.Orders.Quotations.Manage,
		p.Orders.Orders.Read, p.Orders.Orders.Manage,
		p.Orders.Payments.Read, p.Orders.Payments.Record,
		p.Orders.Invoices.Read,
	}
	ordersExec := []string{
		p.Orders.Quotations.Read, p.Orders.Quotations.Manage,
		p.Orders.Orders.Read,
		p.Orders.Payments.Read, p.Orders.Payments.Record,
		p.Orders.Invoices.Read,
	}
	ordersRead := []string{
		p.Orders.Quotations.Read, p.Orders.Orders.Read,
		p.Orders.Payments.Read, p.Orders.Invoices.Read,
	}
	elevation := []string{p.Meta.RequestPermissionElevation}
	directory := []string{p.Users.View, p.Roles.View}
	approver := []string{p.Roles.Approve}
	crmManager := []string{p.Crm.Leads.Read, p.Crm.Leads.ReadAll, p.Crm.Leads.Assign, p.Crm.Leads.Manage}
	crmExec := []string{p.Crm.Leads.Read, p.Crm.Leads.Manage}
	invRead := []string{p.Inventory.Catalog.Read, p.Inventory.Stock.Read}
	want := map[string][]string{
		role.SystemRoles.Tenant.Administrator:       slices.Concat([]string{p.Meta.TenantAdmin}, tasksManager),
		role.SystemRoles.Tenant.OfficeAdministrator: slices.Concat([]string{p.Meta.TenantAdmin}, invRead, tasksManager, ordersRead),
		role.SystemRoles.Tenant.SeniorManager: slices.Concat(tasksManager, directory, approver, crmManager, invRead,
			[]string{p.Dispatch.ConsignmentNotes.Read}, ordersRead),
		role.SystemRoles.Tenant.OfficeExecutive: tasksMember,
		role.SystemRoles.Tenant.SalesManager:    slices.Concat(invRead, tasksManager, ordersFull, directory, approver, crmManager),
		role.SystemRoles.Tenant.SalesExecutive:  slices.Concat(tasksMember, ordersExec, crmExec),
		role.SystemRoles.Tenant.PurchaseManager: slices.Concat([]string{p.Inventory.Catalog.Manage, p.Inventory.Stock.Manage},
			tasksManager, directory, approver),
		role.SystemRoles.Tenant.PurchaseExecutive: tasksMember,
		role.SystemRoles.Tenant.DispatchManager: slices.Concat(tasksManager, dispatchOps, directory, approver,
			[]string{p.Orders.Orders.Read, p.Inventory.Stock.Read}),
		role.SystemRoles.Tenant.DispatchExecutive: slices.Concat(tasksMember, dispatchOps),
		role.SystemRoles.Tenant.HrManager:         slices.Concat(tasksManager, directory, approver),
		role.SystemRoles.Tenant.HrExecutive:       tasksMember,
		role.SystemRoles.Tenant.TeamLead: slices.Concat(directory, approver, crmExec,
			[]string{p.Tasks.WorkItems.Read, p.Tasks.WorkItems.Manage, p.Tasks.WorkItems.Reassign}),
	}
	// Every tenant role additionally carries the self-service notification
	// inbox bundle, and every non-owner role may request elevation. The
	// comparison is as a set: order and duplicates carry no meaning.
	notificationsInbox := []string{p.Notifications.Inbox.Read, p.Notifications.Inbox.Manage}
	asSet := func(xs []string) []string {
		out := slices.Clone(xs)
		slices.Sort(out)
		return slices.Compact(out)
	}
	for name, perms := range want {
		want[name] = asSet(slices.Concat(perms, notificationsInbox, elevation))
	}
	for _, s := range specs {
		if s.Name == role.SystemRoles.Tenant.CompanyOwner {
			continue
		}
		if s.IsSystemDefault {
			t.Errorf("non-Owner role %q is system-default — only CompanyOwner should be", s.Name)
		}
		wantPerms, ok := want[s.Name]
		if !ok {
			t.Errorf("role %q has no expected-permission entry — update test if catalog grew", s.Name)
			continue
		}
		if got := asSet(s.Permissions); !slices.Equal(got, wantPerms) {
			t.Errorf("role %q permissions:\n  got  %v\n  want %v", s.Name, got, wantPerms)
		}
	}
}

func TestDefaultRoleCatalog_TierLevels(t *testing.T) {
	t.Parallel()
	r := role.SystemRoles.Tenant
	want := map[string]int{
		r.CompanyOwner: 0, r.Administrator: 10, r.OfficeAdministrator: 10, r.SeniorManager: 15,
		r.SalesManager: 20, r.PurchaseManager: 20, r.DispatchManager: 20, r.HrManager: 20, r.TeamLead: 30,
		r.SalesExecutive: 50, r.PurchaseExecutive: 50, r.DispatchExecutive: 50, r.HrExecutive: 50, r.OfficeExecutive: 50,
	}
	specs := seed.DefaultRoleCatalog()
	if len(specs) != len(want) {
		t.Fatalf("catalog has %d roles, want %d", len(specs), len(want))
	}
	for _, s := range specs {
		lvl, ok := want[s.Name]
		if !ok {
			t.Errorf("unexpected role %q", s.Name)
			continue
		}
		if s.HierarchyLevel != lvl {
			t.Errorf("%s level = %d, want %d", s.Name, s.HierarchyLevel, lvl)
		}
	}
}

func TestDefaultRoleCatalog_DailyWorkGrants(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	r := role.SystemRoles.Tenant
	must := map[string][]string{
		r.Administrator:  {p.Meta.TenantAdmin, p.Meta.RequestPermissionElevation},
		r.SalesManager:   {p.Users.View, p.Roles.View, p.Roles.Approve, p.Crm.Leads.ReadAll, p.Crm.Leads.Assign, p.Crm.Leads.Manage},
		r.TeamLead:       {p.Users.View, p.Roles.View, p.Roles.Approve, p.Crm.Leads.Read, p.Crm.Leads.Manage, p.Tasks.WorkItems.Reassign},
		r.SalesExecutive: {p.Crm.Leads.Read, p.Crm.Leads.Manage, p.Meta.RequestPermissionElevation},
		r.HrManager:      {p.Roles.Approve},
	}
	mustNot := map[string][]string{
		r.TeamLead:       {p.Tasks.WorkItems.ReadAll, p.Crm.Leads.ReadAll, p.Crm.Leads.Assign},
		r.SalesExecutive: {p.Users.View, p.Roles.Approve},
		r.CompanyOwner:   {p.Roles.Approve},
		// Until PUT /users/{id}/manager is authority-gated (Task A4) an HR
		// Manager holding users.update could appoint themselves manager of anyone.
		r.HrManager: {p.Users.Update},
	}
	for _, s := range seed.DefaultRoleCatalog() {
		for _, n := range must[s.Name] {
			if !slices.Contains(s.Permissions, n) {
				t.Errorf("%s missing %s", s.Name, n)
			}
		}
		for _, n := range mustNot[s.Name] {
			if slices.Contains(s.Permissions, n) {
				t.Errorf("%s must not hold %s", s.Name, n)
			}
		}
	}
}

// Every non-owner role can ask for elevation (the catalogue godoc says every
// regular user holds it); the owner's tenant.admin already implies it.
func TestDefaultRoleCatalog_EveryNonOwnerCanRequestElevation(t *testing.T) {
	t.Parallel()
	elevation := permission.IdentityPermissions.Meta.RequestPermissionElevation
	for _, s := range seed.DefaultRoleCatalog() {
		held := slices.Contains(s.Permissions, elevation)
		if s.Name == role.SystemRoles.Tenant.CompanyOwner {
			if held {
				t.Errorf("%s carries %s redundantly", s.Name, elevation)
			}
			continue
		}
		if !held {
			t.Errorf("%s missing %s", s.Name, elevation)
		}
	}
}

// Nothing in the seed holds a platform-reserved permission (tenant.admin, which
// defines the Admin tier, is the one reserved name roles carry), and every name is in the catalogue.
func TestDefaultRoleCatalog_OnlyKnownTenantPermissions(t *testing.T) {
	t.Parallel()
	for _, s := range seed.DefaultRoleCatalog() {
		for _, n := range s.Permissions {
			if !permission.IsKnown(n) || (permission.IsPlatformReserved(n) && n != permission.IdentityPermissions.Meta.TenantAdmin) {
				t.Errorf("%s holds %q (unknown or platform-reserved)", s.Name, n)
			}
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

func (f *fakeRoleRepo) UpdateByID(_ context.Context, _ tenant.ID, _ role.ID, _ func(*role.Role) (bool, error)) error {
	return errors.New("fake: UpdateByID not used by ApplyDefaultRoles tests")
}

func (f *fakeRoleRepo) GetByID(_ context.Context, tenantID tenant.ID, id role.ID) (*role.Role, error) {
	r, ok := f.roles[id]
	if !ok || r.IsDeleted() || r.TenantID() != tenantID {
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

func (f *fakeRoleRepo) GetByIDs(_ context.Context, tenantID tenant.ID, ids []role.ID) ([]*role.Role, error) {
	out := make([]*role.Role, 0, len(ids))
	for _, id := range ids {
		if r, ok := f.roles[id]; ok && !r.IsDeleted() && r.TenantID() == tenantID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeRoleRepo) GetByIDsIncludingDeleted(_ context.Context, tenantID tenant.ID, ids []role.ID) ([]*role.Role, error) {
	out := make([]*role.Role, 0, len(ids))
	for _, id := range ids {
		if r, ok := f.roles[id]; ok && r.TenantID() == tenantID {
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
	// CompanyOwner must carry Meta.TenantAdmin + the full Tasks
	// manager bundle (BRD §6.8) after apply.
	for _, r := range roles {
		if r.Name() != role.SystemRoles.Tenant.CompanyOwner {
			continue
		}
		names := make([]string, 0, len(r.Permissions()))
		for _, p := range r.Permissions() {
			names = append(names, p.Name())
		}
		mustInclude := []string{
			permission.IdentityPermissions.Meta.TenantAdmin,
			permission.IdentityPermissions.Tasks.WorkItems.Read,
			permission.IdentityPermissions.Tasks.WorkItems.ReadAll,
			permission.IdentityPermissions.Tasks.WorkItems.Manage,
			permission.IdentityPermissions.Tasks.WorkItems.Reassign,
		}
		for _, want := range mustInclude {
			if !slices.Contains(names, want) {
				t.Fatalf("CompanyOwner missing %q after apply: got %v", want, names)
			}
		}
		return
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
