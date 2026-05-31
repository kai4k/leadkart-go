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

// AuthRouterPG is the Postgres-backed authentication-routing adapter.
// Satisfies the consumer-defined interface in
// internal/identity/app/command/login.go.
//
// A dedicated adapter (not a method on PersonRepository or
// MembershipRepository) because the JOIN crosses both aggregates'
// query surfaces — putting it on either repo would require a
// cross-domain import. Single indexed JOIN (persons.email UNIQUE +
// tenant_memberships partial UNIQUE) is the canonical shape; the
// Stripe-2014 materialised-view pattern is no longer needed.
type AuthRouterPG struct {
	tx *pg.Transactor
	q  *db.Queries
}

// NewAuthRouterPG wires the adapter against a pool and transactor.
func NewAuthRouterPG(pool *pgxpool.Pool, tx *pg.Transactor) *AuthRouterPG {
	return &AuthRouterPG{tx: tx, q: db.New(pool)}
}

// derefStr dereferences *string pointers sqlc emits for LEFT JOIN nullable
// columns. Returns "" for nil.
func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ResolveByEmail satisfies the consumer-defined `command.AuthRouter` interface.
//
// Returns:
//   - (*Person, *Membership, nil) — Person found with Active Membership, fully hydrated.
//   - (*Person, nil, nil) — Person found, no Active Membership; login handler treats as invalid_credentials (OWASP enumeration safety).
//   - (nil, nil, person.ErrNotFound) — no Person for this email.
//
// Runs under TxScopePlatform so the JOIN can see tenant_memberships
// (RLS+FORCE) across tenants via the partial-unique index
// uq_memberships_person_active.
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

		// Hydrate Person — always present when the outer query returned a row.
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

		// MembershipID.Valid is the canonical "JOIN matched" indicator; nil
		// is a normal case (pending activation, post-deactivation).
		if !row.MembershipID.Valid {
			return nil
		}

		// sqlc emits *string for LEFT JOIN columns even when the underlying
		// column is NOT NULL. Deref to ""; the membership hydrator tolerates it.
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
