-- Role hierarchy edge queries — identity.role_hierarchy_edges is
-- RLS+FORCE per ADR 0058 (Wave 9.4). Reads/writes flow through RLS:
-- only the current tenant's rows are visible unless
-- app.is_platform = true.
--
-- The single-parent invariant ("a child has at most one ACTIVE
-- parent") lives in the partial unique index
-- uq_role_hierarchy_active_edge_per_child; the adapter translates
-- the SQLSTATE 23505 raised on collision into
-- rolehierarchy.ErrEdgeAlreadyExists.

-- name: InsertHierarchyEdge :exec
-- Active-state row. removed_at / removed_by / removal_reason NULL at
-- creation; populated by UpdateHierarchyEdgeRemoved once
-- Remove fires through the UpdateFn pattern.
INSERT INTO identity.role_hierarchy_edges (
    id, tenant_id, child_role_id, parent_role_id,
    established_at, established_by_membership_id, reason
) VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: GetHierarchyEdgeByID :one
SELECT id, tenant_id, child_role_id, parent_role_id,
       established_at, established_by_membership_id, reason,
       removed_at, removed_by_membership_id, removal_reason
FROM   identity.role_hierarchy_edges
WHERE  id = $1;

-- name: GetActiveHierarchyEdgeByChild :one
-- Used by SetRoleParent's "replace existing parent" path + the
-- read-side RoleView projection (join to populate parent_role_id).
SELECT id, tenant_id, child_role_id, parent_role_id,
       established_at, established_by_membership_id, reason,
       removed_at, removed_by_membership_id, removal_reason
FROM   identity.role_hierarchy_edges
WHERE  child_role_id = $1
  AND  removed_at    IS NULL;

-- name: UpdateHierarchyEdgeRemoved :exec
-- Soft-delete via the UpdateFn pattern. State enforced at the
-- application layer via the aggregate's IsActive guard; the SQL
-- just writes whatever the aggregate computed.
UPDATE identity.role_hierarchy_edges
SET    removed_at               = $2,
       removed_by_membership_id = $3,
       removal_reason           = $4
WHERE  id = $1;

-- name: ListActiveHierarchyEdgesByParent :many
-- "Show direct children of X" — used by approval-workflow reporting
-- + future org-chart UI. Ordered by establishment time so audit
-- dashboards render edges chronologically.
SELECT id, tenant_id, child_role_id, parent_role_id,
       established_at, established_by_membership_id, reason,
       removed_at, removed_by_membership_id, removal_reason
FROM   identity.role_hierarchy_edges
WHERE  parent_role_id = $1
  AND  removed_at     IS NULL
ORDER  BY established_at ASC;

-- name: GetHierarchyAncestorsByChild :many
-- Recursive CTE walking the parent chain UPWARD from the supplied
-- child_role_id. Only ACTIVE edges (removed_at IS NULL) are walked.
-- The seed row (the input child) is NOT returned; only its ancestor
-- edges are.
--
-- Depth column drives the ORDER BY so the caller gets
-- child's-parent first → grandparent → … root.
WITH RECURSIVE ancestor_chain AS (
    SELECT e.id, e.tenant_id, e.child_role_id, e.parent_role_id,
           e.established_at, e.established_by_membership_id, e.reason,
           e.removed_at, e.removed_by_membership_id, e.removal_reason,
           0 AS depth
    FROM   identity.role_hierarchy_edges e
    WHERE  e.child_role_id = $1
      AND  e.removed_at    IS NULL
    UNION ALL
    SELECT step.id, step.tenant_id, step.child_role_id, step.parent_role_id,
           step.established_at, step.established_by_membership_id, step.reason,
           step.removed_at, step.removed_by_membership_id, step.removal_reason,
           ac.depth + 1
    FROM   identity.role_hierarchy_edges step
    INNER JOIN ancestor_chain ac ON step.child_role_id = ac.parent_role_id
    WHERE  step.removed_at IS NULL
      AND  ac.depth        < 32
)
SELECT id, tenant_id, child_role_id, parent_role_id,
       established_at, established_by_membership_id, reason,
       removed_at, removed_by_membership_id, removal_reason
FROM   ancestor_chain
ORDER  BY depth ASC;
