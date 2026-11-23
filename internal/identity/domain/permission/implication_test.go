package permission_test

import (
	"slices"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
)

func TestImpliedByTenantAdmin(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	cases := []struct {
		name string
		want bool
	}{
		{p.Users.View, true}, {p.Users.UpdatePermissions, true}, {p.Roles.Assign, true},
		{p.Tenants.Update, true}, {p.Tenants.UpdateSettings, true},
		{p.Crm.Leads.ReadAll, true}, {p.Inventory.Stock.Manage, true},
		{p.PlatformMarketplace.Purchase, true}, {p.PlatformLeadCredits.Read, true},
		{p.Meta.TenantAdmin, false},
		{p.Platform.TenantsView, false}, {p.Platform.UsersManage, false}, {p.Platform.RolesManage, false},
		{p.Platform.ImpersonationManage, false}, {p.Platform.TenantsOwnership, false},
		{p.PlatformUnverifiedContacts.Manage, false}, {p.PlatformLeadCredits.Topup, false},
		{p.Tenants.Suspend, false}, {p.Tenants.Activate, false}, {p.Tenants.Delete, false},
		{"not.a.permission", false}, {"", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := permission.ImpliedByTenantAdmin(tc.name); got != tc.want {
				t.Fatalf("ImpliedByTenantAdmin(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestPlatformEquivalents(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	cases := []struct {
		name string
		want []string
	}{
		{p.Users.View, []string{p.Platform.UsersView, p.Platform.UsersManage}},
		{p.Users.Create, []string{p.Platform.UsersCreate, p.Platform.UsersManage}},
		{p.Users.Deactivate, []string{p.Platform.UsersManage}},
		{p.Roles.View, []string{p.Platform.RolesView, p.Platform.RolesManage}},
		{p.Roles.Assign, []string{p.Platform.RolesManage}},
		{p.Tenants.View, []string{p.Platform.TenantsView, p.Platform.TenantsManage}},
		{p.Tenants.UpdateSettings, []string{p.Platform.TenantsManage}},
		{p.Crm.Leads.Read, nil},
		// Impersonation has no tenant-plane twin: no tenant permission maps onto it.
		{p.Platform.ImpersonationManage, nil},
		// Ownership has no tenant-plane twin either.
		{p.Platform.TenantsOwnership, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := permission.PlatformEquivalents(tc.name); !slices.Equal(got, tc.want) {
				t.Fatalf("PlatformEquivalents(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestIsPlatformReserved(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	for name, want := range map[string]bool{
		p.Meta.TenantAdmin: true, p.Platform.TenantsManage: true, p.Tenants.Suspend: true,
		p.PlatformLeadCredits.Topup: true, p.Platform.ImpersonationManage: true, p.Platform.TenantsOwnership: true,
		p.Users.View: false, p.Crm.Leads.Read: false, "not.a.permission": false, "": false,
	} {
		if got := permission.IsPlatformReserved(name); got != want {
			t.Errorf("IsPlatformReserved(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestPlatformEquivalents_ReturnsCopy(t *testing.T) {
	t.Parallel()
	p := permission.IdentityPermissions
	first := permission.PlatformEquivalents(p.Users.View)
	first[0] = "tampered"
	if got := permission.PlatformEquivalents(p.Users.View); got[0] != p.Platform.UsersView {
		t.Fatalf("shared state mutated: %v", got)
	}
}

// platform.impersonation.manage opens POST/GET/DELETE /platform/impersonation/sessions
// and so mints a tenant.admin token for any tenant. It is in the catalogue, is
// platform-reserved (tenant.admin never implies it), and is not the platform
// equivalent of any tenant-plane permission, so no tenant permission can stand in for it.
func TestImpersonationManage_IsReservedAndStandsAlone(t *testing.T) {
	t.Parallel()
	name := permission.IdentityPermissions.Platform.ImpersonationManage
	if name != "platform.impersonation.manage" || !permission.IsKnown(name) {
		t.Fatalf("catalogue entry %q known=%v, want platform.impersonation.manage in the catalogue", name, permission.IsKnown(name))
	}
	if !permission.IsPlatformReserved(name) || permission.ImpliedByTenantAdmin(name) || permission.IsRequestable(name) {
		t.Errorf("reserved=%v impliedByTenantAdmin=%v requestable=%v, want true false false",
			permission.IsPlatformReserved(name), permission.ImpliedByTenantAdmin(name), permission.IsRequestable(name))
	}
	for _, p := range permission.All() {
		if slices.Contains(permission.PlatformEquivalents(p.Name()), name) {
			t.Errorf("%s lists %s among its platform equivalents", p.Name(), name)
		}
	}
}

// platform.tenants.ownership gates changing who OWNS a customer tenant from
// outside it (ADR 0069 R3 amendment). It is declared exactly like
// platform.impersonation.manage: in the catalogue, platform-reserved (tenant.admin
// never implies it), not requestable, no tenant-plane equivalent.
func TestTenantsOwnership_IsReservedAndStandsAlone(t *testing.T) {
	t.Parallel()
	name := permission.IdentityPermissions.Platform.TenantsOwnership
	if name != "platform.tenants.ownership" || !permission.IsKnown(name) {
		t.Fatalf("catalogue entry %q known=%v, want platform.tenants.ownership in the catalogue", name, permission.IsKnown(name))
	}
	if !permission.IsPlatformReserved(name) || permission.ImpliedByTenantAdmin(name) || permission.IsRequestable(name) {
		t.Errorf("reserved=%v impliedByTenantAdmin=%v requestable=%v, want true false false",
			permission.IsPlatformReserved(name), permission.ImpliedByTenantAdmin(name), permission.IsRequestable(name))
	}
	for _, p := range permission.All() {
		if slices.Contains(permission.PlatformEquivalents(p.Name()), name) {
			t.Errorf("%s lists %s among its platform equivalents", p.Name(), name)
		}
	}
}
