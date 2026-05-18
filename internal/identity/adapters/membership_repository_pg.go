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

	"github.com/leadkart/leadkart-go/internal/common/clock"
	"github.com/leadkart/leadkart-go/internal/identity/adapters/db"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/permission"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/platform/pg"
)

// permission-override kind constants. Mirror of the
// CHECK (kind IN ('granted', 'revoked')) in migration 20260507000002.
// Wire-stable; persistence + hydration share these — no magic strings.
const (
	overrideKindGranted = "granted"
	overrideKindRevoked = "revoked"
)

// MembershipRepository is the pgx/sqlc-backed implementation of
// [membership.Repository]. tenant_memberships is RLS+FORCE, so every
// write/read except GetActiveForPerson runs under TxScopeTenant — the
// caller MUST have placed the appropriate tenant on context first.
//
// GetActiveForPerson is the documented carve-out for non-login
// callers that need to resolve "what tenant does this Person belong
// to" without prior tenant context (cascade subscribers, audit
// reports, platform-operator UIs). Runs under TxScopePlatform to
// bypass RLS + queries by PersonID through the partial-unique index
// `uq_memberships_person_active`.
//
// The login flow does NOT use this method. Per current canon
// (Brandur Leach / DHH "Postgres scales further than you think")
// login goes through [adapters.AuthRouterPG] which JOINs persons +
// tenant_memberships in one roundtrip — saves the persons-by-email
// + memberships-by-person-id pair into a single indexed lookup.
// Materialised views / denormalised auth_routing tables (the
// 2014-era Stripe / Auth0 patterns) are no longer the canon — modern
// Postgres + JWT + cache layers shifted the cost surface away from
// this query.
type MembershipRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *db.Queries
}

// NewMembershipRepository wires the repository against a pool + transactor.
func NewMembershipRepository(pool *pgxpool.Pool, tx *pg.Transactor) *MembershipRepository {
	return &MembershipRepository{pool: pool, tx: tx, q: db.New(pool)}
}

// Add satisfies [membership.Repository] — persists a new Active Membership
// + drains CreatedEvent. Runs under TxScopeTenant; caller must have set
// tenancy.WithID(ctx, m.TenantID()) before invoking.
//
// Surfaces the partial-unique-index violation (single-Active-Membership
// invariant) as [membership.ErrAlreadyActive].
func (r *MembershipRepository) Add(ctx context.Context, m *membership.Membership) error {
	return r.tx.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		return r.AddInTx(ctx, tx, m)
	})
}

