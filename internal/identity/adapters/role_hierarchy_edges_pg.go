package adapters

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/identity/adapters/db"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/rolehierarchy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// RoleHierarchyEdgeRepository is the pgx/sqlc-backed implementation of
// [rolehierarchy.Repository] per ADR 0058 (Wave 9.4). Tenant-scoped —
// every read + write runs under [pg.TxScopeTenant] so the connection's
// `app.tenant_id` GUC binds before queries touch the table; Postgres
// RLS does the rest.
//
// When ctx carries an active tx (a parent [pg.UnitOfWork] is in flight),
// Add joins that tx rather than opening its own — same shape as
// MembershipRepository.Add per ADR 0047. SetRoleParent's
// "soft-delete + insert" atomic-replacement uses this so both edge
// writes commit atomically.
type RoleHierarchyEdgeRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewRoleHierarchyEdgeRepository wires the repository.
func NewRoleHierarchyEdgeRepository(pool *pgxpool.Pool, tx *pg.Transactor) *RoleHierarchyEdgeRepository {
	return &RoleHierarchyEdgeRepository{pool: pool, tx: tx, q: db.New(pool)}
}

// Add satisfies [rolehierarchy.Repository] — persists a brand-new
// active Edge. Translates DB-level invariant breaches into typed
// domain sentinels (see [translateHierarchyEdgeError]).
//
// The aggregate carries its own TenantID — the GUC is bound from
// e.TenantID() (TDL canon per ADR 0062: tenantID flows through
// explicit values, not ctx).
func (r *RoleHierarchyEdgeRepository) Add(ctx context.Context, e *rolehierarchy.Edge) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.addOnTx(ctx, tx, e)
	}
	return r.tx.WithinTxPgxTenant(ctx, e.TenantID().String(), func(ctx context.Context, tx pgx.Tx) error {
		return r.addOnTx(ctx, tx, e)
	})
}

func (r *RoleHierarchyEdgeRepository) addOnTx(
	ctx context.Context,
	tx pgx.Tx,
	e *rolehierarchy.Edge,
) error {
	q := r.q.WithTx(tx)
	if err := insertHierarchyEdgeRow(ctx, q, e); err != nil {
		return err
	}
	return drainHierarchyEdgeEvents(ctx, tx, e)
}

// GetActiveByChild satisfies [rolehierarchy.Repository]. Tenant-scoped
// read returning the single active edge for the supplied child OR
// [rolehierarchy.ErrEdgeNotFound] when none exists. GUC bound from the
// explicit tenantID parameter (TDL canon per ADR 0062).
func (r *RoleHierarchyEdgeRepository) GetActiveByChild(
	ctx context.Context,
	tenantID tenant.ID,
	childRoleID role.ID,
) (*rolehierarchy.Edge, error) {
	cid, err := uuid.Parse(childRoleID.String())
	if err != nil {
		return nil, fmt.Errorf("edge repo: parse child id %q: %w", childRoleID, err)
	}
	var out *rolehierarchy.Edge
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		row, qerr := q.GetActiveHierarchyEdgeByChild(ctx, pgconv.PgUUID(cid))
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return rolehierarchy.ErrEdgeNotFound
			}
			return fmt.Errorf("edge repo: get active by child: %w", qerr)
		}
		got, perr := rowToHierarchyEdge(row)
		if perr != nil {
			return perr
		}
		out = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// UpdateByID satisfies [rolehierarchy.Repository] — TDL Sep 2024
// UpdateFn pattern: load → updateFn → persist (if shouldPersist) →
// drain events. All in one tenant-scoped transaction. GUC bound from
// the explicit tenantID parameter (TDL canon per ADR 0062).
//
// Joins a parent [pg.UnitOfWork] tx when present so SetRoleParent's
// "soft-delete-old + insert-new" replacement commits atomically.
func (r *RoleHierarchyEdgeRepository) UpdateByID(
	ctx context.Context,
	tenantID tenant.ID,
	id rolehierarchy.ID,
	updateFn func(*rolehierarchy.Edge) (bool, error),
) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.updateOnTx(ctx, tx, id, updateFn)
	}
	return r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		return r.updateOnTx(ctx, tx, id, updateFn)
	})
}

