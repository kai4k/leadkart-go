package permission_test

import (
	"errors"
	"strings"
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
	if permission.IdentityPermissions.Inventory.Catalog.Read != "inventory.catalog.read" {
		t.Fatalf("Inventory.Catalog.Read: %q", permission.IdentityPermissions.Inventory.Catalog.Read)
	}
	if permission.IdentityPermissions.Inventory.Catalog.Manage != "inventory.catalog.manage" {
		t.Fatalf("Inventory.Catalog.Manage: %q", permission.IdentityPermissions.Inventory.Catalog.Manage)
	}
	if permission.IdentityPermissions.Inventory.Stock.Read != "inventory.stock.read" {
		t.Fatalf("Inventory.Stock.Read: %q", permission.IdentityPermissions.Inventory.Stock.Read)
	}
	if permission.IdentityPermissions.Inventory.Stock.Manage != "inventory.stock.manage" {
		t.Fatalf("Inventory.Stock.Manage: %q", permission.IdentityPermissions.Inventory.Stock.Manage)
	}
}

func TestFromConstant_ReturnsInternedInstance(t *testing.T) {
	t.Parallel()
	a := permission.FromConstant(permission.IdentityPermissions.Users.Create)
	b := permission.FromConstant(permission.IdentityPermissions.Users.Create)
	if a != b {
		t.Fatal("intern broken — different pointers for same constant")
	}
}

func TestFromConstant_PanicsOnUnknown(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic for unknown constant")
		}
	}()
	permission.FromConstant("not.a.real.permission")
}

func TestTryFromConstant_HitMissEmpty(t *testing.T) {
	t.Parallel()
	p, err := permission.TryFromConstant(" identity.users.create ")
	if err != nil || p.Name() != "identity.users.create" {
		t.Fatalf("trim hit: %v %q", err, p.Name())
	}
	if _, err := permission.TryFromConstant("nope.nope"); !errors.Is(err, permission.ErrUnknown) {
		t.Fatalf("want ErrUnknown got %v", err)
	}
	if _, err := permission.TryFromConstant("  "); !errors.Is(err, permission.ErrEmpty) {
		t.Fatalf("want ErrEmpty got %v", err)
	}
}

func TestCreate_KnownReturnsInternedUnknownReturnsFresh(t *testing.T) {
	t.Parallel()
	known, _ := permission.Create("identity.users.create")
	if known != permission.FromConstant(permission.IdentityPermissions.Users.Create) {
		t.Fatal("Create did not return interned for known input")
	}
	custom, err := permission.Create("custom.something")
	if err != nil {
		t.Fatalf("Create custom: %v", err)
	}
	if permission.IsKnown(custom.Name()) {
		t.Fatal("custom permission should not be in catalogue")
	}
}

func TestCreate_RejectsBadFormat(t *testing.T) {
	t.Parallel()
	tooLong := strings.Repeat("a", 110)
	for _, in := range []string{"", " ", "ab", "a$b.c", tooLong} {
		if _, err := permission.Create(in); err == nil {
			t.Fatalf("Create(%q) should fail", in)
		}
	}
}

func TestAll_NoDuplicates(t *testing.T) {
	t.Parallel()
	all := permission.All()
	if len(all) < 20 {
		t.Fatalf("All catalogue suspiciously small: %d", len(all))
	}
	seen := map[string]bool{}
	for _, p := range all {
		if seen[p.Name()] {
			t.Fatalf("dup: %q", p.Name())
		}
		seen[p.Name()] = true
	}
}
