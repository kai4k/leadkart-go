package permission

import "slices"

// platformReserved are catalogue entries that tenant.admin NEVER implies:
// platform-operator authority plus tenant-lifecycle and cross-tenant erasure
// actions reserved for the Platform tier (ADR 0036 + multi-tenancy.md
// "Platform admin endpoints"). tenant.admin itself is listed so a requirement
// for it is satisfied only by holding it explicitly. Built once; never mutated.
var platformReserved = func() map[string]struct{} {
	p := IdentityPermissions
	names := []string{
		p.Meta.TenantAdmin,
		p.Platform.TenantsView, p.Platform.TenantsCreate, p.Platform.TenantsManage,
		p.Platform.UsersView, p.Platform.UsersCreate, p.Platform.UsersManage,
		p.Platform.RolesView, p.Platform.RolesManage, p.Platform.ImpersonationManage, p.Platform.TenantsOwnership,
		p.PlatformUnverifiedContacts.Manage,
		p.PlatformLeadCredits.Topup,
		p.Tenants.Suspend, p.Tenants.Activate,
	}
	m := make(map[string]struct{}, len(names))
	for _, n := range names {
		m[n] = struct{}{}
	}
	return m
}()

// IsPlatformReserved reports whether name is a platform-reserved permission
// (see platformReserved). tenant.admin is included: it is satisfied only by
// holding it. Unknown names return false.
func IsPlatformReserved(name string) bool {
	_, ok := platformReserved[name]
	return ok
}

// ImpliedByTenantAdmin reports whether holding tenant.admin satisfies a
// requirement for name inside the holder's OWN tenant. Per ADR 0061
// amendment 1 ("CompanyOwner ... short-circuits via Meta.TenantAdmin") and
// ADR 0045 (an impersonating operator gets the full CompanyOwner authority);
// recorded in ADR 0068. Unknown and platform-reserved names return false.
// Callers MUST also check that the effective tenant is the holder's own.
func ImpliedByTenantAdmin(name string) bool {
	return IsKnown(name) && !IsPlatformReserved(name)
}

// IsPlatformManagedRole reports whether name is one of the three role-management permissions
// (identity.roles.create, update, manage). Owner ruling 2026-11-25 (ADR 0073 addendum): a company
// never creates, edits or deletes its own roles. No tenant-plane grant satisfies them, explicit or
// implied by tenant.admin, whoever holds it in the company; only LeadKart platform authority does
// (the platform.roles.manage equivalent, and an impersonation session, which is a platform
// operator acting inside the company). identity.roles.assign and revoke are unchanged.
func IsPlatformManagedRole(name string) bool {
	p := IdentityPermissions
	return name == p.Roles.Create || name == p.Roles.Update || name == p.Roles.Manage
}

// platformEquivalents maps a tenant-plane identity permission to the
// platform-tier permissions that satisfy it. Built once; never mutated.
var platformEquivalents = func() map[string][]string {
	p := IdentityPermissions
	m := map[string][]string{
		p.Tenants.View:           {p.Platform.TenantsView, p.Platform.TenantsManage},
		p.Tenants.Update:         {p.Platform.TenantsManage},
		p.Tenants.UpdateSettings: {p.Platform.TenantsManage},
		p.Users.View:             {p.Platform.UsersView, p.Platform.UsersManage},
		p.Users.Create:           {p.Platform.UsersCreate, p.Platform.UsersManage},
		p.Roles.View:             {p.Platform.RolesView, p.Platform.RolesManage},
	}
	for _, n := range []string{p.Users.Update, p.Users.Deactivate, p.Users.Reactivate, p.Users.Unlock, p.Users.UpdatePermissions} {
		m[n] = []string{p.Platform.UsersManage}
	}
	for _, n := range []string{p.Roles.Create, p.Roles.Update, p.Roles.Manage, p.Roles.Assign, p.Roles.Revoke} {
		m[n] = []string{p.Platform.RolesManage}
	}
	return m
}()

// PlatformEquivalents returns the platform-tier permissions that satisfy a
// tenant-plane identity permission when held by a slug-anchored Platform
// operator on the unified /v1/users, /v1/roles, /v1/tenants surface
// (ADR 0039 Context; mirrors the .NET PlatformTenant*Endpoints gates).
// Nil when name has no platform equivalent. The returned slice is a copy.
func PlatformEquivalents(name string) []string {
	return slices.Clone(platformEquivalents[name])
}

// IsRequestable reports whether name may be the subject of a permission-
// elevation request (ADR 0069 H4): exactly the permissions tenant.admin implies.
// tenant.admin itself and every platform-reserved name are conferred only by role.
func IsRequestable(name string) bool { return ImpliedByTenantAdmin(name) }
