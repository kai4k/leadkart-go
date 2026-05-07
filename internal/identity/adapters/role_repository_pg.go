package adapters

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/platform/pg"
)

// RoleRepository is the pgx/sqlc-backed implementation of
// [role.Repository]. Tenant-scoped — every read + write runs under
// [pg.TxScopeTenant] so the connection's `app.tenant_id` GUC binds
// before queries touch the table; Postgres RLS does the rest.
//
// Cross-tenant operations (SuperAdmin admin UI, support tooling,
// platform analytics) MUST use a platform-scoped transactor at a
// higher layer; this repo refuses to bypass RLS itself.
//
// Domain↔row mapping lives here; sqlc-generated *Queries hold the SQL.
type RoleRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *Queries
}

// NewRoleRepository wires the repository against a pool + transactor
// (same pool as the transactor — composing distinct pools would split
// the connection state the GUC binds to).
func NewRoleRepository(pool *pgxpool.Pool, tx *pg.Transactor) *RoleRepository {
	return &RoleRepository{pool: pool, tx: tx, q: New(pool)}
}

// Add satisfies [role.Repository] — persists a brand-new Role from
// [role.New], drains its events into the outbox, all in one tx under
// tenant scope. Translates SQLSTATE 23505 from the partial unique
// index `uq_roles_tenant_name WHERE NOT is_deleted` into
// [role.ErrNameTaken].
func (r *RoleRepository) Add(ctx context.Context, ro *role.Role) error {
	return r.tx.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		if err := insertRoleRow(ctx, q, ro); err != nil {
			return err
		}
		return drainRoleEvents(ctx, tx, ro)
	})
}

// GetByID satisfies [role.Repository]. RLS-scoped read.
func (r *RoleRepository) GetByID(ctx context.Context, id role.ID) (*role.Role, error) {
	var out *role.Role
	err := r.tx.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		got, err := loadRole(ctx, q, id)
		if err != nil {
			return err
		}
		out = got
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetByTenantAndName satisfies [role.Repository]. RLS-scoped read by
// (tenant_id, name). Live rows only (soft-deleted filtered out).
func (r *RoleRepository) GetByTenantAndName(
	ctx context.Context,
	tenantID tenant.ID,
	name string,
) (*role.Role, error) {
	tid, err := parseTenantIDForRole(tenantID)
	if err != nil {
		return nil, err
	}
	var out *role.Role
	err = r.tx.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		row, err := q.GetRoleByTenantAndName(ctx, GetRoleByTenantAndNameParams{
			TenantID: pgUUID(tid),
			Name:     name,
		})
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return role.ErrNotFound
			}
			return fmt.Errorf("role repo: get by tenant+name: %w", err)
		}
		got, perr := rowToRole(row)
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

