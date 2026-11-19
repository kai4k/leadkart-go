// Package seed defines the canonical Identity-tier seed data + the
// idempotent Apply functions tenant-onboarding (and admin tooling)
// use to project that data onto a fresh tenant.
//
// `DefaultRoleCatalog` mirrors the .NET parent's
// Application.Tenants.Services.DefaultRoleCatalog: a fixed list of
// (Name, IsSystemDefault, HierarchyLevel, Permissions) tuples. The
// list is deliberately small in v0.2 — most tenant roles ship as
// empty placeholders for product UX to populate later. Only
// CompanyOwner carries a permission grant (Meta.TenantAdmin) so the
// first user a tenant admin onboards has full operational authority.
package seed

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// RoleSpec is the wire-stable shape of one entry in
// [DefaultRoleCatalog]. Materialised into a [role.Role] aggregate at
// seed-apply time via [role.New] + per-permission [role.Role.GrantPermission].
//
// Name is the user-facing display string (matches admin UI labels +
// integration-event payloads). HierarchyLevel uses the same lower-=-higher
// authority semantics as `role.HierarchyLevel*` constants.
type RoleSpec struct {
	Name            string
	IsSystemDefault bool
	IsSuperAdmin    bool
	HierarchyLevel  int
	Permissions     []string
}

