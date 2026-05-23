package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// Wave 9.4 / ADR 0058 — the rolehierarchy aggregate emits these
// when a parent→child edge is established or removed. Replaces
// RoleParentChangedV1 (retired) with two distinct events so
// subscribers (cached effective-permission projection invalidation,
// audit log, future org-chart UI) can react asymmetrically.
//
// Tenant-scoped — every edge belongs to exactly one tenant
// (composite FK fk_edges_*_same_tenant enforces).

// RoleHierarchyEdgeEstablishedV1 — a brand-new active edge was added
// linking child_role_id → parent_role_id. Subscribers invalidate any
// cached effective-permission projections / org-chart caches for
// memberships holding either role.
//
// EstablishedByMembershipID is the zero UUID when a system path
// (data migration, onboarding seed) created the edge.
type RoleHierarchyEdgeEstablishedV1 struct {
	EdgeID                    uuid.UUID `json:"edge_id"`
	TenantIDClaim             uuid.UUID `json:"tenant_id"`
	ChildRoleID               uuid.UUID `json:"child_role_id"`
	ParentRoleID              uuid.UUID `json:"parent_role_id"`
	EstablishedByMembershipID uuid.UUID `json:"established_by_membership_id,omitempty"`
	Reason                    string    `json:"reason,omitempty"`
	OccurredAtUTC             time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (RoleHierarchyEdgeEstablishedV1) Topic() string {
	return "identity.role_hierarchy_edge_established.v1"
}

// OccurredAt returns the domain timestamp.
func (e RoleHierarchyEdgeEstablishedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e RoleHierarchyEdgeEstablishedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// RoleHierarchyEdgeRemovedV1 — an active edge was soft-deleted.
// Subscribers invalidate the same caches the establish event did.
// Same identity tuple so audit dashboards can pair them.
type RoleHierarchyEdgeRemovedV1 struct {
	EdgeID                uuid.UUID `json:"edge_id"`
	TenantIDClaim         uuid.UUID `json:"tenant_id"`
	ChildRoleID           uuid.UUID `json:"child_role_id"`
	ParentRoleID          uuid.UUID `json:"parent_role_id"`
	RemovedByMembershipID uuid.UUID `json:"removed_by_membership_id,omitempty"`
	RemovalReason         string    `json:"removal_reason,omitempty"`
	OccurredAtUTC         time.Time `json:"occurred_at_utc"`
}

// Topic returns the canonical wire alias.
func (RoleHierarchyEdgeRemovedV1) Topic() string {
	return "identity.role_hierarchy_edge_removed.v1"
}

// OccurredAt returns the domain timestamp.
func (e RoleHierarchyEdgeRemovedV1) OccurredAt() time.Time { return e.OccurredAtUTC }

// TenantID satisfies [TenantScoped].
func (e RoleHierarchyEdgeRemovedV1) TenantID() uuid.UUID { return e.TenantIDClaim }

// Compile-time + runtime registration.
var (
	_ TenantScoped = RoleHierarchyEdgeEstablishedV1{}
	_ TenantScoped = RoleHierarchyEdgeRemovedV1{}

	_ = register(RoleHierarchyEdgeEstablishedV1{})
	_ = register(RoleHierarchyEdgeRemovedV1{})
)
