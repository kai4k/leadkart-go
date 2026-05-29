package adapters

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/pgconv"
	"github.com/leadkart/leadkart-go/internal/identity/adapters/db"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

// AuthRouterPG is the postgres-backed authentication-routing adapter.
//
// Single-roundtrip persons → active-membership lookup for the login
// flow, plus deferred role/override hydration for the resolver. Saves
// one network roundtrip vs. the historical
// `persons.GetByEmail + memberships.GetActiveForPerson` pair.
//
// Why a dedicated component (not a method on PersonRepository or
// MembershipRepository): the JOIN crosses two aggregates' query
// surfaces. Putting the method on either repository would force a
// domain-package import of the other (person → membership or
// vice versa), which the existing structure deliberately avoids.
// Auth-routing IS a separate concern; it gets its own adapter
// matching the consumer-defined interface in
// internal/identity/app/command/login.go.
//
// Industry alignment (2026 canon): single JOIN query indexed against
// persons.email (UNIQUE) + tenant_memberships(person_id) WHERE
// status='active' (partial UNIQUE). Postgres planner handles this
// as a 2-step index seek with no sequential scan. The materialised-
// view + denormalised-auth-table escalations (Stripe-2014 era) are
// no longer the canon — modern Postgres + JWT-based auth + cache
// layers (LeadKart's HybridCache) shifted the cost surface away
// from the auth-routing query.
//
// Reference: Brandur Leach "Postgres can do more than you think"
// (Crunchy Bridge 2024); DHH "The Majestic Monolith"; the TDL Wild
// Workouts canonical query patterns; Linear / Cal.com / Vercel
// internal architectures.
type AuthRouterPG struct {
	tx *pg.Transactor
	q  *db.Queries
}

// NewAuthRouterPG wires the adapter against a *pgxpool.Pool +
// Transactor — same constructor shape as every other identity repo
// (NewPersonRepository / NewMembershipRepository / etc.). The
// internal *db.Queries handle is built from the pool here so callers
// don't have to thread a db.Queries instance around.
func NewAuthRouterPG(pool *pgxpool.Pool, tx *pg.Transactor) *AuthRouterPG {
	return &AuthRouterPG{tx: tx, q: db.New(pool)}
}

// derefStr unwraps the *string pointers sqlc emits for LEFT JOIN
// columns whose source is NOT NULL but the JOIN's no-match path
// requires nullability in the result type. Nil → "".
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ResolveByEmail satisfies the consumer-defined
// `command.AuthRouter` interface.
//
// Returns:
//
//   - (*Person, *Membership, nil) when the Person exists AND has an
//     Active Membership. The Membership is fully hydrated (role
//     assignments + permission overrides loaded for the resolver).
//   - (*Person, nil, nil) when the Person exists but has NO Active
//     Membership. The login handler maps this to invalid_credentials
//     (uniform with unknown-email per OWASP enumeration-safety).
//   - (nil, nil, person.ErrNotFound) when no Person matches.
//
// Roundtrip count: 3 in the worst case (JOIN + role_assignments +
// permission_overrides). Down from 4 in the previous
// `GetByEmail → ListMembershipsForPerson → role_assignments →
// permission_overrides` chain. The `tenants.GetByID` call after
// resolution is a separate concern — login handler keeps making it.
//
// Runs under TxScopePlatform: persons is non-RLS but
// tenant_memberships is RLS+FORCE; the platform GUC bypass lets the
// JOIN see the Person's at-most-one Active Membership across tenants
// (the partial-unique index `uq_memberships_person_active` makes
// this single-row + correct).
func (r *AuthRouterPG) ResolveByEmail(
	ctx context.Context,
	e email.Address,
) (*person.Person, *membership.Membership, error) {
	var (
		p *person.Person
		m *membership.Membership
	)
	err := r.tx.WithinTxPgx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		q := r.q.WithTx(tx)
		row, err := q.GetPersonAndActiveMembershipByEmail(ctx, e.String())
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return person.ErrNotFound
			}
			return fmt.Errorf("auth router: get person+membership: %w", err)
		}

		// Hydrate the Person aggregate first — always present when the
		// outer query returned a row.
		personRow := db.IdentityPerson{
			ID:                          row.PersonID,
			Email:                       row.Email,
			FirstName:                   row.FirstName,
			LastName:                    row.LastName,
			PasswordHash:                row.PasswordHash,
			SecurityStamp:               row.SecurityStamp,
			IsActive:                    row.IsActive,
			IsAnonymised:                row.IsAnonymised,
			CreatedAt:                   row.PersonCreatedAt,
			AnonymisedAt:                row.AnonymisedAt,
			IsGloballySuspended:         row.IsGloballySuspended,
			GlobalSuspensionReason:      row.GlobalSuspensionReason,
			GloballySuspendedAt:         row.GloballySuspendedAt,
			PasswordResetTokenHash:      row.PasswordResetTokenHash,
			PasswordResetExpiresAt:      row.PasswordResetExpiresAt,
			PendingEmailChangeNewEmail:  row.PendingEmailChangeNewEmail,
			PendingEmailChangeTokenHash: row.PendingEmailChangeTokenHash,
			PendingEmailChangeExpiresAt: row.PendingEmailChangeExpiresAt,
			MustChangePassword:          row.MustChangePassword,
			FailedLoginCount:            row.FailedLoginCount,
			LockedUntil:                 row.LockedUntil,
			LastFailedLoginAt:           row.LastFailedLoginAt,
		}
		hp, err := rowToPerson(personRow)
		if err != nil {
			return err
		}
		p = hp

		// LEFT JOIN nullability — the membership_id pgtype.UUID's Valid
		// field is the canonical "row matched" indicator. A Person
		// without an Active Membership is a normal case (just-registered
		// pending-activation, post-deactivation reactivation flow); the
		// caller decides how to surface that.
		if !row.MembershipID.Valid {
			return nil
		}

		// LEFT JOIN flips NOT NULL columns to nullable in the result —
		// sqlc emits *string for those even though the underlying row
		// is NOT NULL. Deref via the local helper; nil columns become
		// empty strings, which the membership aggregate hydrator
		// already tolerates for these informational fields.
		membershipRow := db.IdentityTenantMembership{
			ID:                    row.MembershipID,
			PersonID:              row.PersonID,
			TenantID:              row.TenantID,
			Status:                derefStr(row.MembershipStatus),
			JoinedAt:              row.JoinedAt,
			LeftAt:                row.LeftAt,
			Designation:           derefStr(row.Designation),
			Department:            derefStr(row.Department),
			StatusMessage:         derefStr(row.StatusMessage),
			ReportsTo:             row.ReportsTo,
			CreatedByMembershipID: row.CreatedByMembershipID,
		}

		mid := pgconv.UUIDFromPg(row.MembershipID)
		roleIDs, err := loadRoleAssignments(ctx, q, mid)
		if err != nil {
			return err
		}
		granted, revoked, err := loadPermissionOverrides(ctx, q, mid)
		if err != nil {
			return err
		}

		hm, err := rowToMembership(membershipRow, roleIDs, granted, revoked)
		if err != nil {
			return err
		}
		m = hm
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return p, m, nil
}
