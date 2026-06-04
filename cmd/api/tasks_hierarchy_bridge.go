// tasks_hierarchy_bridge.go — composition-root adapters that bind the
// Tasks app/-side interfaces (command.HierarchyReader,
// command.MembershipReader, query.HierarchyReader) to the existing
// identity-side membership repository per ADR 0047 boundary discipline.
//
// The Tasks module declares the interfaces in its own command/ +
// query/ packages so handler tests can mock them; the composition
// root binds them to real implementations here without exposing
// identity-adapter types to the Tasks module.
package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// tasksHierarchyAdapter walks the identity.tenant_memberships
// reports_to chain to produce the set of memberships visible from the
// supplied actor membership. Used by tasks.command.HierarchyReader +
// tasks.query.HierarchyReader.
//
// SQL: a recursive CTE seeds with the actor's membership id +
// recursively unions every direct + indirect report. Tenant-scoped via
// the WHERE tenant_id = $1 + RLS on identity.tenant_memberships.
//
// Returns the actor itself + every subordinate (transitively). Empty
// slice when the actor membership doesn't exist in the tenant.
type tasksHierarchyAdapter struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
}

func newTasksHierarchyAdapter(pool *pgxpool.Pool, tx *pg.Transactor) *tasksHierarchyAdapter {
	return &tasksHierarchyAdapter{pool: pool, tx: tx}
}

const tasksHierarchyRecursiveSQL = `
WITH RECURSIVE chain AS (
    SELECT id, reports_to
    FROM   identity.tenant_memberships
    WHERE  tenant_id = $1
      AND  id = $2
      AND  status = 'active'
    UNION ALL
    SELECT m.id, m.reports_to
    FROM   identity.tenant_memberships m
    INNER JOIN chain c ON m.reports_to = c.id
    WHERE  m.tenant_id = $1
      AND  m.status = 'active'
)
SELECT id FROM chain
`

// ListSubordinateMembershipIDs satisfies the Tasks-side
// HierarchyReader interfaces.
func (a *tasksHierarchyAdapter) ListSubordinateMembershipIDs(ctx context.Context, tenantID tenant.ID, membershipID string) ([]string, error) {
	tid, err := uuid.Parse(tenantID.String())
	if err != nil {
		return nil, fmt.Errorf("tasks hierarchy: parse tenant id %q: %w", tenantID, err)
	}
	mid, err := uuid.Parse(membershipID)
	if err != nil {
		return nil, fmt.Errorf("tasks hierarchy: parse membership id %q: %w", membershipID, err)
	}
	var out []string
	err = a.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, tasksHierarchyRecursiveSQL,
			pgtype.UUID{Bytes: tid, Valid: true},
			pgtype.UUID{Bytes: mid, Valid: true})
		if err != nil {
			return fmt.Errorf("tasks hierarchy: query: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var id pgtype.UUID
			if err := rows.Scan(&id); err != nil {
				return fmt.Errorf("tasks hierarchy: scan: %w", err)
			}
			if !id.Valid {
				continue
			}
			out = append(out, uuid.UUID(id.Bytes).String())
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// tasksMembershipAdapter is the lightweight active-membership probe
// satisfying Tasks-side MembershipReader. Single-row SELECT against
// identity.tenant_memberships scoped by RLS.
type tasksMembershipAdapter struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
}

func newTasksMembershipAdapter(pool *pgxpool.Pool, tx *pg.Transactor) *tasksMembershipAdapter {
	return &tasksMembershipAdapter{pool: pool, tx: tx}
}

const tasksMembershipExistsSQL = `
SELECT 1
FROM   identity.tenant_memberships
WHERE  tenant_id = $1
  AND  id = $2
  AND  status = 'active'
LIMIT  1
`

// ExistsActiveInTenant satisfies the Tasks-side MembershipReader
// interface.
func (a *tasksMembershipAdapter) ExistsActiveInTenant(ctx context.Context, tenantID tenant.ID, membershipID string) (bool, error) {
	tid, err := uuid.Parse(tenantID.String())
	if err != nil {
		return false, fmt.Errorf("tasks membership: parse tenant id %q: %w", tenantID, err)
	}
	mid, err := uuid.Parse(membershipID)
	if err != nil {
		return false, fmt.Errorf("tasks membership: parse membership id %q: %w", membershipID, err)
	}
	var found bool
	err = a.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		var n int
		err := tx.QueryRow(ctx, tasksMembershipExistsSQL,
			pgtype.UUID{Bytes: tid, Valid: true},
			pgtype.UUID{Bytes: mid, Valid: true}).Scan(&n)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			return fmt.Errorf("tasks membership: query: %w", err)
		}
		found = n == 1
		return nil
	})
	if err != nil {
		return false, err
	}
	return found, nil
}
