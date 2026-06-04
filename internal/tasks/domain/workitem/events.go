package workitem

import (
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// Event is the SEALED marker interface for WorkItem domain events.
// Sealed via the unexported isWorkItemEvent() method so only types in
// this package can satisfy it — same shape as crmlead / identity
// aggregate events.
//
// Domain events deliberately do NOT carry wire concerns (Topic / V1
// alias / occurred-at-as-method). Wire-versioning lives in
// internal/tasks/integrationevents/*V1 per Vernon IDDD ch.8 — a v2
// wire rename must NOT force a domain edit.
type Event interface {
	isWorkItemEvent()
}

// CreatedEvent fires when a WorkItem is created via [NewManual] or
// [NewAutoCreated]. Source* fields are empty for manual-created tasks.
type CreatedEvent struct {
	WorkItemID             ID
	TenantID               tenant.ID
	Type                   Type
	Priority               Priority
	Title                  string
	AssignedToMembershipID string
	AssignedByMembershipID string
	DueAt                  time.Time
	BatchID                string // empty for non-batch creation
	SourceModule           string
	SourceEntityType       string
	SourceEntityID         string
	CreatedByMembershipID  string
	At                     time.Time
}

func (CreatedEvent) isWorkItemEvent() {}

// StartedEvent fires when [Start] flips state to in_progress.
type StartedEvent struct {
	WorkItemID ID
	TenantID   tenant.ID
	ActorID    string
	At         time.Time
}

func (StartedEvent) isWorkItemEvent() {}

// CompletedEvent fires when [Complete] terminally closes the task.
type CompletedEvent struct {
	WorkItemID ID
	TenantID   tenant.ID
	ActorID    string
	At         time.Time
}

func (CompletedEvent) isWorkItemEvent() {}

// CancelledEvent fires when [Cancel] terminally drops the task.
// Reason is mandatory (per BRD §6.8 audit doctrine).
type CancelledEvent struct {
	WorkItemID ID
	TenantID   tenant.ID
	ActorID    string
	Reason     string
	At         time.Time
}

func (CancelledEvent) isWorkItemEvent() {}

// OverdueEvent fires when [MarkOverdue] flags a task as overdue. The
// downstream Notifications subscriber routes this AND the management-
// chain variant (raised by the app layer via hierarchy lookup) to the
// right inboxes per BRD §6.8.
type OverdueEvent struct {
	WorkItemID             ID
	TenantID               tenant.ID
	AssignedToMembershipID string
	DueAt                  time.Time
	At                     time.Time
}

func (OverdueEvent) isWorkItemEvent() {}

// ReassignedEvent fires when [Reassign] moves the work item to a
// different membership. PreviousAssignee is the prior owner.
type ReassignedEvent struct {
	WorkItemID               ID
	TenantID                 tenant.ID
	PreviousAssignee         string
	NewAssigneeMembershipID  string
	ReassignedByMembershipID string
	Reason                   string
	At                       time.Time
}

func (ReassignedEvent) isWorkItemEvent() {}
