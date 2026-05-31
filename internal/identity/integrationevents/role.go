package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// RoleCreatedV1 — a Role was created. Tenant-scoped: every Role belongs to
// exactly one tenant, including the platform-tenant SuperAdmin role.
type RoleCreatedV1 struct {
	RoleID          uuid.UUID `json:"role_id"`
	TenantIDClaim   uuid.UUID `json:"tenant_id"`
	Name            string    `json:"name"`
	IsSystemDefault bool      `json:"is_system_default"`
	IsSuperAdmin    bool      `json:"is_super_admin"`
	HierarchyLevel  int       `json:"hierarchy_level"`
	OccurredAtUTC   time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (RoleCreatedV1) Topic() string { return "identity.role_created.v1" }

// OccurredAt returns the domain timestamp.
func (e RoleCreatedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e RoleCreatedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// RoleRenamedV1 — a non-system-default role's name changed. OldName/NewName
// let audit subscribers render the diff without re-loading state.
type RoleRenamedV1 struct {
	RoleID        uuid.UUID `json:"role_id"`
	TenantIDClaim uuid.UUID `json:"tenant_id"`
	OldName       string    `json:"old_name"`
	NewName       string    `json:"new_name"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (RoleRenamedV1) Topic() string { return "identity.role_renamed.v1" }

// OccurredAt returns the domain timestamp.
func (e RoleRenamedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e RoleRenamedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// RolePermissionGrantedV1 — a Permission was added to a Role. Downstream caches
// invalidate by RoleID and the Memberships holding that Role.
type RolePermissionGrantedV1 struct {
	RoleID        uuid.UUID `json:"role_id"`
	TenantIDClaim uuid.UUID `json:"tenant_id"`
	Permission    string    `json:"permission"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (RolePermissionGrantedV1) Topic() string { return "identity.role_permission_granted.v1" }

// OccurredAt returns the domain timestamp.
func (e RolePermissionGrantedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e RolePermissionGrantedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// RolePermissionRevokedV1 — a Permission was removed from a Role's set.
type RolePermissionRevokedV1 struct {
	RoleID        uuid.UUID `json:"role_id"`
	TenantIDClaim uuid.UUID `json:"tenant_id"`
	Permission    string    `json:"permission"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (RolePermissionRevokedV1) Topic() string { return "identity.role_permission_revoked.v1" }

// OccurredAt returns the domain timestamp.
func (e RolePermissionRevokedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e RolePermissionRevokedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// RoleDeletedV1 — a non-system-default role was soft-deleted. Downstream
// modules revoke Membership assignments referencing this RoleID.
type RoleDeletedV1 struct {
	RoleID        uuid.UUID `json:"role_id"`
	TenantIDClaim uuid.UUID `json:"tenant_id"`
	DeletedBy     string    `json:"deleted_by"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (RoleDeletedV1) Topic() string { return "identity.role_deleted.v1" }

// OccurredAt returns the domain timestamp.
func (e RoleDeletedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e RoleDeletedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// RoleParentChangedV1 retired in Wave 9.4 (ADR 0058 supersedes ADR 0054);
// see role_hierarchy.go.

// Compile-time assertions and registration.
var (
	_ TenantScoped = RoleCreatedV1{}
	_ TenantScoped = RoleRenamedV1{}
	_ TenantScoped = RolePermissionGrantedV1{}
	_ TenantScoped = RolePermissionRevokedV1{}
	_ TenantScoped = RoleDeletedV1{}

	_ = register(RoleCreatedV1{})
	_ = register(RoleRenamedV1{})
	_ = register(RolePermissionGrantedV1{})
	_ = register(RolePermissionRevokedV1{})
	_ = register(RoleDeletedV1{})
)
