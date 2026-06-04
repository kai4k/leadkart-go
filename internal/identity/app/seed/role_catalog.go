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
// CompanyOwner carries the `Meta.TenantAdmin` bundle as the "this
// person is the tenant admin" short-circuit. Per ADR 0061 amendment 1
// (Inventory Slice 1 fix-pass), the operational tenant-tier roles
// (PurchaseManager / SalesManager / OfficeAdministrator) ALSO ship with
// the Inventory permissions their workflow implies — without those
// grants every non-Owner membership received 403 on the 11 inventory
// endpoints. The grant policy:
//
//   - PurchaseManager       → Catalog.Manage + Stock.Manage (full inventory write)
//   - SalesManager          → Catalog.Read + Stock.Read (read-only for order context)
//   - OfficeAdministrator   → Catalog.Read + Stock.Read (read-only for back-office)
//
// Other tenant-tier roles (Executives, HR, Dispatch) ship empty —
// product UX + admins populate per-membership overlays when their
// workflow actually needs inventory access. As new modules add their
// permission catalogues this pattern extends per the same rule of
// thumb (managers in the relevant chain get Manage; cross-chain
// admin-tier gets Read).
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

	return []RoleSpec{
		{
			Name:            role.SystemRoles.Tenant.CompanyOwner,
			IsSystemDefault: true,
			HierarchyLevel:  0,
			Permissions:     append([]string{p.Meta.TenantAdmin}, tasksManager...),
		},
		{
			Name:           role.SystemRoles.Tenant.Administrator,
			HierarchyLevel: 10,
			Permissions:    tasksManager,
		},
		{
			Name:           role.SystemRoles.Tenant.SeniorManager,
			HierarchyLevel: 20,
			Permissions:    tasksManager,
		},
		{
			Name:           role.SystemRoles.Tenant.OfficeAdministrator,
			HierarchyLevel: role.HierarchyLevelDefault,
			Permissions: append([]string{
				p.Inventory.Catalog.Read,
				p.Inventory.Stock.Read,
			}, tasksManager...),
		},
		{
			Name:           role.SystemRoles.Tenant.OfficeExecutive,
			HierarchyLevel: role.HierarchyLevelDefault,
			Permissions:    tasksMember,
		},
		{
			Name:           role.SystemRoles.Tenant.SalesManager,
			HierarchyLevel: role.HierarchyLevelDefault,
			Permissions: append([]string{
				p.Inventory.Catalog.Read,
				p.Inventory.Stock.Read,
			}, tasksManager...),
		},
		{
			Name:           role.SystemRoles.Tenant.SalesExecutive,
			HierarchyLevel: role.HierarchyLevelDefault,
			Permissions:    tasksMember,
		},
		{
			Name:           role.SystemRoles.Tenant.PurchaseManager,
			HierarchyLevel: role.HierarchyLevelDefault,
			Permissions: append([]string{
				p.Inventory.Catalog.Manage,
				p.Inventory.Stock.Manage,
			}, tasksManager...),
		},
		{
			Name:           role.SystemRoles.Tenant.PurchaseExecutive,
			HierarchyLevel: role.HierarchyLevelDefault,
			Permissions:    tasksMember,
		},
		{
			Name:           role.SystemRoles.Tenant.DispatchManager,
			HierarchyLevel: role.HierarchyLevelDefault,
			Permissions:    tasksManager,
		},
		{
			Name:           role.SystemRoles.Tenant.DispatchExecutive,
			HierarchyLevel: role.HierarchyLevelDefault,
			Permissions:    tasksMember,
		},
		{
			Name:           role.SystemRoles.Tenant.HrManager,
			HierarchyLevel: role.HierarchyLevelDefault,
			Permissions:    tasksManager,
		},
		{
			Name:           role.SystemRoles.Tenant.HrExecutive,
			HierarchyLevel: role.HierarchyLevelDefault,
			Permissions:    tasksMember,
		},
	}
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
