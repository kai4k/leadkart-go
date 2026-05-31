package adapters

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/identity/adapters/db"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// permission-override kind constants — mirror of the
// CHECK (kind IN ('granted', 'revoked')) in migration 20260507000002.
const (
	overrideKindGranted = "granted"
	overrideKindRevoked = "revoked"
)

// MembershipRepository is the pgx/sqlc-backed [membership.Repository].
// tenant_memberships is RLS+FORCE; every method except GetActiveForPerson
// runs under TxScopeTenant.
//
// GetActiveForPerson runs under TxScopePlatform to bypass RLS for
// callers (cascade subscribers, audit, platform UI) that need to resolve
// "which tenant does this Person belong to" without prior tenant context.
// Queries via the partial-unique index uq_memberships_person_active.
//
// Login does NOT use GetActiveForPerson — it goes through
// [adapters.AuthRouterPG] (single-roundtrip JOIN).
type MembershipRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewMembershipRepository wires the repository against a pool + transactor.
func NewMembershipRepository(pool *pgxpool.Pool, tx *pg.Transactor) *MembershipRepository {
	return &MembershipRepository{pool: pool, tx: tx, q: db.New(pool)}
}

// Add satisfies [membership.Repository]. Joins an ambient UoW tx when
// present; otherwise opens a tenant-scoped tx (caller must bind tenancy
// on ctx). Surfaces the partial-unique-index violation as
// [membership.ErrAlreadyActive].
func (r *MembershipRepository) Add(ctx context.Context, m *membership.Membership) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return r.addOnTx(ctx, tx, m)
	}
	return r.tx.WithinTxPgxTenant(ctx, m.TenantID().String(), func(ctx context.Context, tx pgx.Tx) error {
		return r.addOnTx(ctx, tx, m)
	})
}

// addOnTx writes the membership row, profile, role_assignments, and
// permission_overrides, then drains events. Unexported.
func (r *MembershipRepository) addOnTx(ctx context.Context, tx pgx.Tx, m *membership.Membership) error {
	q := r.q.WithTx(tx)
	if err := insertMembershipRow(ctx, q, m); err != nil {
		return err
	}
	if err := persistMembershipProfile(ctx, q, m); err != nil {
		return err
	}
	if err := replaceRoleAssignments(ctx, q, m); err != nil {
		return err
	}
	if err := replacePermissionOverrides(ctx, q, m); err != nil {
		return err
	}
	return drainMembershipEvents(ctx, tx, m)
}

// runInTxTenant joins an ambient UoW tx when present, else opens a
// tenant-scoped tx bound to tenantID (ADR 0067 Phase-4 UoW-join contract).
func (r *MembershipRepository) runInTxTenant(ctx context.Context, tenantID tenant.ID, fn func(ctx context.Context, tx pgx.Tx) error) error {
	if tx, ok := pg.TxFromContext(ctx); ok {
		return fn(ctx, tx)
	}
	return r.tx.WithinTxPgxTenant(ctx, tenantID.String(), fn)
}

// UpdateByID satisfies [membership.Repository]. Persists full aggregate
// state (status, profile, role_assignments, permission_overrides) under
// replace-all semantics, then drains events.
func (r *MembershipRepository) UpdateByID(
	ctx context.Context,
	tenantID tenant.ID,
	id membership.ID,
	updateFn func(*membership.Membership) (bool, error),
) error {
	return r.runInTxTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		m, err := loadMembership(ctx, q, id)
		if err != nil {
			return err
		}
		shouldPersist, err := updateFn(m)
		if err != nil {
			return err
		}
		if !shouldPersist {
			return nil
		}
		if err := persistMembershipStatus(ctx, q, m); err != nil {
			return err
		}
		if err := persistMembershipProfile(ctx, q, m); err != nil {
			return err
		}
		if err := replaceRoleAssignments(ctx, q, m); err != nil {
			return err
		}
		if err := replacePermissionOverrides(ctx, q, m); err != nil {
			return err
		}
		return drainMembershipEvents(ctx, tx, m)
	})
}