// DefaultRoleCatalog returns the canonical default Tenant-tier roles
// every fresh tenant receives at onboarding. Mirror of the .NET
// parent's `DefaultRoleCatalog.Roles` — keep names + hierarchy-levels
// in lockstep.
//
// Returns a fresh slice on each call (callers may mutate without
// worrying about poisoning a shared package var).
//
// Rank ladder (ADR 0069 H1; BRD §6.7): Owner 0, Administrator and Office
// Administrator 10, Senior Manager 15, the department managers 20, Team Lead
// 30, every executive 50. Lower = more senior; a role only ever acts on roles
// below it.
//
// Grant policy (ADR 0069 H5; BRD §3.2 and §6.7):
//
//   - CompanyOwner         → tenant.admin + the Tasks, Dispatch and Orders bundles
//   - Administrator, OfficeAdministrator → tenant.admin (new tenants; existing
//     tenants opt in, ADR 0069 Q2) + the Tasks manager bundle; OfficeAdministrator
//     also reads inventory and orders
//   - managers (Senior, Sales, Purchase, Dispatch, HR) and Team Lead → the
//     directory views (users.view, roles.view) and roles.approve, so a manager
//     can see the people and roles they approve for; plus their daily work:
//     Senior and Sales managers get the full CRM lead set, Purchase the
//     inventory write set (ADR 0061 amendment 1), Dispatch the consignment and
//     order-read set, HR the directory and approval set plus identity.users.update
//     (reporting lines, rank-gated by the authority policy), Team Lead
//     crm.leads.{read,manage} and the tasks it hands out
//   - Sales Executive      → crm.leads.{read,manage} on top of quotes and orders
//   - every non-owner role → identity.meta.request_permission_elevation
//   - Team Lead deliberately holds no read_all: today read_all is tenant-wide
//
// Returns a fresh slice on each call (callers may mutate without
// worrying about poisoning a shared package var).
func DefaultRoleCatalog() []RoleSpec {
	p := permission.IdentityPermissions

	// Tasks permission policy per BRD §6.8 + §6.7 visibility:
	//   - Every tenant role gets Read + Manage (own tasks — create + complete
	//     + cancel — is a baseline workflow capability).
	//   - Manager-tier roles (SalesManager / PurchaseManager / DispatchManager /
	//     HrManager) + OfficeAdministrator + CompanyOwner additionally get
	//     ReadAll + Reassign (team-wide visibility + hierarchy-gated
	//     reassignment per §6.7 visibility rule).
	//   - Administrator / SeniorManager are above the operational tier and
	//     inherit the same manager-grade Tasks bundle.
	tasksMember := []string{p.Tasks.WorkItems.Read, p.Tasks.WorkItems.Manage}
	tasksManager := []string{
		p.Tasks.WorkItems.Read, p.Tasks.WorkItems.ReadAll,
		p.Tasks.WorkItems.Manage, p.Tasks.WorkItems.Reassign,
	}

	// Dispatch permission policy per BRD §6.6: the Dispatch roles own the
	// ConsignmentNote workflow (create slots + drive carrier status). Read
	// + Manage both go to DispatchManager + DispatchExecutive; CompanyOwner
	// carries the bundle for tenant-wide oversight.
	dispatchOps := []string{p.Dispatch.ConsignmentNotes.Read, p.Dispatch.ConsignmentNotes.Manage}

	// Orders permission policy per BRD §6.4: sales roles own the quote→order
	// flow. ordersFull (managers + owner) drives the full lifecycle; ordersExec
	// (sales executives) can quote + record payments + view; ordersRead
	// (office back-office) is view-only.
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

	// Notifications inbox is self-service: every tenant role can read +
	// manage its OWN notifications (recipient scoping is enforced in the
	// query, not by role). Folded into every spec below via the post-loop.
	notificationsInbox := []string{p.Notifications.Inbox.Read, p.Notifications.Inbox.Manage}

	// Rank-tier and daily-work bundles (ADR 0069 H1/H5; BRD §3.2, §6.7).
	elevation := []string{p.Meta.RequestPermissionElevation}
	directory := []string{p.Users.View, p.Roles.View}
	approver := []string{p.Roles.Approve}
	crmManager := []string{p.Crm.Leads.Read, p.Crm.Leads.ReadAll, p.Crm.Leads.Assign, p.Crm.Leads.Manage}
	crmExec := []string{p.Crm.Leads.Read, p.Crm.Leads.Manage}
	invRead := []string{p.Inventory.Catalog.Read, p.Inventory.Stock.Read}

	specs := []RoleSpec{
		{
			Name:            role.SystemRoles.Tenant.CompanyOwner,
			IsSystemDefault: true,
			HierarchyLevel:  role.HierarchyLevelOwner,
			Permissions:     slices.Concat([]string{p.Meta.TenantAdmin}, tasksManager, dispatchOps, ordersFull),
		},
		{
			Name:           role.SystemRoles.Tenant.Administrator,
			HierarchyLevel: role.HierarchyLevelAdmin,
			Permissions:    slices.Concat([]string{p.Meta.TenantAdmin}, tasksManager),
		},
		{
			Name:           role.SystemRoles.Tenant.OfficeAdministrator,
			HierarchyLevel: role.HierarchyLevelAdmin,
			Permissions:    slices.Concat([]string{p.Meta.TenantAdmin}, invRead, tasksManager, ordersRead),
		},
		{
			Name:           role.SystemRoles.Tenant.SeniorManager,
			HierarchyLevel: role.HierarchyLevelSeniorManager,
			Permissions: slices.Concat(tasksManager, directory, approver, crmManager, invRead,
				[]string{p.Dispatch.ConsignmentNotes.Read}, ordersRead),
		},
		{
			Name:           role.SystemRoles.Tenant.SalesManager,
			HierarchyLevel: role.HierarchyLevelManager,
			Permissions:    slices.Concat(invRead, tasksManager, ordersFull, directory, approver, crmManager),
		},
		{
			Name:           role.SystemRoles.Tenant.PurchaseManager,
			HierarchyLevel: role.HierarchyLevelManager,
			Permissions: slices.Concat([]string{p.Inventory.Catalog.Manage, p.Inventory.Stock.Manage},
				tasksManager, directory, approver),
		},
		{
			Name:           role.SystemRoles.Tenant.DispatchManager,
			HierarchyLevel: role.HierarchyLevelManager,
			Permissions: slices.Concat(tasksManager, dispatchOps, directory, approver,
				[]string{p.Orders.Orders.Read, p.Inventory.Stock.Read}),
		},
		{
			Name:           role.SystemRoles.Tenant.HrManager,
			HierarchyLevel: role.HierarchyLevelManager,
			// identity.users.update (reporting lines) gates PUT/DELETE /users/{id}/manager.
			// Those commands decide through the authority policy (ADR 0069): an HR
			// Manager can link or unlink only members it outranks, and cannot appoint
			// itself over a peer or senior or take a report from an equal-rank manager.
			Permissions: slices.Concat(tasksManager, directory, approver, []string{p.Users.Update}),
		},
		{
			Name:           role.SystemRoles.Tenant.TeamLead,
			HierarchyLevel: role.HierarchyLevelTeamLead,
			Permissions: slices.Concat(directory, approver, crmExec,
				[]string{p.Tasks.WorkItems.Read, p.Tasks.WorkItems.Manage, p.Tasks.WorkItems.Reassign}),
		},
		{
			Name:           role.SystemRoles.Tenant.OfficeExecutive,
			HierarchyLevel: role.HierarchyLevelDefault,
			Permissions:    tasksMember,
		},
		{
			Name:           role.SystemRoles.Tenant.SalesExecutive,
			HierarchyLevel: role.HierarchyLevelDefault,
			Permissions:    slices.Concat(tasksMember, ordersExec, crmExec),
		},
		{
			Name:           role.SystemRoles.Tenant.PurchaseExecutive,
			HierarchyLevel: role.HierarchyLevelDefault,
			Permissions:    tasksMember,
		},
		{
			Name:           role.SystemRoles.Tenant.DispatchExecutive,
			HierarchyLevel: role.HierarchyLevelDefault,
			Permissions:    slices.Concat(tasksMember, dispatchOps),
		},
		{
			Name:           role.SystemRoles.Tenant.HrExecutive,
			HierarchyLevel: role.HierarchyLevelDefault,
			Permissions:    tasksMember,
		},
	}

	// Every tenant role gets the self-service notification inbox. Every role
	// except the owner may also request a permission elevation (the
	// permission catalogue says every regular user holds it); the owner's
	// tenant.admin already implies it.
	for i := range specs {
		specs[i].Permissions = slices.Concat(specs[i].Permissions, notificationsInbox)
		if specs[i].Name != role.SystemRoles.Tenant.CompanyOwner {
			specs[i].Permissions = slices.Concat(specs[i].Permissions, elevation)
		}
	}
	return specs
}

