package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/platform/pg"
)

// MembershipRepository is the pgx/sqlc-backed implementation of
// [membership.Repository]. tenant_memberships is RLS+FORCE, so every
// write/read except GetActiveForPerson runs under TxScopeTenant — the
// caller MUST have placed the appropriate tenant on context first.
//
// GetActiveForPerson is the documented carve-out (login flow needs to
// resolve a Person's home tenant before any tenant context exists). It
// runs under TxScopePlatform to bypass RLS and queries by PersonID
// directly. When the auth_routing index ships, this method swaps to
// the index lookup with no caller change.
type MembershipRepository struct {
	pool *pgxpool.Pool
	tx   *pg.Transactor
	q    *Queries
}

// NewMembershipRepository wires the repository against a pool + transactor.
func NewMembershipRepository(pool *pgxpool.Pool, tx *pg.Transactor) *MembershipRepository {
	return &MembershipRepository{pool: pool, tx: tx, q: New(pool)}
}

// Add satisfies [membership.Repository] — persists a new Active Membership
// + drains CreatedEvent. Runs under TxScopeTenant; caller must have set
// tenancy.WithID(ctx, m.TenantID()) before invoking.
//
// Surfaces the partial-unique-index violation (single-Active-Membership
// invariant) as [membership.ErrAlreadyActive].
func (r *MembershipRepository) Add(ctx context.Context, m *membership.Membership) error {
	return r.tx.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		if err := insertMembershipRow(ctx, q, m); err != nil {
			return err
		}
		return drainMembershipEvents(ctx, tx, m)
	})
}

// UpdateByID satisfies [membership.Repository]. Tenant-scoped: caller
// must have set tenancy on ctx so RLS reveals the row.
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
			if row.Status == "active" {
				m, perr := rowToMembership(row)
				if perr != nil {
					return perr
				}
				out = m
				return nil
			}
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
	tenantID tenant.ID,
) ([]*membership.Membership, error) {
	// (No SQL helper for "list all in current tenant" — we go through
	// ListMembershipsForPerson would be wrong scope. Issue a plain SELECT
	// via the transactor; sqlc adds nothing here.)
	var out []*membership.Membership
	err := r.tx.WithinTx(ctx, pg.TxScopeTenant, func(ctx context.Context, tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			SELECT id, person_id, tenant_id, status, joined_at, left_at
			FROM   identity.tenant_memberships
		`)
		if err != nil {
			return fmt.Errorf("membership repo: list for tenant: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var row IdentityTenantMembership
			if err := rows.Scan(
				&row.ID, &row.PersonID, &row.TenantID,
				&row.Status, &row.JoinedAt, &row.LeftAt,
			); err != nil {
				return fmt.Errorf("membership repo: scan: %w", err)
			}
			m, perr := rowToMembership(row)
			if perr != nil {
				return perr
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// ----- Helpers ---------------------------------------------------------------

func loadMembership(ctx context.Context, q *Queries, id membership.ID) (*membership.Membership, error) {
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
	return rowToMembership(row)
}

func insertMembershipRow(ctx context.Context, q *Queries, m *membership.Membership) error {
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
	err = q.InsertMembership(ctx, InsertMembershipParams{
		ID:       pgUUID(mid),
		PersonID: pgUUID(pid),
		TenantID: pgUUID(tid),
		Status:   m.Status().String(),
		JoinedAt: pgRequiredTimestamp(m.JoinedAt()),
	})
	if err != nil {
		if isMembershipActiveCollision(err) {
			return membership.ErrAlreadyActive
		}
		return fmt.Errorf("membership repo: insert: %w", err)
	}
	return nil
}

func persistMembershipStatus(ctx context.Context, q *Queries, m *membership.Membership) error {
	mid, err := parseMembershipID(m.ID())
	if err != nil {
		return err
	}
	err = q.UpdateMembershipStatus(ctx, UpdateMembershipStatusParams{
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
	out := make([]outboxEvent, len(evs))
	for i, e := range evs {
		out[i] = e
	}
	return writeOutboxEvents(ctx, tx, tid, out)
}

func rowToMembership(row IdentityTenantMembership) (*membership.Membership, error) {
	id := membership.ID(uuidFromPg(row.ID).String())
	personID := person.ID(uuidFromPg(row.PersonID).String())
	tenantID := tenant.ID(uuidFromPg(row.TenantID).String())

	status, err := membership.ParseStatus(row.Status)
	if err != nil {
		return nil, fmt.Errorf("membership repo: hydrate status %q: %w", row.Status, err)
	}
	return membership.UnmarshalFromDB(membership.Snapshot{
		ID:       id,
		PersonID: personID,
		TenantID: tenantID,
		Status:   status,
		JoinedAt: timeFromPg(row.JoinedAt),
		LeftAt:   timeFromPg(row.LeftAt),
	}), nil
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

// isMembershipActiveCollision reports whether err is the partial-unique
// index violation specifically (single-Active-Membership invariant).
// Other unique violations (e.g. (person_id, tenant_id) duplicate) bubble
// as raw errors — they indicate a different bug class.
func isMembershipActiveCollision(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	if pgErr.Code != "23505" {
		return false
	}
	return pgErr.ConstraintName == "uq_memberships_person_active"
}
