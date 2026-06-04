package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// WorkItemCreatedV1 — a WorkItem was created (manual or auto-from-
// subscriber). Tenant-scoped.
//
// Wire alias: `tasks.work_item_created.v1`.
type WorkItemCreatedV1 struct {
	WorkItemID             uuid.UUID `json:"work_item_id"`
	TenantIDClaim          uuid.UUID `json:"tenant_id"`
	Type                   string    `json:"type"`
	Priority               string    `json:"priority"`
	Title                  string    `json:"title"`
	AssignedToMembershipID uuid.UUID `json:"assigned_to_membership_id"`
	AssignedByMembershipID uuid.UUID `json:"assigned_by_membership_id"`
	DueAtUTC               time.Time `json:"due_at_utc"`
	BatchID                uuid.UUID `json:"batch_id,omitzero"`
	SourceModule           string    `json:"source_module,omitzero"`
	SourceEntityType       string    `json:"source_entity_type,omitzero"`
	SourceEntityID         string    `json:"source_entity_id,omitzero"`
	CreatedByMembershipID  uuid.UUID `json:"created_by_membership_id"`
	OccurredAtUTC          time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (WorkItemCreatedV1) Topic() string { return "tasks.work_item_created.v1" }

// OccurredAt returns the domain timestamp.
func (e WorkItemCreatedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e WorkItemCreatedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// WorkItemAssignedV1 — a WorkItem was reassigned to a new membership
// (separate from initial creation). Tenant-scoped.
//
// Wire alias: `tasks.work_item_assigned.v1`.
type WorkItemAssignedV1 struct {
	WorkItemID               uuid.UUID `json:"work_item_id"`
	TenantIDClaim            uuid.UUID `json:"tenant_id"`
	PreviousAssignee         uuid.UUID `json:"previous_assignee,omitzero"`
	NewAssigneeMembershipID  uuid.UUID `json:"new_assignee_membership_id"`
	ReassignedByMembershipID uuid.UUID `json:"reassigned_by_membership_id"`
	Reason                   string    `json:"reason,omitzero"`
	OccurredAtUTC            time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (WorkItemAssignedV1) Topic() string { return "tasks.work_item_assigned.v1" }

// OccurredAt returns the domain timestamp.
func (e WorkItemAssignedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e WorkItemAssignedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// WorkItemCompletedV1 — a WorkItem was terminally completed.
//
// Wire alias: `tasks.work_item_completed.v1`.
type WorkItemCompletedV1 struct {
	WorkItemID    uuid.UUID `json:"work_item_id"`
	TenantIDClaim uuid.UUID `json:"tenant_id"`
	ActorID       uuid.UUID `json:"actor_membership_id"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (WorkItemCompletedV1) Topic() string { return "tasks.work_item_completed.v1" }

// OccurredAt returns the domain timestamp.
func (e WorkItemCompletedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e WorkItemCompletedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// WorkItemCancelledV1 — a WorkItem was terminally cancelled. Reason
// is mandatory per BRD §6.8 audit doctrine.
//
// Wire alias: `tasks.work_item_cancelled.v1`.
type WorkItemCancelledV1 struct {
	WorkItemID    uuid.UUID `json:"work_item_id"`
	TenantIDClaim uuid.UUID `json:"tenant_id"`
	ActorID       uuid.UUID `json:"actor_membership_id"`
	Reason        string    `json:"reason"`
	OccurredAtUTC time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (WorkItemCancelledV1) Topic() string { return "tasks.work_item_cancelled.v1" }

// OccurredAt returns the domain timestamp.
func (e WorkItemCancelledV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e WorkItemCancelledV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// WorkItemOverdueV1 — a WorkItem was flagged overdue. Notifications
// subscriber routes to the assignee inbox.
//
// Wire alias: `tasks.work_item_overdue.v1`.
type WorkItemOverdueV1 struct {
	WorkItemID             uuid.UUID `json:"work_item_id"`
	TenantIDClaim          uuid.UUID `json:"tenant_id"`
	AssignedToMembershipID uuid.UUID `json:"assigned_to_membership_id"`
	DueAtUTC               time.Time `json:"due_at_utc"`
	OccurredAtUTC          time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (WorkItemOverdueV1) Topic() string { return "tasks.work_item_overdue.v1" }

// OccurredAt returns the domain timestamp.
func (e WorkItemOverdueV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e WorkItemOverdueV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// WorkItemHierarchyOverdueV1 — fires when an OVERDUE work item is
// detected for a subordinate of the supplied manager chain. The
// future Notifications subscriber routes to managers per BRD §6.9
// management-chain notification rules.
//
// This is a SEPARATE event from WorkItemOverdueV1: the Tasks module
// emits the per-task overdue once; an upstream subscriber / future
// in-module enricher fans out the management-chain copies. v0.2
// ships the type so consumers can already wire against it; the
// enrichment hop lands when Notifications module ships.
//
// Wire alias: `tasks.work_item_hierarchy_overdue.v1`.
type WorkItemHierarchyOverdueV1 struct {
	WorkItemID             uuid.UUID `json:"work_item_id"`
	TenantIDClaim          uuid.UUID `json:"tenant_id"`
	AssignedToMembershipID uuid.UUID `json:"assigned_to_membership_id"`
	ManagerMembershipID    uuid.UUID `json:"manager_membership_id"`
	DueAtUTC               time.Time `json:"due_at_utc"`
	OccurredAtUTC          time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (WorkItemHierarchyOverdueV1) Topic() string {
	return "tasks.work_item_hierarchy_overdue.v1"
}

// OccurredAt returns the domain timestamp.
func (e WorkItemHierarchyOverdueV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e WorkItemHierarchyOverdueV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// Compile-time assertions + registry registrations.
var (
	_ TenantScoped = WorkItemCreatedV1{}
	_ TenantScoped = WorkItemAssignedV1{}
	_ TenantScoped = WorkItemCompletedV1{}
	_ TenantScoped = WorkItemCancelledV1{}
	_ TenantScoped = WorkItemOverdueV1{}
	_ TenantScoped = WorkItemHierarchyOverdueV1{}

	_ = register(WorkItemCreatedV1{})
	_ = register(WorkItemAssignedV1{})
	_ = register(WorkItemCompletedV1{})
	_ = register(WorkItemCancelledV1{})
	_ = register(WorkItemOverdueV1{})
	_ = register(WorkItemHierarchyOverdueV1{})
)
