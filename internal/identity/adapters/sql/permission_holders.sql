-- Who holds a permission in a company: the read the Notifications module needs to
-- address stock alerts (it cannot read the identity schema, so the composition
-- root binds a port to this query).

-- name: ListActiveMembershipsHoldingAnyPermission :many
-- Active memberships of the tenant holding at least one of the permissions through
-- a role (a super-admin role holds every permission). Direct per-member overrides
-- are not consulted: an alert is addressed to the role holders.
SELECT DISTINCT m.id
FROM   identity.tenant_memberships m
JOIN   identity.role_assignments ra
    ON ra.membership_id = m.id
   AND ra.tenant_id     = m.tenant_id
JOIN   identity.roles r
    ON r.id = ra.role_id
   AND NOT r.is_deleted
WHERE  m.tenant_id = $1
AND    m.status    = 'active'
AND    (r.is_super_admin OR jsonb_exists_any(r.permissions, sqlc.arg(permissions)::text[]))
ORDER  BY m.id;
