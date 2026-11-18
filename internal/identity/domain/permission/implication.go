package permission

import "slices"

// platformReservedNames are catalogue entries that tenant.admin NEVER implies:
// platform-operator authority plus tenant-lifecycle and cross-tenant erasure
// actions reserved for the Platform tier (ADR 0036 + multi-tenancy.md
// "Platform admin endpoints"). tenant.admin itself is listed so a requirement
// for it is satisfied only by holding it explicitly.
func platformReservedNames() []string {
	p := IdentityPermissions
	return []string{
		p.Meta.TenantAdmin,
		p.Platform.TenantsView, p.Platform.TenantsCreate, p.Platform.TenantsManage,
		p.Platform.UsersView, p.Platform.UsersCreate, p.Platform.UsersManage,
		p.Platform.RolesView, p.Platform.RolesManage,
		p.PlatformUnverifiedContacts.Manage,
		p.PlatformLeadCredits.Topup,
		p.Tenants.Suspend, p.Tenants.Activate, p.Tenants.Delete,
		p.Users.Anonymise,
	}
}

// ImpliedByTenantAdmin reports whether holding tenant.admin satisfies a
// requirement for name inside the holder's OWN tenant. Per ADR 0061
// amendment 1 ("CompanyOwner ... short-circuits via Meta.TenantAdmin") and
// ADR 0045 (an impersonating operator gets the full CompanyOwner authority);
// recorded in ADR 0068. Unknown and platform-reserved names return false.
// Callers MUST also check that the effective tenant is the holder's own.
func ImpliedByTenantAdmin(name string) bool {
	if !IsKnown(name) {
		return false
	}
	return !slices.Contains(platformReservedNames(), name)
}

// PlatformEquivalents returns the platform-tier permissions that satisfy a
// tenant-plane identity permission when held by a slug-anchored Platform
// operator on the unified /v1/users, /v1/roles, /v1/tenants surface
// (ADR 0039 Context; mirrors the .NET PlatformTenant*Endpoints gates).
// Nil when name has no platform equivalent.
func PlatformEquivalents(name string) []string {
	p := IdentityPermissions
	switch name {
	case p.Tenants.View:
		return []string{p.Platform.TenantsView, p.Platform.TenantsManage}
	case p.Tenants.Update, p.Tenants.UpdateSettings:
		return []string{p.Platform.TenantsManage}
	case p.Users.View:
		return []string{p.Platform.UsersView, p.Platform.UsersManage}
	case p.Users.Create:
		return []string{p.Platform.UsersCreate, p.Platform.UsersManage}
	case p.Users.Update, p.Users.Deactivate, p.Users.Reactivate, p.Users.Unlock, p.Users.UpdatePermissions:
		return []string{p.Platform.UsersManage}
	case p.Roles.View:
		return []string{p.Platform.RolesView, p.Platform.RolesManage}
	case p.Roles.Create, p.Roles.Update, p.Roles.Delete, p.Roles.Assign, p.Roles.Revoke:
		return []string{p.Platform.RolesManage}
	}
	return nil
}
