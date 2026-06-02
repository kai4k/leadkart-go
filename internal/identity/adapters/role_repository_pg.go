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

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/identity/adapters/db"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// RoleRepository is the pgx/sqlc-backed [role.Repository]. Tenant-scoped:
// all reads and writes run under [pg.TxScopeTenant]. Cross-tenant operations
// must go through a platform-scoped transactor at a higher layer.
// Hierarchy (parent→child) lives in [rolehierarchy] per ADR 0058 (Wave 9.4).
type RoleRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewRoleRepository wires the repository against a pool and transactor.
func NewRoleRepository(pool *pgxpool.Pool, tx *pg.Transactor) *RoleRepository {
	return &RoleRepository{pool: pool, tx: tx, q: db.New(pool)}
}

// Add satisfies [role.Repository]. Translates SQLSTATE 23505 from
// uq_roles_tenant_name (WHERE NOT is_deleted) to [role.ErrNameTaken].
// GUC bound from ro.TenantID() (ADR 0062).
func (r *RoleRepository) Add(ctx context.Context, ro *role.Role) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.addOnTx(ctx, tx, ro)
	}
	return r.tx.WithinTxPgxTenant(ctx, ro.TenantID().String(), func(ctx context.Context, tx pgx.Tx) error {
		return r.addOnTx(ctx, tx, ro)
	})
}

func (r *RoleRepository) addOnTx(ctx context.Context, tx pgx.Tx, ro *role.Role) error {
	q := r.q.WithTx(tx)
	if err := insertRoleRow(ctx, q, ro); err != nil {
		return err
	}
	return drainRoleEvents(ctx, tx, ro)
}

