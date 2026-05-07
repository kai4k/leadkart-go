package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/platform/pg"
)

// CreateUserCommand carries the validated input for adding a user to
// an existing tenant. TenantID arrives from the verified JWT claim;
// the HTTP layer populates it before dispatch.
//
// The Auth0/Microsoft Entra ID canon: "create user" is ALWAYS find-
// or-create-by-email server-side. The admin enters an email; the
// server decides whether to attach to an existing global Person or
// mint a new one. No UI choice required.
type CreateUserCommand struct {
	TenantID  tenant.ID
	Email     email.Address
	Password  string
	FirstName string
	LastName  string
}

// CreateUserResult carries the IDs of the (Person, Membership) pair
// resulting from the flow. Callers use these for the HTTP response
// + downstream audit/event correlation.
type CreateUserResult struct {
	PersonID     person.ID
	MembershipID membership.ID
	PersonExisted bool // true when an existing global Person was attached to a new Membership
}

// UserOnboardingService orchestrates the find-or-create-Person +
// create-Membership flow for adding a user to an existing tenant.
// Mirrors the [TenantOnboardingService] pattern but smaller — Tenant
// already exists, we only persist Person (maybe) + Membership.
type UserOnboardingService struct {
	tx          *pg.Transactor
	persons     *adapters.PersonRepository
	memberships *adapters.MembershipRepository
}

// NewUserOnboardingService wires the orchestrator.
func NewUserOnboardingService(
	tx *pg.Transactor,
	persons *adapters.PersonRepository,
	memberships *adapters.MembershipRepository,
) *UserOnboardingService {
	if tx == nil || persons == nil || memberships == nil {
		panic("service: NewUserOnboardingService all dependencies required")
	}
	return &UserOnboardingService{tx: tx, persons: persons, memberships: memberships}
}

// CreateUser runs the flow:
//
//  1. Argon2id-hash the supplied password OUTSIDE the tx.
//  2. Pre-tx: lookup Person by email globally. If one is found AND
//     has an Active Membership somewhere → ErrEmailHasActiveMembership
//     (single-Active-Membership invariant per multi-tenancy.md).
//  3. Single-tx: find-or-create Person + create Membership in the
//     supplied tenant. Both writes' integration events drain to the
//     outbox same-tx per ADR 0008.
func (s *UserOnboardingService) CreateUser(
	ctx context.Context,
	cmd CreateUserCommand,
) (CreateUserResult, error) {
	if cmd.TenantID.IsZero() {
		return CreateUserResult{}, errors.New("create_user: tenant id required")
	}
	if cmd.Email.IsZero() {
		return CreateUserResult{}, errors.New("create_user: email required")
	}

	pwd, err := s.hashPassword(cmd.Password)
	if err != nil {
		return CreateUserResult{}, err
	}

	existing, err := s.persons.GetByEmail(ctx, cmd.Email)
	switch {
	case errors.Is(err, person.ErrNotFound):
		existing = nil
	case err != nil:
		return CreateUserResult{}, fmt.Errorf("create_user: lookup person: %w", err)
	}
	if existing != nil {
		if active, aerr := s.memberships.GetActiveForPerson(ctx, existing.ID()); aerr == nil && active != nil {
			return CreateUserResult{}, ErrEmailHasActiveMembership
		} else if aerr != nil && !errors.Is(aerr, membership.ErrNotFound) {
			return CreateUserResult{}, fmt.Errorf("create_user: lookup active: %w", aerr)
		}
	}

	var result CreateUserResult
	err = s.tx.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		p, perr := s.findOrCreatePersonInTx(ctx, tx, cmd, existing, pwd)
		if perr != nil {
			return perr
		}
		m, merr := s.createMembershipInTx(ctx, tx, p.ID(), cmd.TenantID)
		if merr != nil {
			return merr
		}
		result = CreateUserResult{
			PersonID:      p.ID(),
			MembershipID:  m.ID(),
			PersonExisted: existing != nil,
		}
		return nil
	})
	if err != nil {
		return CreateUserResult{}, err
	}
	return result, nil
}

func (s *UserOnboardingService) hashPassword(plain string) (person.PasswordHash, error) {
	if plain == "" {
		return person.PasswordHash{}, errors.New("create_user: password required")
	}
	raw, err := argon2.Hash(plain)
	if err != nil {
		return person.PasswordHash{}, fmt.Errorf("create_user: hash password: %w", err)
	}
	pwd, err := person.NewPasswordHash(raw)
	if err != nil {
		return person.PasswordHash{}, fmt.Errorf("create_user: wrap hash: %w", err)
	}
	return pwd, nil
}

func (s *UserOnboardingService) findOrCreatePersonInTx(
	ctx context.Context,
	tx pgx.Tx,
	cmd CreateUserCommand,
	existing *person.Person,
	pwd person.PasswordHash,
) (*person.Person, error) {
	if existing != nil {
		return existing, nil
	}
	p, err := person.New(person.ID(ids.NewV7().String()),
		cmd.Email, cmd.FirstName, cmd.LastName, pwd)
	if err != nil {
		return nil, fmt.Errorf("create_user: construct person: %w", err)
	}
	if err := s.persons.AddInTx(ctx, tx, p); err != nil {
		// Race-loss: another concurrent create-user committed first.
		// Surface as the same friendly error the pre-tx check would
		// have produced if it had won the race.
		if errors.Is(err, person.ErrEmailTaken) {
			return nil, ErrEmailHasActiveMembership
		}
		return nil, fmt.Errorf("create_user: persist person: %w", err)
	}
	return p, nil
}

func (s *UserOnboardingService) createMembershipInTx(
	ctx context.Context,
	tx pgx.Tx,
	personID person.ID,
	tenantID tenant.ID,
) (*membership.Membership, error) {
	m, err := membership.New(membership.ID(ids.NewV7().String()), personID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("create_user: construct membership: %w", err)
	}
	if err := s.memberships.AddInTx(ctx, tx, m); err != nil {
		if errors.Is(err, membership.ErrAlreadyActive) {
			return nil, ErrEmailHasActiveMembership
		}
		return nil, fmt.Errorf("create_user: persist membership: %w", err)
	}
	return m, nil
}
