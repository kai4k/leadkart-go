// Package permission defines the Permission value object and the
// closed-set IdentityPermissions catalogue.
package permission

import (
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Permission name length bounds (mirror of .NET parent's
// Permission.cs Create validator). Open-format input via [Create]
// must satisfy NameMinLength ≤ len(trimmed) ≤ NameMaxLength.
const (
	NameMinLength = 3
	NameMaxLength = 100
)

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

type metaPermissions struct {
	TenantAdmin string
	// RequestPermissionElevation lets a Membership submit an ADR 0055
	// permission-elevation request. Granted to every regular user by
	// default — the act of asking carries no authority by itself; the
	// approval flow is where authority is conferred.
	RequestPermissionElevation string
}
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
	// Approve is the manager-approval permission introduced in ADR 0055.
	// Held by managers / role-leads so they can approve permission-
	// elevation requests submitted by their direct reports. Platform
	// operators ALSO satisfy this gate via is_platform=true so they
	// can approve requests for orphan / root memberships with no manager.
	View, Create, Update, Delete, Assign, Revoke, Approve string
}

// crmLeadsPermissions cover the CRM lead-management surface introduced
// in ADR 0060 (CRM Slice 1). Per BRD §6.3 + §4.9: Read held by every
// sales-tier role; Assign by SalesManager/CompanyOwner/OfficeAdministrator;
// Manage by the assigned SalesExecutive + SalesManager + CompanyOwner;
// ReadAll overrides the per-handler "only my assigned leads" filter
// (Auth0 "read:any" canon) — held by SalesManager + CompanyOwner.
type crmLeadsPermissions struct{ Read, Assign, Manage, ReadAll string }

// crmPermissions groups the CRM bounded context surface. Slice 1 only
// ships the Leads namespace; CallLogs / Reminders / AssignmentHistory
// aggregates are gated through these four (no per-aggregate permission
// proliferation per ADR 0036 closed-set discipline).
type crmPermissions struct {
	Leads crmLeadsPermissions
}

// platformUnverifiedContactsPermissions, platformMarketplacePermissions,
// and platformLeadCreditsPermissions cover the Platform bounded context
// surface added in ADR 0059. The Platform module ports gate on these
// strings via authn.RequirePermission / RequireAnyPermission.
type platformUnverifiedContactsPermissions struct{ Manage string }
type platformMarketplacePermissions struct{ Browse, Purchase string }
type platformLeadCreditsPermissions struct{ Read, Topup string }

// inventoryCatalogPermissions cover Product master reads + writes
// (name, SKU, dosage form, pack size, HSN, gst_rate_bps, is_active,
// soft-delete). Per BRD §6.5 + ADR 0061 (Inventory Slice 1).
type inventoryCatalogPermissions struct{ Read, Manage string }

// inventoryStockPermissions cover Batch + StockMovement reads + writes.
// Manage gates POST /v1/inventory/batches + POST .../movements; Read
// gates GET .../batches + .../movements + .../products/{id}/batches.
type inventoryStockPermissions struct{ Read, Manage string }

// inventoryPermissions groups the Inventory bounded context surface —
// paired Catalog (product master) + Stock (batches + movements) per
// BRD §6.5. Slice 1 ships these four; future slices (Reservation,
// LowStockAlerts) extend.
type inventoryPermissions struct {
	Catalog inventoryCatalogPermissions
	Stock   inventoryStockPermissions
}

// tasksWorkItemsPermissions cover the Tasks WorkItem aggregate per
// BRD §6.8. Every tenant member gets Read + Manage (own tasks);
// Manager-tier + OfficeAdministrator + CompanyOwner additionally get
// ReadAll (team-wide visibility via the org hierarchy) + Reassign
// (move a work item to a different membership in the manager's chain
// per §6.7 visibility rule).
type tasksWorkItemsPermissions struct{ Read, ReadAll, Manage, Reassign string }

// tasksPermissions groups the Tasks bounded context surface. Slice 1
// only ships the WorkItems namespace; future Reminders / Templates
// aggregates extend.
type tasksPermissions struct {
	WorkItems tasksWorkItemsPermissions
}
// IdentityPermissions is the closed catalogue of every permission the
// system recognises. Mirror of the .NET `IdentityPermissions` static
// class. Maintain in lockstep with the intern-table list.
//
// Module-owned permission groups (Platform-context groups +
// Inventory) are declared here per ADR 0051 "single-module type
// placement" carve-out: the closed catalogue is the one place where
// every recognised permission name is enumerated, so the
// PermissionResolver + middleware see them as a single closed set
// regardless of which bounded context owns the named action.
// Adding a new module-owned permission = append to this catalogue +
// the [allNames] slice. Drift caught at test time
// (TestAll_NoDuplicates + TestIdentityPermissions_Catalogue).
var IdentityPermissions = struct {
	Meta     metaPermissions
	Platform platformPermissions
	Tenants  tenantsPermissions
	Users    usersPermissions
	Roles    rolesPermissions

	// CRM bounded context (ADR 0060) — CrmLead / CallLog /
	// AssignmentHistory aggregates per BRD §6.3.
	Crm crmPermissions

	// Platform bounded context (ADR 0059) — verification pipeline +
	// marketplace + lead credits.
	PlatformUnverifiedContacts platformUnverifiedContactsPermissions
	PlatformMarketplace        platformMarketplacePermissions
	PlatformLeadCredits        platformLeadCreditsPermissions

	// Inventory bounded context (ADR 0061) — Product / Batch /
	// StockMovement aggregates per BRD §6.5.
	Inventory inventoryPermissions

	// Tasks bounded context — WorkItem aggregate per BRD §6.8.
	Tasks tasksPermissions
}{
	Meta: metaPermissions{
		TenantAdmin:                "tenant.admin",
		RequestPermissionElevation: "identity.meta.request_permission_elevation",
	},
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
		View:    "identity.roles.view",
		Create:  "identity.roles.create",
		Update:  "identity.roles.update",
		Delete:  "identity.roles.delete",
		Assign:  "identity.roles.assign",
		Revoke:  "identity.roles.revoke",
		Approve: "identity.roles.approve",
	},

	Crm: crmPermissions{
		Leads: crmLeadsPermissions{
			Read:    "crm.leads.read",
			Assign:  "crm.leads.assign",
			Manage:  "crm.leads.manage",
			ReadAll: "crm.leads.read_all",
		},
	},

	PlatformUnverifiedContacts: platformUnverifiedContactsPermissions{
		Manage: "platform.unverified_contacts.manage",
	},
	PlatformMarketplace: platformMarketplacePermissions{
		Browse:   "platform.marketplace.browse",
		Purchase: "platform.marketplace.purchase",
	},
	PlatformLeadCredits: platformLeadCreditsPermissions{
		Read:  "platform.lead_credits.read",
		Topup: "platform.lead_credits.topup",
	},
	Inventory: inventoryPermissions{
		Catalog: inventoryCatalogPermissions{
			Read:   "inventory.catalog.read",
			Manage: "inventory.catalog.manage",
		},
		Stock: inventoryStockPermissions{
			Read:   "inventory.stock.read",
			Manage: "inventory.stock.manage",
		},
	},

	Tasks: tasksPermissions{
		WorkItems: tasksWorkItemsPermissions{
			Read:     "tasks.work_items.read",
			ReadAll:  "tasks.work_items.read_all",
			Manage:   "tasks.work_items.manage",
			Reassign: "tasks.work_items.reassign",
		},
	},
}