// ApplyDefaultRoles materialises the [DefaultRoleCatalog] entries for
// the supplied tenantID. Idempotent — re-running against an already-
// seeded tenant skips existing roles (lookup by GetByTenantAndName)
// without error. Returns the list of Role aggregates created OR
// already present, in catalog order.
//
// Caller is responsible for the surrounding tenant-context: the
// repo's Add path runs under TxScopeTenant so ctx MUST carry
// tenancy.WithID(tenantID) before invocation. Tenant onboarding
// (Task 20) calls this from inside its single-tx orchestrator under
// platform context — RoleRepository's per-call WithinTx opens its
// own nested-style tx but the GUC carry forward keeps writes routed
// to the right tenant via INSERT WITH CHECK.
//
// CompanyOwner's Meta.TenantAdmin permission is granted via
// Role.GrantPermission BEFORE Add — emits PermissionGrantedEvent
// alongside the CreatedEvent on the same drain.
func ApplyDefaultRoles(
	ctx context.Context,
	repo role.Repository,
	tenantID tenant.ID,
	now time.Time,
) ([]*role.Role, error) {
	if tenantID.IsZero() {
		return nil, errors.New("seed: tenantID required")
	}
	specs := DefaultRoleCatalog()
	out := make([]*role.Role, 0, len(specs))
	for _, spec := range specs {
		existing, err := repo.GetByTenantAndName(ctx, tenantID, spec.Name)
		switch {
		case err == nil:
			// Already seeded — skip without error (idempotency).
			out = append(out, existing)
			continue
		case errors.Is(err, role.ErrNotFound):
			// expected — fall through to create
		default:
			return nil, fmt.Errorf("seed: lookup %q: %w", spec.Name, err)
		}

		r, err := role.New(
			role.ID(ids.NewV7().String()),
			tenantID,
			spec.Name,
			spec.IsSystemDefault,
			spec.HierarchyLevel,
			spec.IsSuperAdmin,
			now,
		)
		if err != nil {
			return nil, fmt.Errorf("seed: build role %q: %w", spec.Name, err)
		}
		for _, pname := range spec.Permissions {
			p, perr := permission.TryFromConstant(pname)
			if perr != nil {
				return nil, fmt.Errorf("seed: unknown permission %q in spec %q: %w",
					pname, spec.Name, perr)
			}
			if err := r.GrantPermission(p, now); err != nil {
				return nil, fmt.Errorf("seed: grant %q on %q: %w", pname, spec.Name, err)
			}
		}
		if err := repo.Add(ctx, r); err != nil {
			return nil, fmt.Errorf("seed: add role %q: %w", spec.Name, err)
		}
		out = append(out, r)
	}
	return out, nil
}
