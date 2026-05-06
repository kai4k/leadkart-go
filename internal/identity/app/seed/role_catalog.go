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
// CompanyOwner is the only role with a permission grant out of the
// box — Meta.TenantAdmin acts as the "this person is the tenant
// admin" bundle. Other tenant-tier roles ship empty; product UX +
// admins populate them with the per-module permissions their workflow
// actually needs.
func DefaultRoleCatalog() []RoleSpec {
	return []RoleSpec{
		{
			Name:            role.SystemRoles.Tenant.CompanyOwner,
			IsSystemDefault: true,
			HierarchyLevel:  0,
			Permissions:     []string{permission.IdentityPermissions.Meta.TenantAdmin},
		},
		{Name: role.SystemRoles.Tenant.Administrator, HierarchyLevel: 10},
		{Name: role.SystemRoles.Tenant.SeniorManager, HierarchyLevel: 20},
		{Name: role.SystemRoles.Tenant.OfficeAdministrator, HierarchyLevel: role.HierarchyLevelDefault},
		{Name: role.SystemRoles.Tenant.OfficeExecutive, HierarchyLevel: role.HierarchyLevelDefault},
		{Name: role.SystemRoles.Tenant.SalesManager, HierarchyLevel: role.HierarchyLevelDefault},
		{Name: role.SystemRoles.Tenant.SalesExecutive, HierarchyLevel: role.HierarchyLevelDefault},
		{Name: role.SystemRoles.Tenant.PurchaseManager, HierarchyLevel: role.HierarchyLevelDefault},
		{Name: role.SystemRoles.Tenant.PurchaseExecutive, HierarchyLevel: role.HierarchyLevelDefault},
		{Name: role.SystemRoles.Tenant.DispatchManager, HierarchyLevel: role.HierarchyLevelDefault},
		{Name: role.SystemRoles.Tenant.DispatchExecutive, HierarchyLevel: role.HierarchyLevelDefault},
		{Name: role.SystemRoles.Tenant.HrManager, HierarchyLevel: role.HierarchyLevelDefault},
		{Name: role.SystemRoles.Tenant.HrExecutive, HierarchyLevel: role.HierarchyLevelDefault},
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
			if err := r.GrantPermission(p); err != nil {
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