func allNames() []string {
	p := IdentityPermissions
	return []string{
		p.Meta.TenantAdmin, p.Meta.RequestPermissionElevation,
		p.Platform.TenantsView, p.Platform.TenantsCreate, p.Platform.TenantsManage,
		p.Platform.UsersView, p.Platform.UsersCreate, p.Platform.UsersManage,
		p.Platform.RolesView, p.Platform.RolesManage,
		p.Tenants.View, p.Tenants.Update, p.Tenants.UpdateSettings,
		p.Tenants.Suspend, p.Tenants.Activate, p.Tenants.Delete,
		p.Users.View, p.Users.Create, p.Users.Update,
		p.Users.Deactivate, p.Users.Reactivate, p.Users.Unlock,
		p.Users.Anonymise, p.Users.UpdatePermissions,
		p.Roles.View, p.Roles.Create, p.Roles.Update, p.Roles.Delete,
		p.Roles.Assign, p.Roles.Revoke, p.Roles.Approve,

		// CRM bounded context (ADR 0060).
		p.Crm.Leads.Read, p.Crm.Leads.Assign, p.Crm.Leads.Manage, p.Crm.Leads.ReadAll,

		// Platform bounded context (ADR 0059).
		p.PlatformUnverifiedContacts.Manage,
		p.PlatformMarketplace.Browse, p.PlatformMarketplace.Purchase,
		p.PlatformLeadCredits.Read, p.PlatformLeadCredits.Topup,

		// Inventory bounded context (ADR 0061).
		p.Inventory.Catalog.Read, p.Inventory.Catalog.Manage,
		p.Inventory.Stock.Read, p.Inventory.Stock.Manage,

		// Tasks bounded context (BRD §6.8).
		p.Tasks.WorkItems.Read, p.Tasks.WorkItems.ReadAll,
		p.Tasks.WorkItems.Manage, p.Tasks.WorkItems.Reassign,
	}
}

