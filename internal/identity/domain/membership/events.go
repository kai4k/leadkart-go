package membership

import (
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Event is the SEALED marker interface for Membership domain events.
// Sealed via the unexported isMembershipEvent() method so only types
// in this package can satisfy it — same shape as role.Event.
//
// Domain events deliberately do NOT carry wire concerns (Topic / V1
// alias / occurred-at-as-method). Wire-versioning lives in
// integrationevents.*V1 per Vernon IDDD ch. 8 ("Domain Events vs.
// Integration Events"): a v2 wire rename must NOT force a domain edit.
// The integration mapper in internal/identity/integrationevents/
// type-switches on these structs and emits the canonical V1 envelope.
type Event interface {
	isMembershipEvent()
}

// CreatedEvent fires on [New].
type CreatedEvent struct {
	MembershipID ID
	PersonID     person.ID
	TenantID     tenant.ID
	At           time.Time
}

func (CreatedEvent) isMembershipEvent() {}

// DeactivatedEvent fires when a Membership transitions Active → Inactive.
type DeactivatedEvent struct {
	MembershipID ID
	PersonID     person.ID
	TenantID     tenant.ID
	Reason       string
	At           time.Time
}

func (DeactivatedEvent) isMembershipEvent() {}

// ReactivatedEvent fires when a Membership transitions Inactive → Active.
type ReactivatedEvent struct {
	MembershipID ID
	PersonID     person.ID
	TenantID     tenant.ID
	At           time.Time
}

func (ReactivatedEvent) isMembershipEvent() {}

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

func (RoleAssignedEvent) isMembershipEvent() {}

// RoleRevokedEvent fires when a Role is removed from the Membership.
type RoleRevokedEvent struct {
	MembershipID ID
	PersonID     person.ID
	TenantID     tenant.ID
	RoleID       role.ID
	At           time.Time
}

func (RoleRevokedEvent) isMembershipEvent() {}

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

func (PermissionsUpdatedEvent) isMembershipEvent() {}

// ProfileUpdatedEvent fires when Designation / Department /
// StatusMessage change. Bundle-update so wire payload reads
// coherently.
type ProfileUpdatedEvent struct {
	MembershipID  ID
	PersonID      person.ID
	TenantID      tenant.ID
	Designation   string
	Department    string
	StatusMessage string
	At            time.Time
}

func (ProfileUpdatedEvent) isMembershipEvent() {}

// ManagerAssignedEvent fires when ReportsTo is set to a non-zero
// manager Membership ID. PreviousManager carried for audit-log
// narrative + hierarchy-cache invalidation.
type ManagerAssignedEvent struct {
	MembershipID    ID
	PersonID        person.ID
	TenantID        tenant.ID
	ManagerID       ID
	PreviousManager ID
	At              time.Time
}

func (ManagerAssignedEvent) isMembershipEvent() {}

// ManagerRemovedEvent fires when ReportsTo is cleared (top-of-tree).
type ManagerRemovedEvent struct {
	MembershipID    ID
	PersonID        person.ID
	TenantID        tenant.ID
	PreviousManager ID
	At              time.Time
}

func (ManagerRemovedEvent) isMembershipEvent() {}