// GetByIDs satisfies [role.Repository] — bulk-load for the
// PermissionResolver. RLS still applies — cross-tenant IDs are
// silently dropped from the result set (caller cannot distinguish
// "doesn't exist" from "wrong tenant" through this contract; that's
// intentional per `multi-tenancy.md` "Identity model").
func (r *RoleRepository) GetByIDs(ctx context.Context, ids []role.ID) ([]*role.Role, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	pgIDs := make([]pgtype.UUID, 0, len(ids))
	for _, id := range ids {
		uid, err := uuid.Parse(id.String())
		if err != nil {
			return nil, fmt.Errorf("role repo: parse id %q: %w", id, err)
		}
		pgIDs = append(pgIDs, pgUUID(uid))
	}
	var out []*role.Role
	err := r.tx.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		rows, err := q.GetRolesByIDs(ctx, pgIDs)
		if err != nil {
			return fmt.Errorf("role repo: get by ids: %w", err)
		}
		out = make([]*role.Role, 0, len(rows))
		for _, row := range rows {
			got, perr := rowToRole(row)
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

// ListByTenant satisfies [role.Repository] — full live catalog for the
// supplied tenant, ordered hierarchy_level then name. RLS-scoped.
func (r *RoleRepository) ListByTenant(
	ctx context.Context,
	tenantID tenant.ID,
) ([]*role.Role, error) {
	tid, err := parseTenantIDForRole(tenantID)
	if err != nil {
		return nil, err
	}
	var out []*role.Role
	err = r.tx.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		rows, err := q.ListRolesByTenant(ctx, pgUUID(tid))
		if err != nil {
			return fmt.Errorf("role repo: list by tenant: %w", err)
		}
		out = make([]*role.Role, 0, len(rows))
		for _, row := range rows {
			got, perr := rowToRole(row)
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

// UpdateByID satisfies [role.Repository] — TDL Sep 2024 UpdateFn pattern:
// Load → updateFn → persist (if shouldPersist) → drain events. All in
// one tenant-scoped transaction. RLS naturally refuses cross-tenant
// loads — updateFn never runs under a foreign tenant context.
//
// Persistence branches on aggregate state:
//   - role.IsDeleted() → SoftDeleteRole (sets is_deleted/deleted_at/deleted_by)
//   - otherwise        → UpdateRole (name, hierarchy_level, permissions JSONB)
//
// is_system_default + is_super_admin + tenant_id + created_at are
// aggregate-immutable; the SQL doesn't write them under any branch.
func (r *RoleRepository) UpdateByID(
	ctx context.Context,
	id role.ID,
	updateFn func(*role.Role) (bool, error),
) error {
	return r.tx.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		ro, err := loadRole(ctx, q, id)
		if err != nil {
			return err
		}
		shouldPersist, err := updateFn(ro)
		if err != nil {
			return err
		}
		if !shouldPersist {
			return nil
		}
		if err := persistRoleState(ctx, q, ro); err != nil {
			return err
		}
		return drainRoleEvents(ctx, tx, ro)
	})
}

// ----- Helpers ---------------------------------------------------------------

func loadRole(ctx context.Context, q *Queries, id role.ID) (*role.Role, error) {
	uid, err := uuid.Parse(id.String())
	if err != nil {
		return nil, fmt.Errorf("role repo: parse id %q: %w", id, err)
	}
	row, err := q.GetRoleByID(ctx, pgUUID(uid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, role.ErrNotFound
		}
		return nil, fmt.Errorf("role repo: get by id: %w", err)
	}
	if row.IsDeleted {
		// Soft-deleted rows are forensic-only; live reads must not see them.
		return nil, role.ErrNotFound
	}
	return rowToRole(row)
}

func insertRoleRow(ctx context.Context, q *Queries, ro *role.Role) error {
	rid, err := uuid.Parse(ro.ID().String())
	if err != nil {
		return fmt.Errorf("role repo: parse id %q: %w", ro.ID(), err)
	}
	tid, err := parseTenantIDForRole(ro.TenantID())
	if err != nil {
		return err
	}
	permsJSON, err := encodePermissions(ro.Permissions())
	if err != nil {
		return err
	}
	err = q.InsertRole(ctx, InsertRoleParams{
		ID:              pgUUID(rid),
		TenantID:        pgUUID(tid),
		Name:            ro.Name(),
		IsSystemDefault: ro.IsSystemDefault(),
		IsSuperAdmin:    ro.IsSuperAdmin(),
		// HierarchyLevel is bounded by role.HierarchyLevelMin (0) +
		// role.HierarchyLevelMax (99) per the aggregate's New + ChangeHierarchyLevel
		// invariants. Cast to int32 cannot overflow.
		HierarchyLevel: int32(ro.HierarchyLevel()), //nolint:gosec // G115: bounded [0,99] by aggregate
		Permissions:     permsJSON,
		CreatedAt:       pgRequiredTimestamp(ro.CreatedAt()),
	})
	if err != nil {
		if isRoleNameUniqueViolation(err) {
			return role.ErrNameTaken
		}
		return fmt.Errorf("role repo: insert: %w", err)
	}
	return nil
}

// persistRoleState writes the mutable Role state — name, hierarchy_level,
// permissions OR soft-delete flags — under the aggregate's current
// shape. Caller (UpdateByID) owns the tx + chose to persist.
func persistRoleState(ctx context.Context, q *Queries, ro *role.Role) error {
	rid, err := uuid.Parse(ro.ID().String())
	if err != nil {
		return fmt.Errorf("role repo: parse id %q: %w", ro.ID(), err)
	}
	if ro.IsDeleted() {
		// Soft-delete branch — name + permissions stay as-is in the row;
		// the AND NOT is_deleted predicate on every read filters it out.
		var deletedBy *string
		if by := ro.DeletedBy(); by != "" {
			deletedBy = &by
		}
		err = q.SoftDeleteRole(ctx, SoftDeleteRoleParams{
			ID:        pgUUID(rid),
			DeletedAt: pgRequiredTimestamp(ro.DeletedAt()),
			DeletedBy: deletedBy,
		})
		if err != nil {
			return fmt.Errorf("role repo: soft-delete: %w", err)
		}
		return nil
	}
	permsJSON, err := encodePermissions(ro.Permissions())
	if err != nil {
		return err
	}
	err = q.UpdateRole(ctx, UpdateRoleParams{
		ID:   pgUUID(rid),
		Name: ro.Name(),
		// Bounded [0,99] by role aggregate invariants per insertRoleRow.
		HierarchyLevel: int32(ro.HierarchyLevel()), //nolint:gosec // G115: bounded [0,99] by aggregate
		Permissions:    permsJSON,
	})
	if err != nil {
		if isRoleNameUniqueViolation(err) {
			// Rename collided with another live role's name in the same tenant.
			return role.ErrNameTaken
		}
		return fmt.Errorf("role repo: update: %w", err)
	}
	return nil
}

// drainRoleEvents pulls events off the aggregate, maps each through
// integrationevents.FromDomainEvent, and writes the resulting V1
// records to the outbox. No-op when PullEvents returns nil.
func drainRoleEvents(ctx context.Context, tx pgx.Tx, ro *role.Role) error {
	evs := ro.PullEvents()
	if len(evs) == 0 {
		return nil
	}
	tid, err := parseTenantIDForRole(ro.TenantID())
	if err != nil {
		return err
	}
	asAny := make([]any, len(evs))
	for i, e := range evs {
		asAny[i] = e
	}
	mapped, err := mapDomainEvents(asAny)
	if err != nil {
		return fmt.Errorf("role repo: map events: %w", err)
	}
	return writeOutboxEvents(ctx, tx, tid, mapped)
}

// rowToRole hydrates the aggregate from the sqlc row. UnmarshalFromDB
// trusts the data — no re-validation per TDL canon.
func rowToRole(row IdentityRole) (*role.Role, error) {
	id := role.ID(uuidFromPg(row.ID).String())
	tid := tenant.ID(uuidFromPg(row.TenantID).String())
	perms, err := decodePermissions(row.Permissions)
	if err != nil {
		return nil, fmt.Errorf("role repo: hydrate permissions: %w", err)
	}
	deletedBy := ""
	if row.DeletedBy != nil {
		deletedBy = *row.DeletedBy
	}
	return role.UnmarshalFromDB(role.Snapshot{
		ID:              id,
		TenantID:        tid,
		Name:            row.Name,
		IsSystemDefault: row.IsSystemDefault,
		IsSuperAdmin:    row.IsSuperAdmin,
		HierarchyLevel:  int(row.HierarchyLevel),
		Permissions:     perms,
		CreatedAt:       timeFromPg(row.CreatedAt),
		IsDeleted:       row.IsDeleted,
		DeletedAt:       timeFromPg(row.DeletedAt),
		DeletedBy:       deletedBy,
	}), nil
}

// encodePermissions serialises the role's permission set as a JSONB
// array of name strings. Names are the wire-stable identifier — they
// survive aggregate-internal pointer churn (intern table re-pointing
// across deploys).
func encodePermissions(perms []*permission.Permission) ([]byte, error) {
	names := make([]string, 0, len(perms))
	for _, p := range perms {
		if p == nil {
			continue
		}
		names = append(names, p.Name())
	}
	out, err := json.Marshal(names)
	if err != nil {
		return nil, fmt.Errorf("role repo: encode permissions: %w", err)
	}
	return out, nil
}

// decodePermissions reverses [encodePermissions]. Unknown names are
// rejected — a Role row carrying an unknown permission name is data
// corruption (catalogue is closed-set per `coding-standards.md`
// "Permissions — closed-set construction"); fail-loud beats silent
// privilege-loss.
func decodePermissions(payload []byte) ([]*permission.Permission, error) {
	if len(payload) == 0 {
		return nil, nil
	}
	var names []string
	if err := json.Unmarshal(payload, &names); err != nil {
		return nil, fmt.Errorf("role repo: decode permissions: %w", err)
	}
	out := make([]*permission.Permission, 0, len(names))
	for _, n := range names {
		p, err := permission.TryFromConstant(n)
		if err != nil {
			return nil, fmt.Errorf("role repo: unknown permission %q in row: %w", n, err)
		}
		out = append(out, p)
	}
	return out, nil
}

func parseTenantIDForRole(id tenant.ID) (uuid.UUID, error) {
	parsed, err := uuid.Parse(id.String())
	if err != nil {
		return uuid.Nil, fmt.Errorf("role repo: parse tenant id %q: %w", id, err)
	}
	return parsed, nil
}

// constraintRoleTenantName names the partial unique index protecting
// (tenant_id, name) WHERE NOT is_deleted. Migration 20260507000002
// declares `uq_roles_tenant_name` — this constant is the single source
// of truth for the constraint identifier per `coding-standards.md`
// "No magic strings".
const constraintRoleTenantName = "uq_roles_tenant_name"

// isRoleNameUniqueViolation reports whether err wraps a Postgres
// unique-constraint violation on `uq_roles_tenant_name`. Other unique
// violations bubble up as wrapped errors — they signal a different
// invariant breach and shouldn't masquerade as ErrNameTaken.
func isRoleNameUniqueViolation(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if !ok {
		return false
	}
	if pgErr.Code != pg.SQLStateUniqueViolation {
		return false
	}
	return pgErr.ConstraintName == constraintRoleTenantName
}