var (
	internOnce sync.Once
	intern     map[string]*Permission
)

func ensureIntern() {
	internOnce.Do(func() {
		names := allNames()
		intern = make(map[string]*Permission, len(names))
		for _, n := range names {
			intern[n] = &Permission{name: n}
		}
	})
}

// All returns every catalogue entry as an interned [Permission] slice.
func All() []*Permission {
	ensureIntern()
	names := allNames()
	out := make([]*Permission, len(names))
	for i, n := range names {
		out[i] = intern[n]
	}
	return out
}

// FromConstant returns the interned [Permission] for a known catalogue
// constant. Panics on miss — callers MUST pass an `IdentityPermissions.X`
// reference. Use [TryFromConstant] for untrusted input.
func FromConstant(name string) *Permission {
	ensureIntern()
	p, ok := intern[name]
	if !ok {
		panic(fmt.Sprintf("permission: %q not in catalogue", name))
	}
	return p
}

// TryFromConstant is the Result-shaped lookup for untrusted input.
func TryFromConstant(name string) (*Permission, error) {
	if strings.TrimSpace(name) == "" {
		return nil, ErrEmpty
	}
	ensureIntern()
	if p, ok := intern[strings.TrimSpace(name)]; ok {
		return p, nil
	}
	return nil, fmt.Errorf("%w: %q", ErrUnknown, name)
}

// Create is the open-input validator (charset + length bounds). Returns
// the interned pointer when input matches a catalogue entry; fresh
// non-interned otherwise.
func Create(name string) (*Permission, error) {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return nil, ErrEmpty
	}
	if len(trimmed) < NameMinLength || len(trimmed) > NameMaxLength {
		return nil, fmt.Errorf("%w: length %d not in [%d,%d]",
			ErrFormat, len(trimmed), NameMinLength, NameMaxLength)
	}
	for _, r := range trimmed {
		ok := (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '.' || r == '_' || r == ':'
		if !ok {
			return nil, fmt.Errorf("%w: invalid char %q", ErrFormat, r)
		}
	}
	ensureIntern()
	if p, ok := intern[trimmed]; ok {
		return p, nil
	}
	return &Permission{name: trimmed}, nil
}

// IsKnown reports whether the supplied name appears in the closed catalogue.
func IsKnown(name string) bool {
	ensureIntern()
	_, ok := intern[name]
	return ok
}