// GetByID satisfies [role.Repository]. GUC bound from tenantID (ADR 0062).
func (r *RoleRepository) GetByID(ctx context.Context, tenantID tenant.ID, id role.ID) (*role.Role, error) {
	var out *role.Role
	err := r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
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

// GetByTenantAndName satisfies [role.Repository]. Live rows only.
// GUC bound from tenantID (ADR 0062).
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
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		row, err := q.GetRoleByTenantAndName(ctx, db.GetRoleByTenantAndNameParams{
			TenantID: pgconv.PgUUID(tid),
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

// GetByIDs satisfies [role.Repository]. Bulk-load scoped to tenantID;
// RLS silently drops cross-tenant IDs from the result.
func (r *RoleRepository) GetByIDs(ctx context.Context, tenantID tenant.ID, ids []role.ID) ([]*role.Role, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	pgIDs := make([]pgtype.UUID, 0, len(ids))
	for _, id := range ids {
		uid, err := uuid.Parse(id.String())
		if err != nil {
			return nil, fmt.Errorf("role repo: parse id %q: %w", id, err)
		}
		pgIDs = append(pgIDs, pgconv.PgUUID(uid))
	}
	var out []*role.Role
	err := r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
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

// ListByTenant satisfies [role.Repository]. Returns live roles ordered by
// hierarchy_level then name. GUC bound from tenantID.
func (r *RoleRepository) ListByTenant(
	ctx context.Context,
	tenantID tenant.ID,
) ([]*role.Role, error) {
	tid, err := parseTenantIDForRole(tenantID)
	if err != nil {
		return nil, err
	}
	var out []*role.Role
	err = r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		rows, err := q.ListRolesByTenant(ctx, pgconv.PgUUID(tid))
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

// UpdateByID satisfies [role.Repository]. TDL UpdateFn pattern.
// Branches on IsDeleted: true → SoftDeleteRole; false → UpdateRole.
// Immutable fields (is_system_default, is_super_admin, tenant_id,
// created_at) are never written.
func (r *RoleRepository) UpdateByID(
	ctx context.Context,
	tenantID tenant.ID,
	id role.ID,
	updateFn func(*role.Role) (bool, error),
) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.updateOnTx(ctx, tx, id, updateFn)
	}
	return r.tx.WithinTxPgxTenant(ctx, tenantID.String(), func(ctx context.Context, tx pgx.Tx) error {
		return r.updateOnTx(ctx, tx, id, updateFn)
	})
}

func (r *RoleRepository) updateOnTx(
	ctx context.Context,
	tx pgx.Tx,
	id role.ID,
	updateFn func(*role.Role) (bool, error),
) error {
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
}

// ----- Helpers ---------------------------------------------------------------

func loadRole(ctx context.Context, q *db.Queries, id role.ID) (*role.Role, error) {
	uid, err := uuid.Parse(id.String())
	if err != nil {
		return nil, fmt.Errorf("role repo: parse id %q: %w", id, err)
	}
	row, err := q.GetRoleByID(ctx, pgconv.PgUUID(uid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, role.ErrNotFound
		}
		return nil, fmt.Errorf("role repo: get by id: %w", err)
	}
	if row.IsDeleted {
		// Soft-deleted rows are forensic-only.
		return nil, role.ErrNotFound
	}
	return rowToRole(row)
}

func insertRoleRow(ctx context.Context, q *db.Queries, ro *role.Role) error {
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
	err = q.InsertRole(ctx, db.InsertRoleParams{
		ID:              pgconv.PgUUID(rid),
		TenantID:        pgconv.PgUUID(tid),
		Name:            ro.Name(),
		IsSystemDefault: ro.IsSystemDefault(),
		IsSuperAdmin:    ro.IsSuperAdmin(),
		// HierarchyLevel bounded [0,99] by aggregate invariants; int32 safe.
		HierarchyLevel: int32(ro.HierarchyLevel()), //nolint:gosec // G115: bounded [0,99] by aggregate
		Permissions:    permsJSON,
		CreatedAt:      pgconv.PgRequiredTimestamp(ro.CreatedAt()),
	})
	if err != nil {
		if isRoleNameUniqueViolation(err) {
			return role.ErrNameTaken
		}
		return fmt.Errorf("role repo: insert: %w", err)
	}
	return nil
}

// persistRoleState writes mutable Role state (name, hierarchy_level,
// permissions) or soft-delete flags, depending on aggregate state.
func persistRoleState(ctx context.Context, q *db.Queries, ro *role.Role) error {
	rid, err := uuid.Parse(ro.ID().String())
	if err != nil {
		return fmt.Errorf("role repo: parse id %q: %w", ro.ID(), err)
	}
	if ro.IsDeleted() {
		// Soft-delete: name + permissions stay; AND NOT is_deleted filters them.
		deletedBy := pgconv.ZeroToNil(ro.DeletedBy())
		err = q.SoftDeleteRole(ctx, db.SoftDeleteRoleParams{
			ID:        pgconv.PgUUID(rid),
			DeletedAt: pgconv.PgRequiredTimestamp(ro.DeletedAt()),
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
	err = q.UpdateRole(ctx, db.UpdateRoleParams{
		ID:   pgconv.PgUUID(rid),
		Name: ro.Name(),
		// Bounded [0,99] by aggregate invariants.
		HierarchyLevel: int32(ro.HierarchyLevel()), //nolint:gosec // G115: bounded [0,99] by aggregate
		Permissions:    permsJSON,
	})
	if err != nil {
		if isRoleNameUniqueViolation(err) {
			return role.ErrNameTaken
		}
		return fmt.Errorf("role repo: update: %w", err)
	}
	return nil
}

// drainRoleEvents maps aggregate events to V1 integration events and
// writes them to the outbox. No-op when PullEvents returns nil.
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
// trusts the data without re-validation (TDL canon).
func rowToRole(row db.IdentityRole) (*role.Role, error) {
	id := role.ID(pgconv.UUIDFromPg(row.ID).String())
	tid := tenant.ID(pgconv.UUIDFromPg(row.TenantID).String())
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
		CreatedAt:       pgconv.TimeFromPg(row.CreatedAt),
		IsDeleted:       row.IsDeleted,
		DeletedAt:       pgconv.TimeFromPg(row.DeletedAt),
		DeletedBy:       deletedBy,
	}), nil
}

// encodePermissions serialises the role's permission set as a JSONB array
// of name strings (wire-stable identifiers).
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

// decodePermissions reverses [encodePermissions]. Unknown permission names
// are data corruption (closed-set catalogue); fails loudly.
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

// constraintRoleTenantName is the partial unique index name for
// (tenant_id, name) WHERE NOT is_deleted (migration 20260507000002).
const constraintRoleTenantName = "uq_roles_tenant_name"

// isRoleNameUniqueViolation reports whether err is a unique-constraint
// violation specifically on uq_roles_tenant_name.
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
