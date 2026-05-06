package permission_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
)

func TestErrors_AreSentinels(t *testing.T) {
	t.Parallel()
	if !errors.Is(permission.ErrUnknown, permission.ErrUnknown) {
		t.Fatal("ErrUnknown not a sentinel")
	}
	if !errors.Is(permission.ErrEmpty, permission.ErrEmpty) {
		t.Fatal("ErrEmpty not a sentinel")
	}
	if !errors.Is(permission.ErrFormat, permission.ErrFormat) {
		t.Fatal("ErrFormat not a sentinel")
	}
}

func TestPermission_NilSafe(t *testing.T) {
	t.Parallel()
	var p *permission.Permission
	if p.Name() != "" {
		t.Fatal("nil.Name() should return empty")
	}
	if p.String() != "" {
		t.Fatal("nil.String() should return empty")
	}
}

func TestPermission_Equal(t *testing.T) {
	t.Parallel()
	var nilP *permission.Permission
	if !nilP.Equal(nil) {
		t.Fatal("nil.Equal(nil) should be true")
	}
}

func TestIdentityPermissions_Catalogue(t *testing.T) {
	t.Parallel()
	if permission.IdentityPermissions.Meta.TenantAdmin != "tenant.admin" {
		t.Fatalf("Meta.TenantAdmin: %q", permission.IdentityPermissions.Meta.TenantAdmin)
	}
	if permission.IdentityPermissions.Platform.TenantsManage != "platform.tenants.manage" {
		t.Fatalf("Platform.TenantsManage: %q", permission.IdentityPermissions.Platform.TenantsManage)
	}
	if permission.IdentityPermissions.Tenants.UpdateSettings != "identity.tenants.update_settings" {
		t.Fatalf("Tenants.UpdateSettings: %q", permission.IdentityPermissions.Tenants.UpdateSettings)
	}
	if permission.IdentityPermissions.Users.Anonymise != "identity.users.anonymise" {
		t.Fatalf("Users.Anonymise: %q", permission.IdentityPermissions.Users.Anonymise)
	}
	if permission.IdentityPermissions.Roles.Assign != "identity.roles.assign" {
		t.Fatalf("Roles.Assign: %q", permission.IdentityPermissions.Roles.Assign)
	}
}
