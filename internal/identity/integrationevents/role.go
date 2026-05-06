package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// RoleCreatedV1 — a Role was created in the given tenant. Consumed
// by audit + permission-cache invalidation subscribers + future
// admin-UI live-update channels.
//
// Tenant-scoped: every Role belongs to exactly one tenant. The
// platform-tenant SuperAdmin role is no exception — its TenantID
// is the platform tenant's UUID.
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

// Compile-time + runtime registration.
var (
	_ TenantScoped = RoleCreatedV1{}
	_              = register(RoleCreatedV1{})
)
