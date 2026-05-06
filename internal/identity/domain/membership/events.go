package membership

import (
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Event is the marker interface for Membership domain events.
type Event interface {
	Topic() string
	OccurredAt() time.Time
}

// CreatedEvent fires on [New].
type CreatedEvent struct {
	MembershipID ID
	PersonID     person.ID
	TenantID     tenant.ID
	At           time.Time
}

// Topic returns the integration-event type.
func (CreatedEvent) Topic() string { return "identity.membership_created.v1" }

// OccurredAt returns the domain timestamp.
func (e CreatedEvent) OccurredAt() time.Time { return e.At }

// DeactivatedEvent fires when a Membership transitions Active → Inactive.
type DeactivatedEvent struct {
	MembershipID ID
	PersonID     person.ID
	TenantID     tenant.ID
	Reason       string
	At           time.Time
}

// Topic returns the integration-event type.
func (DeactivatedEvent) Topic() string { return "identity.membership_deactivated.v1" }

// OccurredAt returns the domain timestamp.
func (e DeactivatedEvent) OccurredAt() time.Time { return e.At }

// ReactivatedEvent fires when a Membership transitions Inactive → Active.
type ReactivatedEvent struct {
	MembershipID ID
	PersonID     person.ID
	TenantID     tenant.ID
	At           time.Time
}

// Topic returns the integration-event type.
func (ReactivatedEvent) Topic() string { return "identity.membership_reactivated.v1" }

// OccurredAt returns the domain timestamp.
func (e ReactivatedEvent) OccurredAt() time.Time { return e.At }

// RoleAssignedEvent fires when a Role is assigned to the Membership.
// Subscribers: SecurityStamp invalidator (rotation triggers per
// `security.md`), audit log, integration-event mapper.
type RoleAssignedEvent struct {
	MembershipID ID
	PersonID     person.ID
	TenantID     tenant.ID
	RoleID       role.ID
	At           time.Time
}

// Topic returns the integration-event type.
func (RoleAssignedEvent) Topic() string { return "identity.membership_role_assigned.v1" }

// OccurredAt returns the domain timestamp.
func (e RoleAssignedEvent) OccurredAt() time.Time { return e.At }

// RoleRevokedEvent fires when a Role is removed from the Membership.
type RoleRevokedEvent struct {
	MembershipID ID
	PersonID     person.ID
	TenantID     tenant.ID
	RoleID       role.ID
	At           time.Time
}

// Topic returns the integration-event type.
func (RoleRevokedEvent) Topic() string { return "identity.membership_role_revoked.v1" }

// OccurredAt returns the domain timestamp.
func (e RoleRevokedEvent) OccurredAt() time.Time { return e.At }

// PermissionsUpdatedEvent fires when the per-Membership Granted /
// Revoked overlay changes. Single event per mutation regardless of
// granularity — subscribers care about "permissions changed for this
// Membership", not per-permission deltas. Triggers SecurityStamp
// rotation (per `security.md` "SecurityStamp rotation triggers").
type PermissionsUpdatedEvent struct {
	MembershipID ID
	PersonID     person.ID
	TenantID     tenant.ID
	At           time.Time
}

// Topic returns the integration-event type.
func (PermissionsUpdatedEvent) Topic() string { return "identity.membership_permissions_updated.v1" }

// OccurredAt returns the domain timestamp.
func (e PermissionsUpdatedEvent) OccurredAt() time.Time { return e.At }