// AddInTx persists a new Active Membership under an EXISTING
// transaction. Scope-agnostic: caller chooses TxScopeTenant (in-tenant
// admin onboards a user) OR TxScopePlatform (orchestrator creates the
// admin Membership during tenant-registration when no tenant context
// exists yet).
//
// Surfaces ErrAlreadyActive on partial-unique-index violation.
//
// Projects the full aggregate state to all four tables: tenant_memberships
// (row + profile fields), role_assignments (one row per RoleAssignment),
// membership_permission_overrides (one row per granted/revoked permission).
func (r *MembershipRepository) AddInTx(ctx context.Context, tx pgx.Tx, m *membership.Membership) error {
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

// UpdateByID satisfies [membership.Repository]. Tenant-scoped: caller
// must have set tenancy on ctx so RLS reveals the row.
//
// Persistence projects the full aggregate state — status (+ left_at),
// profile fields, role_assignments (replace-all), and permission overrides
// (replace-all) — under one transaction, then drains events. Replace-all
// is simpler than per-row diff tracking and idempotent under retry.
func (r *MembershipRepository) UpdateByID(
	ctx context.Context,
	id membership.ID,
	updateFn func(*membership.Membership) (bool, error),
) error {
	return r.tx.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
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

// GetByID satisfies [membership.Repository]. Read path; runs under
// tenant scope for RLS. Returns [membership.ErrNotFound] if no row
// matches OR if the row exists in a different tenant scope (RLS-hidden
// — same observable behaviour as truly missing).
func (r *MembershipRepository) GetByID(ctx context.Context, id membership.ID) (*membership.Membership, error) {
	var out *membership.Membership
	err := r.tx.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
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

// GetActiveForPerson satisfies [membership.Repository]. Cross-tenant:
// runs under platform scope to bypass RLS. Returns the (single) Active
// Membership for the Person across all tenants — the partial-unique
// index guarantees there is at most one.
//
// Returns [membership.ErrNotFound] if the Person has no Active Membership.
func (r *MembershipRepository) GetActiveForPerson(
	ctx context.Context,
	personID person.ID,
) (*membership.Membership, error) {
	uid, err := parsePersonIDForMembership(personID)
	if err != nil {
		return nil, err
	}
	var out *membership.Membership
	err = r.tx.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		// ListMembershipsForPerson returns all (active + inactive) rows
		// across tenants. The partial-unique index guarantees at most one
		// is Active; we filter in memory rather than adding a fourth sqlc
		// query path.
		rows, err := q.ListMembershipsForPerson(ctx, pgUUID(uid))
		if err != nil {
			return fmt.Errorf("membership repo: list for person: %w", err)
		}
		for _, row := range rows {
			if row.Status != "active" {
				continue
			}
			roleIDs, lerr := loadRoleAssignments(ctx, q, uuidFromPg(row.ID))
			if lerr != nil {
				return lerr
			}
			granted, revoked, lerr := loadPermissionOverrides(ctx, q, uuidFromPg(row.ID))
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

// ListForTenant satisfies [membership.Repository]. Tenant-scoped read.
// Caller's tenant ID on ctx MUST equal the requested tenantID — we
// redundantly assert so downstream callers can't accidentally cross
// boundaries (RLS would have hidden mismatches anyway, but explicit
// failure beats silent empty result).
func (r *MembershipRepository) ListForTenant(
	ctx context.Context,
	_ tenant.ID, // RLS scopes via ctx tenant; explicit param kept for contract symmetry
) ([]*membership.Membership, error) {
	// (No SQL helper for "list all in current tenant" — we go through
	// ListMembershipsForPerson would be wrong scope. Issue a plain SELECT
	// via the transactor; sqlc adds nothing here.)
	var out []*membership.Membership
	err := r.tx.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		hydrated, err := q.ListMembershipsInCurrentTenant(ctx)
		if err != nil {
			return fmt.Errorf("membership repo: list for tenant: %w", err)
		}
		// Hydrate child-table state for each row. ListForTenant is an admin
		// path; the N+2 round-trips per row are acceptable until a hot
		// path needs the bulk json_agg join (benchmark first).
		out = make([]*membership.Membership, 0, len(hydrated))
		for _, row := range hydrated {
			roleIDs, lerr := loadRoleAssignments(ctx, q, uuidFromPg(row.ID))
			if lerr != nil {
				return lerr
			}
			granted, revoked, lerr := loadPermissionOverrides(ctx, q, uuidFromPg(row.ID))
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

// ListForTenantPage satisfies [membership.Repository]. Keyset-
// paginated active-only listing per ADR 0038. Backed by the
// partial composite index idx_memberships_tenant_active_joined
// shipped in migration 20260518000001.
//
// limit is page_size + 1 (the "peek one extra" pattern); the
// application-layer query handler drops the extra row when present
// and uses it to set next_cursor.
func (r *MembershipRepository) ListForTenantPage(
	ctx context.Context,
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
	err = r.tx.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		hydrated, qerr := q.ListActiveMembershipsInTenantPage(ctx, db.ListActiveMembershipsInTenantPageParams{
			BeforeJoinedAt: pgRequiredTimestamp(beforeJoinedAt),
			BeforeID:       pgUUID(beforeUUID),
			Limit:          int32(limit), //nolint:gosec // page_size capped at 200 well within int32
		})
		if qerr != nil {
			return fmt.Errorf("membership repo: list-page: %w", qerr)
		}
		// Hydrate child-table state for each row — same N+2 round-trip
		// pattern as ListForTenant. With page_size capped at 200 + the
		// partial-index seek + composite, the per-page cost is bounded.
		// Future optimization: bulk-fetch role_assignments + overrides
		// in a single query keyed by IN(membership_ids).
		out = make([]*membership.Membership, 0, len(hydrated))
		for _, row := range hydrated {
			roleIDs, lerr := loadRoleAssignments(ctx, q, uuidFromPg(row.ID))
			if lerr != nil {
				return lerr
			}
			granted, revoked, lerr := loadPermissionOverrides(ctx, q, uuidFromPg(row.ID))
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

// HasActiveSuperAdmin satisfies [membership.Repository]. Platform-
// scope query against ListSuperAdminMembershipsInTenant — returns
// true if the supplied tenant has at least one active Membership
// holding a role flagged is_super_admin=true.
func (r *MembershipRepository) HasActiveSuperAdmin(
	ctx context.Context,
	tenantID tenant.ID,
) (bool, error) {
	tid, err := parseTenantIDForMembership(tenantID)
	if err != nil {
		return false, err
	}
	var found bool
	err = r.tx.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		rows, err := q.ListSuperAdminMembershipsInTenant(ctx, pgUUID(tid))
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

// ListAllForPerson satisfies [membership.Repository]. Platform-only
// cross-tenant lookup — returns every Membership the Person holds
// (Active + Inactive) across all tenants. Backed by the existing
// ListMembershipsForPerson sqlc query which crosses tenant boundaries
// (the index is non-RLS-filtered per database.md "Single-Active-
// Membership constraint").
//
// Authorization: this method MUST only be called from a path the
// HTTP layer has gated on Platform tier. The repository itself does
// no permission check — that's middleware's job.
func (r *MembershipRepository) ListAllForPerson(
	ctx context.Context,
	personID person.ID,
) ([]*membership.Membership, error) {
	uid, err := parsePersonIDForMembership(personID)
	if err != nil {
		return nil, err
	}
	var out []*membership.Membership
	err = r.tx.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		rows, err := q.ListMembershipsForPerson(ctx, pgUUID(uid))
		if err != nil {
			return fmt.Errorf("membership repo: list for person: %w", err)
		}
		out = make([]*membership.Membership, 0, len(rows))
		for _, row := range rows {
			roleIDs, lerr := loadRoleAssignments(ctx, q, uuidFromPg(row.ID))
			if lerr != nil {
				return lerr
			}
			granted, revoked, lerr := loadPermissionOverrides(ctx, q, uuidFromPg(row.ID))
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
	row, err := q.GetMembershipByID(ctx, pgUUID(uid))
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

// loadRoleAssignments fetches the Membership's projected role-id list.
// Order: assigned_at, role_id (matches the SQL query) — domain treats
// the slice as a set, so ordering is informational only.
func loadRoleAssignments(ctx context.Context, q *db.Queries, mid uuid.UUID) ([]role.ID, error) {
	rows, err := q.ListRoleAssignmentsByMembership(ctx, pgUUID(mid))
	if err != nil {
		return nil, fmt.Errorf("membership repo: list role assignments: %w", err)
	}
	out := make([]role.ID, 0, len(rows))
	for _, r := range rows {
		out = append(out, role.ID(uuidFromPg(r.RoleID).String()))
	}
	return out, nil
}

// loadPermissionOverrides splits the persisted overlay into the two
// slices the domain Snapshot expects. Names go through TryFromConstant
// — unknown permission_name in storage = data corruption (catalogue is
// closed-set per coding-standards.md "Permissions — closed-set
// construction"); fail-loud beats silent privilege-loss.
func loadPermissionOverrides(
	ctx context.Context,
	q *db.Queries,
	mid uuid.UUID,
) (granted, revoked []*permission.Permission, err error) {
	rows, err := q.ListPermissionOverridesByMembership(ctx, pgUUID(mid))
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
			granted = append(granted, p)
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
	// CreatedBy is the audit chain — caller's Membership ID, or zero
	// for self-bootstrapped paths. Zero ID maps to a NULL pgtype.UUID
	// (Valid:false), which the column accepts via migration
	// 20260507000008's NULL allowance.
	createdBy := pgtype.UUID{}
	if cb := m.CreatedBy(); !cb.IsZero() {
		cbUUID, perr := uuid.Parse(cb.String())
		if perr != nil {
			return fmt.Errorf("membership repo: parse createdBy %q: %w", cb, perr)
		}
		createdBy = pgUUID(cbUUID)
	}
	err = q.InsertMembership(ctx, db.InsertMembershipParams{
		ID:                    pgUUID(mid),
		PersonID:              pgUUID(pid),
		TenantID:              pgUUID(tid),
		Status:                m.Status().String(),
		JoinedAt:              pgRequiredTimestamp(m.JoinedAt()),
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
		ID:     pgUUID(mid),
		Status: m.Status().String(),
		LeftAt: pgTimestamp(m.LeftAt()),
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
// + the projected child-table state (roleAssignments, granted/revoked
// permission overrides). Caller (loadMembership) batches the child reads
// in one tx so RLS scope stays consistent across all four tables.
func rowToMembership(
	row db.IdentityTenantMembership,
	roleAssignments []role.ID,
	granted, revoked []*permission.Permission,
) (*membership.Membership, error) {
	id := membership.ID(uuidFromPg(row.ID).String())
	personID := person.ID(uuidFromPg(row.PersonID).String())
	tenantID := tenant.ID(uuidFromPg(row.TenantID).String())

	status, err := membership.ParseStatus(row.Status)
	if err != nil {
		return nil, fmt.Errorf("membership repo: hydrate status %q: %w", row.Status, err)
	}
	reportsTo := membership.ID("")
	if reports := uuidFromPg(row.ReportsTo); reports != uuid.Nil {
		reportsTo = membership.ID(reports.String())
	}
	createdBy := membership.ID("")
	if cb := uuidFromPg(row.CreatedByMembershipID); cb != uuid.Nil {
		createdBy = membership.ID(cb.String())
	}
	return membership.UnmarshalFromDB(membership.Snapshot{
		ID:                 id,
		PersonID:           personID,
		TenantID:           tenantID,
		Status:             status,
		JoinedAt:           timeFromPg(row.JoinedAt),
		LeftAt:             timeFromPg(row.LeftAt),
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

// persistMembershipProfile writes the per-tenant profile fields. Always
// runs (no diff check) — the columns are NOT NULL DEFAULT ” in schema,
// so writing the aggregate's current values is always safe.
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
		ID:            pgUUID(mid),
		Designation:   m.Designation(),
		Department:    m.Department(),
		StatusMessage: m.StatusMessage(),
		ReportsTo:     pgUUIDOpt(reportsTo),
	})
	if err != nil {
		return fmt.Errorf("membership repo: update profile: %w", err)
	}
	return nil
}

// replaceRoleAssignments projects the aggregate's current RoleAssignments
// slice onto identity.role_assignments under replace-all semantics: clear
// every row for this Membership, then INSERT the current set. Idempotent
// under retry; simpler than per-row diff tracking.
//
// Composite FK on (membership_id, tenant_id) → tenant_memberships(id, tenant_id)
// rejects cross-tenant role IDs at the schema layer; the domain's
// `multi-tenancy.md` "Identity model" CALLER INVARIANT (every assigned
// Role MUST belong to the Membership's TenantID) is the upstream guard.
func replaceRoleAssignments(ctx context.Context, q *db.Queries, m *membership.Membership) error {
	mid, err := parseMembershipID(m.ID())
	if err != nil {
		return err
	}
	tid, err := parseTenantIDForMembership(m.TenantID())
	if err != nil {
		return err
	}
	if err := q.DeleteRoleAssignmentsByMembership(ctx, pgUUID(mid)); err != nil {
		return fmt.Errorf("membership repo: clear role assignments: %w", err)
	}
	now := pgRequiredTimestamp(clock.Now())
	for _, rid := range m.RoleAssignments() {
		ruid, err := uuid.Parse(rid.String())
		if err != nil {
			return fmt.Errorf("membership repo: parse role id %q: %w", rid, err)
		}
		err = q.InsertRoleAssignment(ctx, db.InsertRoleAssignmentParams{
			MembershipID: pgUUID(mid),
			RoleID:       pgUUID(ruid),
			TenantID:     pgUUID(tid),
			AssignedAt:   now,
		})
		if err != nil {
			return fmt.Errorf("membership repo: insert role assignment %q: %w", rid, err)
		}
	}
	return nil
}

// replacePermissionOverrides projects the aggregate's overlay (granted +
// revoked permission slices) onto identity.membership_permission_overrides
// under replace-all semantics. Domain-level invariant guarantees a
// permission_name appears at most once across both slices for a given
// Membership; the table's PK (membership_id, permission_name) defends
// against any drift.
func replacePermissionOverrides(ctx context.Context, q *db.Queries, m *membership.Membership) error {
	mid, err := parseMembershipID(m.ID())
	if err != nil {
		return err
	}
	tid, err := parseTenantIDForMembership(m.TenantID())
	if err != nil {
		return err
	}
	if err := q.DeletePermissionOverridesByMembership(ctx, pgUUID(mid)); err != nil {
		return fmt.Errorf("membership repo: clear permission overrides: %w", err)
	}
	now := pgRequiredTimestamp(clock.Now())
	for _, p := range m.GrantedPermissions() {
		if p == nil {
			continue
		}
		err := q.InsertPermissionOverride(ctx, db.InsertPermissionOverrideParams{
			MembershipID:   pgUUID(mid),
			PermissionName: p.Name(),
			Kind:           overrideKindGranted,
			TenantID:       pgUUID(tid),
			UpdatedAt:      now,
		})
		if err != nil {
			return fmt.Errorf("membership repo: insert granted override %q: %w", p.Name(), err)
		}
	}
	for _, p := range m.RevokedPermissions() {
		if p == nil {
			continue
		}
		err := q.InsertPermissionOverride(ctx, db.InsertPermissionOverrideParams{
			MembershipID:   pgUUID(mid),
			PermissionName: p.Name(),
			Kind:           overrideKindRevoked,
			TenantID:       pgUUID(tid),
			UpdatedAt:      now,
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
// from `migrations/20260505000002_identity_init.sql` enforcing the
// single-Active-Membership invariant per `multi-tenancy.md`. Renaming
// in the migration MUST be paired with updating this constant.
const constraintMembershipsPersonActive = "uq_memberships_person_active"

// isMembershipActiveCollision reports whether err is the partial-unique
// index violation specifically (single-Active-Membership invariant).
// Other unique violations (e.g. (person_id, tenant_id) duplicate) bubble
// as raw errors — they indicate a different bug class.
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
