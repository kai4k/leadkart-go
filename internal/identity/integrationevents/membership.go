package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// MembershipCreatedV1 — a Person joined a tenant (or rejoined after a
// gap). Consumed by Tasks/CRM/etc. for default permission-set seeding
// + future hierarchy initialisation.
type MembershipCreatedV1 struct {
	MembershipID    uuid.UUID `json:"membership_id"`
	PersonID        uuid.UUID `json:"person_id"`
	TenantIDClaim   uuid.UUID `json:"tenant_id"`
	OccurredAtUTC   time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (MembershipCreatedV1) Topic() string { return "identity.membership_created.v1" }

// OccurredAt returns the domain timestamp.
func (e MembershipCreatedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped]. Method form (not direct field
// read) keeps the marker interface consistent across types — some
// future events may compute the tenant from a richer field set.
func (e MembershipCreatedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// MembershipDeactivatedV1 — Membership marked Inactive (job change,
// admin deactivation). Consumed by CRM (reassign open leads), Tasks
// (reassign open work items), Notifications (silence per-user channels).
type MembershipDeactivatedV1 struct {
	MembershipID  uuid.UUID `json:"membership_id"`
	PersonID      uuid.UUID `json:"person_id"`
	TenantIDClaim uuid.UUID `json:"tenant_id"`
	Reason        string    `json:"reason"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (MembershipDeactivatedV1) Topic() string { return "identity.membership_deactivated.v1" }

// OccurredAt returns the domain timestamp.
func (e MembershipDeactivatedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e MembershipDeactivatedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// MembershipReactivatedV1 — Inactive → Active (e.g. re-hire). Consumers
// re-enable per-user channels closed by [MembershipDeactivatedV1].
type MembershipReactivatedV1 struct {
	MembershipID  uuid.UUID `json:"membership_id"`
	PersonID      uuid.UUID `json:"person_id"`
	TenantIDClaim uuid.UUID `json:"tenant_id"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (MembershipReactivatedV1) Topic() string { return "identity.membership_reactivated.v1" }

// OccurredAt returns the domain timestamp.
func (e MembershipReactivatedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e MembershipReactivatedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// MembershipRoleAssignedV1 — a Role was assigned to a Membership.
// Drives SecurityStamp rotation per security.md "SecurityStamp rotation
// triggers" + permission-cache invalidation downstream.
type MembershipRoleAssignedV1 struct {
	MembershipID  uuid.UUID `json:"membership_id"`
	PersonID      uuid.UUID `json:"person_id"`
	TenantIDClaim uuid.UUID `json:"tenant_id"`
	RoleID        uuid.UUID `json:"role_id"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (MembershipRoleAssignedV1) Topic() string { return "identity.membership_role_assigned.v1" }

// OccurredAt returns the domain timestamp.
func (e MembershipRoleAssignedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e MembershipRoleAssignedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// MembershipRoleRevokedV1 — a Role was removed from a Membership.
type MembershipRoleRevokedV1 struct {
	MembershipID  uuid.UUID `json:"membership_id"`
	PersonID      uuid.UUID `json:"person_id"`
	TenantIDClaim uuid.UUID `json:"tenant_id"`
	RoleID        uuid.UUID `json:"role_id"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (MembershipRoleRevokedV1) Topic() string { return "identity.membership_role_revoked.v1" }

// OccurredAt returns the domain timestamp.
func (e MembershipRoleRevokedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e MembershipRoleRevokedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// MembershipPermissionsUpdatedV1 — the per-Membership permission overlay
// (granted / revoked override slices) changed. Single event per mutation
// regardless of granularity — consumers re-resolve effective permissions
// rather than diff per-permission deltas.
type MembershipPermissionsUpdatedV1 struct {
	MembershipID  uuid.UUID `json:"membership_id"`
	PersonID      uuid.UUID `json:"person_id"`
	TenantIDClaim uuid.UUID `json:"tenant_id"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (MembershipPermissionsUpdatedV1) Topic() string {
	return "identity.membership_permissions_updated.v1"
}

// OccurredAt returns the domain timestamp.
func (e MembershipPermissionsUpdatedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e MembershipPermissionsUpdatedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// MembershipProfileUpdatedV1 — Designation / Department /
// StatusMessage changed. Bundle update so wire payload reads coherently
// without per-field deltas.
type MembershipProfileUpdatedV1 struct {
	MembershipID  uuid.UUID `json:"membership_id"`
	PersonID      uuid.UUID `json:"person_id"`
	TenantIDClaim uuid.UUID `json:"tenant_id"`
	Designation   string    `json:"designation"`
	Department    string    `json:"department"`
	StatusMessage string    `json:"status_message"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (MembershipProfileUpdatedV1) Topic() string { return "identity.membership_profile_updated.v1" }

// OccurredAt returns the domain timestamp.
func (e MembershipProfileUpdatedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e MembershipProfileUpdatedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// MembershipManagerAssignedV1 — ReportsTo set to a non-zero manager.
// PreviousManager carried for audit narrative + hierarchy-cache
// invalidation. Zero PreviousManager indicates promotion from top-of-tree.
type MembershipManagerAssignedV1 struct {
	MembershipID    uuid.UUID `json:"membership_id"`
	PersonID        uuid.UUID `json:"person_id"`
	TenantIDClaim   uuid.UUID `json:"tenant_id"`
	ManagerID       uuid.UUID `json:"manager_id"`
	PreviousManager uuid.UUID `json:"previous_manager,omitempty"`
	OccurredAtUTC   time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (MembershipManagerAssignedV1) Topic() string {
	return "identity.membership_manager_assigned.v1"
}

// OccurredAt returns the domain timestamp.
func (e MembershipManagerAssignedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e MembershipManagerAssignedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// MembershipManagerRemovedV1 — ReportsTo cleared (top-of-tree). Carries
// PreviousManager so audit subscribers render the diff.
type MembershipManagerRemovedV1 struct {
	MembershipID    uuid.UUID `json:"membership_id"`
	PersonID        uuid.UUID `json:"person_id"`
	TenantIDClaim   uuid.UUID `json:"tenant_id"`
	PreviousManager uuid.UUID `json:"previous_manager"`
	OccurredAtUTC   time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (MembershipManagerRemovedV1) Topic() string {
	return "identity.membership_manager_removed.v1"
}

// OccurredAt returns the domain timestamp.
func (e MembershipManagerRemovedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e MembershipManagerRemovedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// Compile-time assertions + registration.
var (
	_ TenantScoped = MembershipCreatedV1{}
	_ TenantScoped = MembershipDeactivatedV1{}
	_ TenantScoped = MembershipReactivatedV1{}
	_ TenantScoped = MembershipRoleAssignedV1{}
	_ TenantScoped = MembershipRoleRevokedV1{}
	_ TenantScoped = MembershipPermissionsUpdatedV1{}
	_ TenantScoped = MembershipProfileUpdatedV1{}
	_ TenantScoped = MembershipManagerAssignedV1{}
	_ TenantScoped = MembershipManagerRemovedV1{}

	_ = register(MembershipCreatedV1{})
	_ = register(MembershipDeactivatedV1{})
	_ = register(MembershipReactivatedV1{})
	_ = register(MembershipRoleAssignedV1{})
	_ = register(MembershipRoleRevokedV1{})
	_ = register(MembershipPermissionsUpdatedV1{})
	_ = register(MembershipProfileUpdatedV1{})
	_ = register(MembershipManagerAssignedV1{})
	_ = register(MembershipManagerRemovedV1{})
)