func (r *RoleHierarchyEdgeRepository) updateOnTx(
	ctx context.Context,
	tx pgx.Tx,
	id rolehierarchy.ID,
	updateFn func(*rolehierarchy.Edge) (bool, error),
) error {
	q := r.q.WithTx(tx)
	e, err := loadHierarchyEdge(ctx, q, id)
	if err != nil {
		return err
	}
	shouldPersist, err := updateFn(e)
	if err != nil {
		return err
	}
	if !shouldPersist {
		return nil
	}
	if err := persistHierarchyEdgeRemoval(ctx, q, e); err != nil {
		return err
	}
	return drainHierarchyEdgeEvents(ctx, tx, e)
}

// GetAncestorsByChild satisfies [rolehierarchy.Repository]. Returns
// ancestor edges in depth order (child's parent first → root) via the
// recursive CTE; only ACTIVE edges are walked. Empty result = child
// is a root. GUC bound from the explicit tenantID parameter (TDL
// canon per ADR 0062).
func (r *RoleHierarchyEdgeRepository) GetAncestorsByChild(
	ctx context.Context,
	tenantID tenant.ID,
	childRoleID role.ID,
) ([]*rolehierarchy.Edge, error) {
	cid, err := uuid.Parse(childRoleID.String())
	if err != nil {
		return nil, fmt.Errorf("edge repo: parse child id %q: %w", childRoleID, err)
	}
	var out []*rolehierarchy.Edge
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		rows, qerr := q.GetHierarchyAncestorsByChild(ctx, pgconv.PgUUID(cid))
		if qerr != nil {
			return fmt.Errorf("edge repo: get ancestors by child: %w", qerr)
		}
		out = make([]*rolehierarchy.Edge, 0, len(rows))
		for _, row := range rows {
			edge := rowToHierarchyEdgeFromAncestorRow(row)
			out = append(out, edge)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListActiveByParent satisfies [rolehierarchy.Repository]. Returns
// every direct child of `parentRoleID` ordered by establishment time.
// GUC bound from the explicit tenantID parameter (TDL canon per
// ADR 0062).
func (r *RoleHierarchyEdgeRepository) ListActiveByParent(
	ctx context.Context,
	tenantID tenant.ID,
	parentRoleID role.ID,
) ([]*rolehierarchy.Edge, error) {
	pid, err := uuid.Parse(parentRoleID.String())
	if err != nil {
		return nil, fmt.Errorf("edge repo: parse parent id %q: %w", parentRoleID, err)
	}
	var out []*rolehierarchy.Edge
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		rows, qerr := q.ListActiveHierarchyEdgesByParent(ctx, pgconv.PgUUID(pid))
		if qerr != nil {
			return fmt.Errorf("edge repo: list active by parent: %w", qerr)
		}
		out = make([]*rolehierarchy.Edge, 0, len(rows))
		for _, row := range rows {
			got, perr := rowToHierarchyEdge(row)
			if perr != nil {
				return perr
			}
			out = append(out, got)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ----- Helpers --------------------------------------------------------------

func loadHierarchyEdge(
	ctx context.Context,
	q *db.Queries,
	id rolehierarchy.ID,
) (*rolehierarchy.Edge, error) {
	rid, err := uuid.Parse(id.String())
	if err != nil {
		return nil, fmt.Errorf("edge repo: parse id %q: %w", id, err)
	}
	row, err := q.GetHierarchyEdgeByID(ctx, pgconv.PgUUID(rid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, rolehierarchy.ErrEdgeNotFound
		}
		return nil, fmt.Errorf("edge repo: get by id: %w", err)
	}
	return rowToHierarchyEdge(row)
}

func insertHierarchyEdgeRow(
	ctx context.Context,
	q *db.Queries,
	e *rolehierarchy.Edge,
) error {
	rid, err := uuid.Parse(e.ID().String())
	if err != nil {
		return fmt.Errorf("edge repo: parse id %q: %w", e.ID(), err)
	}
	tid, err := uuid.Parse(e.TenantID().String())
	if err != nil {
		return fmt.Errorf("edge repo: parse tenant id %q: %w", e.TenantID(), err)
	}
	cid, err := uuid.Parse(e.ChildRoleID().String())
	if err != nil {
		return fmt.Errorf("edge repo: parse child id %q: %w", e.ChildRoleID(), err)
	}
	pid, err := uuid.Parse(e.ParentRoleID().String())
	if err != nil {
		return fmt.Errorf("edge repo: parse parent id %q: %w", e.ParentRoleID(), err)
	}
	var establishedByPg = pgconv.PgUUIDOrNull(uuid.Nil)
	if mid := e.EstablishedByMembershipID(); !mid.IsZero() {
		muid, perr := uuid.Parse(mid.String())
		if perr != nil {
			return fmt.Errorf("edge repo: parse established_by %q: %w", mid, perr)
		}
		establishedByPg = pgconv.PgUUIDOrNull(muid)
	}
	reasonPg := pgconv.ZeroToNil(e.Reason())
	err = q.InsertHierarchyEdge(ctx, db.InsertHierarchyEdgeParams{
		ID:                        pgconv.PgUUID(rid),
		TenantID:                  pgconv.PgUUID(tid),
		ChildRoleID:               pgconv.PgUUID(cid),
		ParentRoleID:              pgconv.PgUUID(pid),
		EstablishedAt:             pgconv.PgRequiredTimestamp(e.EstablishedAt()),
		EstablishedByMembershipID: establishedByPg,
		Reason:                    reasonPg,
	})
	if err != nil {
		if tErr := translateHierarchyEdgeError(err); tErr != nil {
			return tErr
		}
		return fmt.Errorf("edge repo: insert: %w", err)
	}
	return nil
}

func persistHierarchyEdgeRemoval(
	ctx context.Context,
	q *db.Queries,
	e *rolehierarchy.Edge,
) error {
	rid, err := uuid.Parse(e.ID().String())
	if err != nil {
		return fmt.Errorf("edge repo: parse id %q: %w", e.ID(), err)
	}
	var removedByPg = pgconv.PgUUIDOrNull(uuid.Nil)
	if mid := e.RemovedByMembershipID(); !mid.IsZero() {
		muid, perr := uuid.Parse(mid.String())
		if perr != nil {
			return fmt.Errorf("edge repo: parse removed_by %q: %w", mid, perr)
		}
		removedByPg = pgconv.PgUUIDOrNull(muid)
	}
	reasonPg := pgconv.ZeroToNil(e.RemovalReason())
	err = q.UpdateHierarchyEdgeRemoved(ctx, db.UpdateHierarchyEdgeRemovedParams{
		ID:                    pgconv.PgUUID(rid),
		RemovedAt:             pgconv.PgTimestamp(e.RemovedAt()),
		RemovedByMembershipID: removedByPg,
		RemovalReason:         reasonPg,
	})
	if err != nil {
		return fmt.Errorf("edge repo: update removal: %w", err)
	}
	return nil
}

func drainHierarchyEdgeEvents(ctx context.Context, tx pgx.Tx, e *rolehierarchy.Edge) error {
	evs := e.PullEvents()
	if len(evs) == 0 {
		return nil
	}
	tid, err := uuid.Parse(e.TenantID().String())
	if err != nil {
		return fmt.Errorf("edge repo: parse tenant id %q: %w", e.TenantID(), err)
	}
	asAny := make([]any, len(evs))
	for i, ev := range evs {
		asAny[i] = ev
	}
	mapped, err := mapDomainEvents(asAny)
	if err != nil {
		return fmt.Errorf("edge repo: map events: %w", err)
	}
	return writeOutboxEvents(ctx, tx, tid, mapped)
}

func rowToHierarchyEdge(row db.IdentityRoleHierarchyEdge) (*rolehierarchy.Edge, error) {
	return rowToHierarchyEdgeFromCols(
		row.ID, row.TenantID, row.ChildRoleID, row.ParentRoleID,
		row.EstablishedAt, row.EstablishedByMembershipID, row.Reason,
		row.RemovedAt, row.RemovedByMembershipID, row.RemovalReason,
	), nil
}

func rowToHierarchyEdgeFromAncestorRow(row db.GetHierarchyAncestorsByChildRow) *rolehierarchy.Edge {
	return rowToHierarchyEdgeFromCols(
		row.ID, row.TenantID, row.ChildRoleID, row.ParentRoleID,
		row.EstablishedAt, row.EstablishedByMembershipID, row.Reason,
		row.RemovedAt, row.RemovedByMembershipID, row.RemovalReason,
	)
}

func rowToHierarchyEdgeFromCols(
	id, tid, cid, pid pgtype.UUID,
	estAt pgtype.Timestamptz, estBy pgtype.UUID, reason *string,
	remAt pgtype.Timestamptz, remBy pgtype.UUID, remReason *string,
) *rolehierarchy.Edge {
	establishedBy := membership.ID("")
	if v := pgconv.UUIDFromPg(estBy); v != uuid.Nil {
		establishedBy = membership.ID(v.String())
	}
	removedBy := membership.ID("")
	if v := pgconv.UUIDFromPg(remBy); v != uuid.Nil {
		removedBy = membership.ID(v.String())
	}
	reasonStr := ""
	if reason != nil {
		reasonStr = *reason
	}
	removalReasonStr := ""
	if remReason != nil {
		removalReasonStr = *remReason
	}
	return rolehierarchy.UnmarshalFromDB(rolehierarchy.Snapshot{
		ID:                        rolehierarchy.ID(pgconv.UUIDFromPg(id).String()),
		TenantID:                  tenant.ID(pgconv.UUIDFromPg(tid).String()),
		ChildRoleID:               role.ID(pgconv.UUIDFromPg(cid).String()),
		ParentRoleID:              role.ID(pgconv.UUIDFromPg(pid).String()),
		EstablishedAt:             pgconv.TimeFromPg(estAt),
		EstablishedByMembershipID: establishedBy,
		Reason:                    reasonStr,
		RemovedAt:                 pgconv.TimeFromPg(remAt),
		RemovedByMembershipID:     removedBy,
		RemovalReason:             removalReasonStr,
	})
}

// ----- DB-error → domain-sentinel translation ------------------------------

// constraintRoleHierarchyActiveEdgePerChild names the partial unique
// index from migration 20260523000007 enforcing the single-parent
// invariant. Adapter translates the SQLSTATE 23505 to
// [rolehierarchy.ErrEdgeAlreadyExists].
const constraintRoleHierarchyActiveEdgePerChild = "uq_role_hierarchy_active_edge_per_child"

// constraintEdgeSelfLoop names the CHECK from the same migration
// catching child == parent. Adapter translates the
// SQLSTATE 23514 to [rolehierarchy.ErrSelfReference].
const constraintEdgeSelfLoop = "chk_edge_no_self_loop"

// fkPrefixEdgesSameTenant is the shared prefix for both composite-FK
// constraint names (child + parent). Either firing means cross-tenant
// — translate to [rolehierarchy.ErrCrossTenant].
const fkPrefixEdgesSameTenant = "fk_edges_"

// translateHierarchyEdgeError maps the edge table's invariant breaches
// to typed domain sentinels. Returns nil when err is not a recognised
// edge-table violation; the caller falls through to its default wrap.
//
// SECURITY-DEFINER trigger gone per ADR 0058 — cross-tenant safety is
// now declarative (composite FK fires 23503) + cycle is the new
// edge_check_cycle SECURITY INVOKER trigger (raises 23514 with
// "cycle detected" in the message).
func translateHierarchyEdgeError(err error) error {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if !ok {
		return nil
	}
	switch pgErr.Code {
	case pg.SQLStateUniqueViolation:
		if pgErr.ConstraintName == constraintRoleHierarchyActiveEdgePerChild {
			return rolehierarchy.ErrEdgeAlreadyExists
		}
	case pg.SQLStateForeignKeyViolation:
		if strings.HasPrefix(pgErr.ConstraintName, fkPrefixEdgesSameTenant) {
			return rolehierarchy.ErrCrossTenant
		}
	case pg.SQLStateCheckViolation:
		if pgErr.ConstraintName == constraintEdgeSelfLoop {
			return rolehierarchy.ErrSelfReference
		}
		// edge_check_cycle trigger raises with USING ERRCODE = 'check_violation'
		// but no constraint name — discriminate by message text. Same pattern
		// the Wave 9.1d trigger used (now retired) — match by message marker.
		if strings.Contains(pgErr.Message, "cycle detected") {
			return rolehierarchy.ErrCycle
		}
	}
	return nil
}
