package rolehierarchy

import (
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Event is the SEALED marker interface for Edge domain events.
// Sealed via the unexported isEdgeEvent() method so only types in
// this package can satisfy it — same shape as role.Event +
// permissionrequest.Event.
//
// Per Vernon IDDD ch.8 — domain events deliberately do NOT carry
// wire concerns (Topic / V1 alias). Wire-versioning lives in
// internal/identity/integrationevents/role_hierarchy.go; the
// mapper there type-switches on these structs + emits the canonical
// V1 envelope.
type Event interface {
	isEdgeEvent()
}

// EstablishedEvent fires when a brand-new active Edge is constructed
// via [New]. Carries the full identity tuple so subscribers (cached
// effective-permission projection invalidation, audit log, future
// org-chart UI live-update channels) don't need to re-load the
// aggregate.
type EstablishedEvent struct {
	ID                        ID
	TenantID                  tenant.ID
	ChildRoleID               role.ID
	ParentRoleID              role.ID
	EstablishedByMembershipID membership.ID // zero = system / migration
	Reason                    string
	At                        time.Time
}

func (EstablishedEvent) isEdgeEvent() {}

// RemovedEvent fires when an active Edge is soft-deleted via
// [Edge.Remove]. Carries the same identity tuple as the establishment
// event so subscribers can pair them in audit dashboards.
type RemovedEvent struct {
	ID                    ID
	TenantID              tenant.ID
	ChildRoleID           role.ID
	ParentRoleID          role.ID
	RemovedByMembershipID membership.ID // zero = system / cascade
	Reason                string
	At                    time.Time
}

func (RemovedEvent) isEdgeEvent() {}
