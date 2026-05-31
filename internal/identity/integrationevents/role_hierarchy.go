package integrationevents

import (
	"time"

	"github.com/google/uuid"
)

// Wave 9.4 / ADR 0058: two distinct events replace the retired RoleParentChangedV1
// so subscribers can react asymmetrically. Tenant-scoped — every edge belongs
// to exactly one tenant (composite FK fk_edges_*_same_tenant).

// RoleHierarchyEdgeEstablishedV1 — a new child→parent edge was created.
// Subscribers invalidate effective-permission caches for memberships holding
// either role. EstablishedByMembershipID is zero for system paths (migrations,
// seeds).
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

// RoleHierarchyEdgeRemovedV1 — an active edge was soft-deleted. Same identity
// tuple as [RoleHierarchyEdgeEstablishedV1] so audit dashboards can pair them.
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

// Compile-time assertions and registration.
var (
	_ TenantScoped = RoleHierarchyEdgeEstablishedV1{}
	_ TenantScoped = RoleHierarchyEdgeRemovedV1{}

	_ = register(RoleHierarchyEdgeEstablishedV1{})
	_ = register(RoleHierarchyEdgeRemovedV1{})
)
