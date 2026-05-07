package command

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
// the caller's tenant. TenantID arrives from the verified JWT claim;
// the HTTP layer populates it before dispatch.
//
// Per Auth0 / Microsoft Entra ID canon: "create user" is ALWAYS
// find-or-create-by-email server-side. The admin enters an email; the
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
// resulting from the flow + a signal whether the global Person
// pre-existed (true = attach-to-existing path; false = brand-new
// Person minted).
type CreateUserResult struct {
	PersonID      person.ID
	MembershipID  membership.ID
	PersonExisted bool
}

// CreateUserHandler orchestrates the find-or-create-Person +
// create-Membership flow for adding a user to an existing tenant. The
// handler IS the orchestrator per TDL strict — same shape as
// [RegisterTenantHandler] but smaller (Tenant already exists; only
// Person + Membership writes).
type CreateUserHandler struct {
	tx          *pg.Transactor
	persons     *adapters.PersonRepository
	memberships *adapters.MembershipRepository
}

// NewCreateUserHandler wires the handler.
func NewCreateUserHandler(
	tx *pg.Transactor,
	persons *adapters.PersonRepository,
	memberships *adapters.MembershipRepository,
) CreateUserHandler {
	if tx == nil || persons == nil || memberships == nil {
		panic("command: NewCreateUserHandler all dependencies required")
	}
	return CreateUserHandler{tx: tx, persons: persons, memberships: memberships}
}

// Handle runs the flow:
//
//  1. Argon2id-hash the supplied password OUTSIDE the tx.
//  2. Pre-tx: lookup Person by email globally. If one is found AND
//     has an Active Membership somewhere → ErrEmailHasActiveMembership
//     (single-Active-Membership invariant per `multi-tenancy.md`).
//  3. Single-tx: find-or-create Person + create Membership in the
//     supplied tenant. Both writes' integration events drain to the
//     outbox same-tx per ADR 0008.
func (h CreateUserHandler) Handle(
	ctx context.Context,
	cmd CreateUserCommand,
) (CreateUserResult, error) {
	if cmd.TenantID.IsZero() {
		return CreateUserResult{}, errors.New("create user: tenant id required")
	}
	if cmd.Email.IsZero() {
		return CreateUserResult{}, errors.New("create user: email required")
	}

	pwd, err := h.hashPassword(cmd.Password)
	if err != nil {
		return CreateUserResult{}, err
	}

	existing, err := h.persons.GetByEmail(ctx, cmd.Email)
	switch {
	case errors.Is(err, person.ErrNotFound):
		existing = nil
	case err != nil:
		return CreateUserResult{}, fmt.Errorf("create user: lookup person: %w", err)
	}
	if existing != nil {
		active, aerr := h.memberships.GetActiveForPerson(ctx, existing.ID())
		switch {
		case errors.Is(aerr, membership.ErrNotFound):
			// fine — Person exists but is currently between jobs.
		case aerr != nil:
			return CreateUserResult{}, fmt.Errorf("create user: lookup active: %w", aerr)
		case active != nil:
			return CreateUserResult{}, ErrEmailHasActiveMembership
		}
	}

	var result CreateUserResult
	err = h.tx.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		p, perr := h.findOrCreatePersonInTx(ctx, tx, cmd, existing, pwd)
		if perr != nil {
			return perr
		}
		m, merr := h.createMembershipInTx(ctx, tx, p.ID(), cmd.TenantID)
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

func (h CreateUserHandler) hashPassword(plain string) (person.PasswordHash, error) {
	if plain == "" {
		return person.PasswordHash{}, errors.New("create user: password required")
	}
	raw, err := argon2.Hash(plain)
	if err != nil {
		return person.PasswordHash{}, fmt.Errorf("create user: hash password: %w", err)
	}
	pwd, err := person.NewPasswordHash(raw)
	if err != nil {
		return person.PasswordHash{}, fmt.Errorf("create user: wrap hash: %w", err)
	}
	return pwd, nil
}

func (h CreateUserHandler) findOrCreatePersonInTx(
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
		return nil, fmt.Errorf("create user: construct person: %w", err)
	}
	if err := h.persons.AddInTx(ctx, tx, p); err != nil {
		// Race-loss: another concurrent create-user committed first.
		// Surface as the same friendly error the pre-tx check would
		// have produced if it had won the race.
		if errors.Is(err, person.ErrEmailTaken) {
			return nil, ErrEmailHasActiveMembership
		}
		return nil, fmt.Errorf("create user: persist person: %w", err)
	}
	return p, nil
}

func (h CreateUserHandler) createMembershipInTx(
	ctx context.Context,
	tx pgx.Tx,
	personID person.ID,
	tenantID tenant.ID,
) (*membership.Membership, error) {
	m, err := membership.New(membership.ID(ids.NewV7().String()), personID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("create user: construct membership: %w", err)
	}
	if err := h.memberships.AddInTx(ctx, tx, m); err != nil {
		if errors.Is(err, membership.ErrAlreadyActive) {
			return nil, ErrEmailHasActiveMembership
		}
		return nil, fmt.Errorf("create user: persist membership: %w", err)
	}
	return m, nil
}
