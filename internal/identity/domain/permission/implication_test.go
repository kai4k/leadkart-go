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
		{p.PlatformUnverifiedContacts.Manage, false}, {p.PlatformLeadCredits.Topup, false},
		{p.Tenants.Suspend, false}, {p.Tenants.Activate, false}, {p.Tenants.Delete, false},
		{p.Users.Anonymise, false},
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
		{p.Users.Anonymise, nil},
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
		p.Users.Anonymise: true, p.PlatformLeadCredits.Topup: true,
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
