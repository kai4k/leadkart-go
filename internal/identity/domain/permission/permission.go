// Package permission defines the Permission value object and the
// closed-set IdentityPermissions catalogue.
package permission

import "errors"

// ErrUnknown is the sentinel returned when an input string does not
// match any catalogue entry. HTTP layer maps to 400 with code
// `permission_unknown`.
var ErrUnknown = errors.New("permission: unknown name")

// ErrEmpty is returned for empty / whitespace-only input.
var ErrEmpty = errors.New("permission: name required")

// ErrFormat is returned for charset / length-bound failures.
var ErrFormat = errors.New("permission: invalid format")

// Permission is a value object — comparable by Name. Identity-equality
// holds for any two pointers obtained from the intern table.
type Permission struct {
	name string
}

// Name returns the canonical wire-string form. Nil-safe.
func (p *Permission) Name() string {
	if p == nil {
		return ""
	}
	return p.name
}

// String implements fmt.Stringer for log + error formatting.
func (p *Permission) String() string { return p.Name() }

// Equal reports whether two permissions are the same. Pointer equality
// for interned instances; name compare otherwise. nil == nil is true.
func (p *Permission) Equal(other *Permission) bool {
	if p == other {
		return true
	}
	if p == nil || other == nil {
		return false
	}
	return p.name == other.name
}

type metaPermissions struct{ TenantAdmin string }
type platformPermissions struct {
	TenantsView, TenantsCreate, TenantsManage string
	UsersView, UsersCreate, UsersManage       string
	RolesView, RolesManage                    string
}
type tenantsPermissions struct {
	View, Update, UpdateSettings, Suspend, Activate, Delete string
}
type usersPermissions struct {
	View, Create, Update, Deactivate, Reactivate, Unlock, Anonymise, UpdatePermissions string
}
type rolesPermissions struct {
	View, Create, Update, Delete, Assign, Revoke string
}

// IdentityPermissions is the closed catalogue of every permission the
// system recognises. Mirror of the .NET `IdentityPermissions` static
// class. Maintain in lockstep with the intern-table list.
var IdentityPermissions = struct {
	Meta     metaPermissions
	Platform platformPermissions
	Tenants  tenantsPermissions
	Users    usersPermissions
	Roles    rolesPermissions
}{
	Meta: metaPermissions{TenantAdmin: "tenant.admin"},
	Platform: platformPermissions{
		TenantsView:   "platform.tenants.view",
		TenantsCreate: "platform.tenants.create",
		TenantsManage: "platform.tenants.manage",
		UsersView:     "platform.users.view",
		UsersCreate:   "platform.users.create",
		UsersManage:   "platform.users.manage",
		RolesView:     "platform.roles.view",
		RolesManage:   "platform.roles.manage",
	},
	Tenants: tenantsPermissions{
		View:           "identity.tenants.view",
		Update:         "identity.tenants.update",
		UpdateSettings: "identity.tenants.update_settings",
		Suspend:        "identity.tenants.suspend",
		Activate:       "identity.tenants.activate",
		Delete:         "identity.tenants.delete",
	},
	Users: usersPermissions{
		View:              "identity.users.view",
		Create:            "identity.users.create",
		Update:            "identity.users.update",
		Deactivate:        "identity.users.deactivate",
		Reactivate:        "identity.users.reactivate",
		Unlock:            "identity.users.unlock",
		Anonymise:         "identity.users.anonymise",
		UpdatePermissions: "identity.users.update_permissions",
	},
	Roles: rolesPermissions{
		View:   "identity.roles.view",
		Create: "identity.roles.create",
		Update: "identity.roles.update",
		Delete: "identity.roles.delete",
		Assign: "identity.roles.assign",
		Revoke: "identity.roles.revoke",
	},
}