// GetByID satisfies [membership.Repository]. Returns [membership.ErrNotFound]
// when no row matches or the row is RLS-hidden by a different tenant scope.
func (r *MembershipRepository) GetByID(ctx context.Context, tenantID tenant.ID, id membership.ID) (*membership.Membership, error) {
	var out *membership.Membership
	err := r.runInTxTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		m, err := loadMembership(ctx, r.q.WithTx(tx), id)
		if err != nil {
			return err
		}
		out = m
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// GetActiveForPerson satisfies [membership.Repository]. Platform-scoped
// (bypasses RLS). Returns the single Active Membership for the Person, or
// [membership.ErrNotFound] when none exists.
func (r *MembershipRepository) GetActiveForPerson(
	ctx context.Context,
	personID person.ID,
) (*membership.Membership, error) {
	uid, err := parsePersonIDForMembership(personID)
	if err != nil {
		return nil, err
	}
	var out *membership.Membership
	err = r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		// ListMembershipsForPerson returns all rows (active + inactive)
		// across tenants. Filter active in memory; the partial-unique
		// index guarantees at most one.
		rows, err := q.ListMembershipsForPerson(ctx, pgconv.PgUUID(uid))
		if err != nil {
			return fmt.Errorf("membership repo: list for person: %w", err)
		}
		for _, row := range rows {
			if row.Status != membership.StatusActive.String() {
				continue
			}
			roleIDs, lerr := loadRoleAssignments(ctx, q, pgconv.UUIDFromPg(row.ID))
			if lerr != nil {
				return lerr
			}
			granted, revoked, lerr := loadPermissionOverrides(ctx, q, pgconv.UUIDFromPg(row.ID))
			if lerr != nil {
				return lerr
			}
			m, perr := rowToMembership(row, roleIDs, granted, revoked)
			if perr != nil {
				return perr
			}
			out = m
			return nil
		}
		return membership.ErrNotFound
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListForTenant satisfies [membership.Repository]. RLS isolates rows to
// the bound tenant; mismatches return empty rather than error.
func (r *MembershipRepository) ListForTenant(
	ctx context.Context,
	tenantID tenant.ID,
) ([]*membership.Membership, error) {
	// ListMembershipsInCurrentTenant is the correct scope here.
	var out []*membership.Membership
	err := r.runInTxTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		hydrated, err := q.ListMembershipsInCurrentTenant(ctx)
		if err != nil {
			return fmt.Errorf("membership repo: list for tenant: %w", err)
		}
		// N+2 hydration per row; acceptable for this admin-path listing.
		out = make([]*membership.Membership, 0, len(hydrated))
		for _, row := range hydrated {
			roleIDs, lerr := loadRoleAssignments(ctx, q, pgconv.UUIDFromPg(row.ID))
			if lerr != nil {
				return lerr
			}
			granted, revoked, lerr := loadPermissionOverrides(ctx, q, pgconv.UUIDFromPg(row.ID))
			if lerr != nil {
				return lerr
			}
			m, perr := rowToMembership(row, roleIDs, granted, revoked)
			if perr != nil {
				return perr
			}
			out = append(out, m)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ListForTenantPage satisfies [membership.Repository]. Keyset-paginated
// active-only listing (ADR 0038); backed by
// idx_memberships_tenant_active_joined. limit = page_size+1 (peek-one-extra).
func (r *MembershipRepository) ListForTenantPage(
	ctx context.Context,
	tenantID tenant.ID,
	beforeJoinedAt time.Time,
	beforeID string,
	limit int,
) ([]*membership.Membership, error) {
	beforeUUID, err := uuid.Parse(beforeID)
	if err != nil {
		return nil, fmt.Errorf("membership repo: list-page: parse before id: %w", err)
	}
	if limit <= 0 {
		return nil, fmt.Errorf("membership repo: list-page: limit must be positive (got %d)", limit)
	}

	var out []*membership.Membership
	err = r.runInTxTenant(ctx, tenantID, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		hydrated, qerr := q.ListActiveMembershipsInTenantPage(ctx, db.ListActiveMembershipsInTenantPageParams{
			BeforeJoinedAt: pgconv.PgRequiredTimestamp(beforeJoinedAt),
			BeforeID:       pgconv.PgUUID(beforeUUID),
			Limit:          int32(limit), //nolint:gosec // page_size capped at 200 well within int32
		})
		if qerr != nil {
			return fmt.Errorf("membership repo: list-page: %w", qerr)
		}
		// N+2 hydration per row; page_size capped at 200 bounds the cost.
		out = make([]*membership.Membership, 0, len(hydrated))
		for _, row := range hydrated {
			roleIDs, lerr := loadRoleAssignments(ctx, q, pgconv.UUIDFromPg(row.ID))
			if lerr != nil {
				return lerr
			}
			granted, revoked, lerr := loadPermissionOverrides(ctx, q, pgconv.UUIDFromPg(row.ID))
			if lerr != nil {
				return lerr
			}
			m, perr := rowToMembership(row, roleIDs, granted, revoked)
			if perr != nil {
				return perr
			}
			out = append(out, m)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// HasActiveSuperAdmin satisfies [membership.Repository]. Platform-scoped;
// returns true when the tenant has at least one active Membership with
// is_super_admin=true.
func (r *MembershipRepository) HasActiveSuperAdmin(
	ctx context.Context,
	tenantID tenant.ID,
) (bool, error) {
	tid, err := parseTenantIDForMembership(tenantID)
	if err != nil {
		return false, err
	}
	var found bool
	err = r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		rows, err := q.ListSuperAdminMembershipsInTenant(ctx, pgconv.PgUUID(tid))
		if err != nil {
			return fmt.Errorf("membership repo: list super-admin in tenant: %w", err)
		}
		found = len(rows) > 0
		return nil
	})
	if err != nil {
		return false, err
	}
	return found, nil
}

// ListAllForPerson satisfies [membership.Repository]. Platform-scoped
// cross-tenant lookup — returns all Memberships (Active + Inactive).
// Caller must be gated on Platform tier; the repo does not check.
func (r *MembershipRepository) ListAllForPerson(
	ctx context.Context,
	personID person.ID,
) ([]*membership.Membership, error) {
	uid, err := parsePersonIDForMembership(personID)
	if err != nil {
		return nil, err
	}
	var out []*membership.Membership
	err = r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		rows, err := q.ListMembershipsForPerson(ctx, pgconv.PgUUID(uid))
		if err != nil {
			return fmt.Errorf("membership repo: list for person: %w", err)
		}
		out = make([]*membership.Membership, 0, len(rows))
		for _, row := range rows {
			roleIDs, lerr := loadRoleAssignments(ctx, q, pgconv.UUIDFromPg(row.ID))
			if lerr != nil {
				return lerr
			}
			granted, revoked, lerr := loadPermissionOverrides(ctx, q, pgconv.UUIDFromPg(row.ID))
			if lerr != nil {
				return lerr
			}
			m, perr := rowToMembership(row, roleIDs, granted, revoked)
			if perr != nil {
				return perr
			}
			out = append(out, m)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ----- Helpers ---------------------------------------------------------------

func loadMembership(ctx context.Context, q *db.Queries, id membership.ID) (*membership.Membership, error) {
	uid, err := parseMembershipID(id)
	if err != nil {
		return nil, err
	}
	row, err := q.GetMembershipByID(ctx, pgconv.PgUUID(uid))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, membership.ErrNotFound
		}
		return nil, fmt.Errorf("membership repo: get by id: %w", err)
	}
	roleIDs, err := loadRoleAssignments(ctx, q, uid)
	if err != nil {
		return nil, err
	}
	granted, revoked, err := loadPermissionOverrides(ctx, q, uid)
	if err != nil {
		return nil, err
	}
	return rowToMembership(row, roleIDs, granted, revoked)
}

// loadRoleAssignments fetches the Membership's role-id list ordered by
// (assigned_at, role_id). The domain treats the slice as a set.
func loadRoleAssignments(ctx context.Context, q *db.Queries, mid uuid.UUID) ([]role.ID, error) {
	rows, err := q.ListRoleAssignmentsByMembership(ctx, pgconv.PgUUID(mid))
	if err != nil {
		return nil, fmt.Errorf("membership repo: list role assignments: %w", err)
	}
	out := make([]role.ID, 0, len(rows))
	for _, r := range rows {
		out = append(out, role.ID(pgconv.UUIDFromPg(r.RoleID).String()))
	}
	return out, nil
}

// loadPermissionOverrides splits the persisted overlay into granted +
// revoked slices. Unknown permission_name = data corruption (closed-set
// catalogue); fails loudly. Granted entries carry an optional ExpiresAt
// (zero = perpetual); revoked entries do not. ADR 0055.
func loadPermissionOverrides(
	ctx context.Context,
	q *db.Queries,
	mid uuid.UUID,
) (granted []membership.GrantedOverride, revoked []*permission.Permission, err error) {
	rows, err := q.ListPermissionOverridesByMembership(ctx, pgconv.PgUUID(mid))
	if err != nil {
		return nil, nil, fmt.Errorf("membership repo: list permission overrides: %w", err)
	}
	for _, row := range rows {
		p, err := permission.TryFromConstant(row.PermissionName)
		if err != nil {
			return nil, nil, fmt.Errorf("membership repo: unknown permission %q in row: %w",
				row.PermissionName, err)
		}
		switch row.Kind {
		case overrideKindGranted:
			// ADR 0055: ExpiresAt zero (NULL) = perpetual; non-zero = JIT-bounded.
			granted = append(granted, membership.GrantedOverride{
				Permission: p,
				ExpiresAt:  pgconv.TimeFromPg(row.ExpiresAt),
			})
		case overrideKindRevoked:
			revoked = append(revoked, p)
		default:
			return nil, nil, fmt.Errorf("membership repo: unknown override kind %q", row.Kind)
		}
	}
	return granted, revoked, nil
}

func insertMembershipRow(ctx context.Context, q *db.Queries, m *membership.Membership) error {
	mid, err := parseMembershipID(m.ID())
	if err != nil {
		return err
	}
	pid, err := parsePersonIDForMembership(m.PersonID())
	if err != nil {
		return err
	}
	tid, err := parseTenantIDForMembership(m.TenantID())
	if err != nil {
		return err
	}
	// CreatedBy is the audit chain — zero maps to NULL (migration 20260507000008
	// allows NULL for self-bootstrapped paths).
	createdBy := pgtype.UUID{}
	if cb := m.CreatedBy(); !cb.IsZero() {
		cbUUID, perr := uuid.Parse(cb.String())
		if perr != nil {
			return fmt.Errorf("membership repo: parse createdBy %q: %w", cb, perr)
		}
		createdBy = pgconv.PgUUID(cbUUID)
	}
	err = q.InsertMembership(ctx, db.InsertMembershipParams{
		ID:                    pgconv.PgUUID(mid),
		PersonID:              pgconv.PgUUID(pid),
		TenantID:              pgconv.PgUUID(tid),
		Status:                m.Status().String(),
		JoinedAt:              pgconv.PgRequiredTimestamp(m.JoinedAt()),
		CreatedByMembershipID: createdBy,
	})
	if err != nil {
		if isMembershipActiveCollision(err) {
			return membership.ErrAlreadyActive
		}
		return fmt.Errorf("membership repo: insert: %w", err)
	}
	return nil
}

func persistMembershipStatus(ctx context.Context, q *db.Queries, m *membership.Membership) error {
	mid, err := parseMembershipID(m.ID())
	if err != nil {
		return err
	}
	err = q.UpdateMembershipStatus(ctx, db.UpdateMembershipStatusParams{
		ID:     pgconv.PgUUID(mid),
		Status: m.Status().String(),
		LeftAt: pgconv.PgTimestamp(m.LeftAt()),
	})
	if err != nil {
		if isMembershipActiveCollision(err) {
			return membership.ErrAlreadyActive
		}
		return fmt.Errorf("membership repo: update status: %w", err)
	}
	return nil
}

func drainMembershipEvents(ctx context.Context, tx pgx.Tx, m *membership.Membership) error {
	evs := m.PullEvents()
	if len(evs) == 0 {
		return nil
	}
	tid, err := parseTenantIDForMembership(m.TenantID())
	if err != nil {
		return err
	}
	asAny := make([]any, len(evs))
	for i, e := range evs {
		asAny[i] = e
	}
	mapped, err := mapDomainEvents(asAny)
	if err != nil {
		return fmt.Errorf("membership repo: map events: %w", err)
	}
	return writeOutboxEvents(ctx, tx, tid, mapped)
}

// rowToMembership hydrates the Membership aggregate from the parent row
// and child-table state (role_assignments + permission overrides).
func rowToMembership(
	row db.IdentityTenantMembership,
	roleAssignments []role.ID,
	granted []membership.GrantedOverride,
	revoked []*permission.Permission,
) (*membership.Membership, error) {
	id := membership.ID(pgconv.UUIDFromPg(row.ID).String())
	personID := person.ID(pgconv.UUIDFromPg(row.PersonID).String())
	tenantID := tenant.ID(pgconv.UUIDFromPg(row.TenantID).String())

	status, err := membership.ParseStatus(row.Status)
	if err != nil {
		return nil, fmt.Errorf("membership repo: hydrate status %q: %w", row.Status, err)
	}
	reportsTo := membership.ID("")
	if reports := pgconv.UUIDFromPg(row.ReportsTo); reports != uuid.Nil {
		reportsTo = membership.ID(reports.String())
	}
	createdBy := membership.ID("")
	if cb := pgconv.UUIDFromPg(row.CreatedByMembershipID); cb != uuid.Nil {
		createdBy = membership.ID(cb.String())
	}
	return membership.UnmarshalFromDB(membership.Snapshot{
		ID:                 id,
		PersonID:           personID,
		TenantID:           tenantID,
		Status:             status,
		JoinedAt:           pgconv.TimeFromPg(row.JoinedAt),
		LeftAt:             pgconv.TimeFromPg(row.LeftAt),
		RoleAssignments:    roleAssignments,
		GrantedPermissions: granted,
		RevokedPermissions: revoked,
		Designation:        row.Designation,
		Department:         row.Department,
		StatusMessage:      row.StatusMessage,
		ReportsTo:          reportsTo,
		CreatedBy:          createdBy,
	}), nil
}

// persistMembershipProfile writes per-tenant profile fields. Always runs;
// columns are NOT NULL DEFAULT ” so writing current values is safe.
func persistMembershipProfile(ctx context.Context, q *db.Queries, m *membership.Membership) error {
	mid, err := parseMembershipID(m.ID())
	if err != nil {
		return err
	}
	var reportsTo uuid.UUID
	if rt := m.ReportsTo(); !rt.IsZero() {
		parsed, perr := uuid.Parse(rt.String())
		if perr != nil {
			return fmt.Errorf("membership repo: parse reports_to %q: %w", rt, perr)
		}
		reportsTo = parsed
	}
	err = q.UpdateMembershipProfile(ctx, db.UpdateMembershipProfileParams{
		ID:            pgconv.PgUUID(mid),
		Designation:   m.Designation(),
		Department:    m.Department(),
		StatusMessage: m.StatusMessage(),
		ReportsTo:     pgconv.PgUUIDOrNull(reportsTo),
	})
	if err != nil {
		return fmt.Errorf("membership repo: update profile: %w", err)
	}
	return nil
}

// replaceRoleAssignments replaces the Membership's role_assignments using
// replace-all semantics (DELETE + INSERT). Idempotent under retry. The
// composite FK rejects cross-tenant role IDs at the schema layer.
func replaceRoleAssignments(ctx context.Context, q *db.Queries, m *membership.Membership) error {
	mid, err := parseMembershipID(m.ID())
	if err != nil {
		return err
	}
	tid, err := parseTenantIDForMembership(m.TenantID())
	if err != nil {
		return err
	}
	if err := q.DeleteRoleAssignmentsByMembership(ctx, pgconv.PgUUID(mid)); err != nil {
		return fmt.Errorf("membership repo: clear role assignments: %w", err)
	}
	now := pgconv.PgRequiredTimestamp(time.Now().UTC())
	for _, rid := range m.RoleAssignments() {
		ruid, err := uuid.Parse(rid.String())
		if err != nil {
			return fmt.Errorf("membership repo: parse role id %q: %w", rid, err)
		}
		err = q.InsertRoleAssignment(ctx, db.InsertRoleAssignmentParams{
			MembershipID: pgconv.PgUUID(mid),
			RoleID:       pgconv.PgUUID(ruid),
			TenantID:     pgconv.PgUUID(tid),
			AssignedAt:   now,
		})
		if err != nil {
			return fmt.Errorf("membership repo: insert role assignment %q: %w", rid, err)
		}
	}
	return nil
}

// replacePermissionOverrides replaces the Membership's permission overrides
// using replace-all semantics. The PK (membership_id, permission_name)
// defends against any domain-level drift.
func replacePermissionOverrides(ctx context.Context, q *db.Queries, m *membership.Membership) error {
	mid, err := parseMembershipID(m.ID())
	if err != nil {
		return err
	}
	tid, err := parseTenantIDForMembership(m.TenantID())
	if err != nil {
		return err
	}
	if err := q.DeletePermissionOverridesByMembership(ctx, pgconv.PgUUID(mid)); err != nil {
		return fmt.Errorf("membership repo: clear permission overrides: %w", err)
	}
	now := pgconv.PgRequiredTimestamp(time.Now().UTC())
	for _, g := range m.GrantedPermissions() {
		if g.Permission == nil {
			continue
		}
		err := q.InsertPermissionOverride(ctx, db.InsertPermissionOverrideParams{
			MembershipID:   pgconv.PgUUID(mid),
			PermissionName: g.Permission.Name(),
			Kind:           overrideKindGranted,
			TenantID:       pgconv.PgUUID(tid),
			UpdatedAt:      now,
			ExpiresAt:      pgconv.PgTimestamp(g.ExpiresAt), // nil → NULL = perpetual
		})
		if err != nil {
			return fmt.Errorf("membership repo: insert granted override %q: %w", g.Permission.Name(), err)
		}
	}
	for _, p := range m.RevokedPermissions() {
		if p == nil {
			continue
		}
		err := q.InsertPermissionOverride(ctx, db.InsertPermissionOverrideParams{
			MembershipID:   pgconv.PgUUID(mid),
			PermissionName: p.Name(),
			Kind:           overrideKindRevoked,
			TenantID:       pgconv.PgUUID(tid),
			UpdatedAt:      now,
			// Revocations are permanent until re-granted; ExpiresAt stays NULL.
		})
		if err != nil {
			return fmt.Errorf("membership repo: insert revoked override %q: %w", p.Name(), err)
		}
	}
	return nil
}

func parseMembershipID(id membership.ID) (uuid.UUID, error) {
	parsed, err := uuid.Parse(id.String())
	if err != nil {
		return uuid.Nil, fmt.Errorf("membership repo: parse id %q: %w", id, err)
	}
	return parsed, nil
}

func parsePersonIDForMembership(id person.ID) (uuid.UUID, error) {
	parsed, err := uuid.Parse(id.String())
	if err != nil {
		return uuid.Nil, fmt.Errorf("membership repo: parse person id %q: %w", id, err)
	}
	return parsed, nil
}

func parseTenantIDForMembership(id tenant.ID) (uuid.UUID, error) {
	parsed, err := uuid.Parse(id.String())
	if err != nil {
		return uuid.Nil, fmt.Errorf("membership repo: parse tenant id %q: %w", id, err)
	}
	return parsed, nil
}

// constraintMembershipsPersonActive is the partial-unique-index name
// (migration 20260505000002) enforcing the single-Active-Membership invariant.
// Any rename must update this constant.
const constraintMembershipsPersonActive = "uq_memberships_person_active"

// isMembershipActiveCollision reports whether err is the partial-unique-index
// violation on the single-Active-Membership invariant. Other unique violations
// bubble as-is.
func isMembershipActiveCollision(err error) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	if !ok {
		return false
	}
	if pgErr.Code != pg.SQLStateUniqueViolation {
		return false
	}
	return pgErr.ConstraintName == constraintMembershipsPersonActive
}
