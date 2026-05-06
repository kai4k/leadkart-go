// Package service holds Identity application services — multi-aggregate
// orchestrators per `architecture.md` "Command handler scope — no
// orchestration": handlers stay thin (≤40 LOC, ≤6 ctor deps);
// orchestration that crosses aggregate boundaries lives here.
//
// Per the same doctrine: services are NOT cross-cutting infrastructure
// (that's `internal/platform/`). They are use-case orchestrators that
// stitch domain factories + repositories into a single transactional
// flow.
package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/app/seed"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/platform/pg"
)

// OnboardTenantCommand carries the validated input for the
// register-tenant use case. Handlers translate HTTP DTOs into this
// struct after constructing the domain VOs (slug, email).
type OnboardTenantCommand struct {
	Slug           slug.Slug
	LegalName      string
	DisplayName    string
	AdminEmail     email.Address
	AdminPassword  string
	AdminFirstName string
	AdminLastName  string
}

// OnboardTenantResult carries the IDs of the three aggregates created
// during onboarding. Handlers map this into the response DTO.
type OnboardTenantResult struct {
	TenantID     tenant.ID
	PersonID     person.ID
	MembershipID membership.ID
}

// ErrEmailHasActiveMembership — admin email already belongs to a
// Person who has an Active Membership in some tenant. The single-
// Active-Membership invariant per `multi-tenancy.md` blocks the
// onboard; the existing tenant must deactivate the Membership first
// (e.g. job change), then this Person can re-onboard to a new tenant.
var ErrEmailHasActiveMembership = errors.New(
	"onboard tenant: admin email already has an active membership elsewhere",
)

// TenantOnboardingService orchestrates tenant registration: hashes the
// admin password, finds-or-creates the global Person aggregate,
// constructs Tenant + Membership, and persists ALL THREE in ONE
// transaction. Per TDL TransactionProvider escape hatch
// (`messaging.md` G.H.1) — the service composes repo.AddInTx calls
// inside Transactor.WithinTx so cross-aggregate consistency is real.
//
// No saga. Not needed: the three writes are within one Identity
// bounded context + one DB. Per TDL doctrine "if you feel you need a
// saga between three services, maybe merge them" (plan §G.H.4).
type TenantOnboardingService struct {
	tx          *pg.Transactor
	tenants     *adapters.TenantRepository
	persons     *adapters.PersonRepository
	memberships *adapters.MembershipRepository
	roles       *adapters.RoleRepository
}

// NewTenantOnboardingService wires the orchestrator. All five
// dependencies are concrete — handlers depend on the orchestrator
// concrete type, not an interface, per Go-canon "accept interfaces
// at the consumer" + the orchestrator IS the consumer here.
func NewTenantOnboardingService(
	tx *pg.Transactor,
	tenants *adapters.TenantRepository,
	persons *adapters.PersonRepository,
	memberships *adapters.MembershipRepository,
	roles *adapters.RoleRepository,
) *TenantOnboardingService {
	return &TenantOnboardingService{
		tx:          tx,
		tenants:     tenants,
		persons:     persons,
		memberships: memberships,
		roles:       roles,
	}
}

// Onboard executes the multi-aggregate registration flow as a thin
// narrative — each numbered step delegates to a private helper that
// owns the per-step concern + error translation. Reads as a story per
// Vernon *IDDD* ch.7 (Domain Services) + TDL "Database Transactions
// in Go with Layered Architecture" Sep 2024.
//
// Steps:
//
//  1. Argon2id-hash the admin password OUTSIDE the tx (CPU-bound;
//     no reason to hold a connection).
//  2. Pre-tx existence check: same-email Person already has an Active
//     Membership? Surfaces ErrEmailHasActiveMembership early so the
//     caller sees a friendly error, not a SQLSTATE 23505 conflict.
//     The DB partial unique index is the authoritative gate inside the tx.
//  3. Single-tx persist of Tenant + Person (find-or-create) +
//     Membership. All three integration events fire from one outbox
//     batch — subscribers see a consistent post-onboarding state.
//  4. Post-commit: idempotent default-role seeding + CompanyOwner
//     assignment. Split from main tx because role.Repository.Add
//     opens its own TxScopeTenant tx; each step is independently
//     idempotent so partial failure is operator-recoverable per
//     messaging.md "outbox pattern over distributed transactions".
func (s *TenantOnboardingService) Onboard(
	ctx context.Context,
	cmd OnboardTenantCommand,
) (OnboardTenantResult, error) {
	pwd, err := s.hashAdminPassword(cmd.AdminPassword)
	if err != nil {
		return OnboardTenantResult{}, err
	}
	existing, err := s.resolveExistingPerson(ctx, cmd.AdminEmail)
	if err != nil {
		return OnboardTenantResult{}, err
	}
	result, err := s.persistAggregatesInTx(ctx, cmd, existing, pwd)
	if err != nil {
		return OnboardTenantResult{}, err
	}
	if err := s.seedRolesAndAssignOwner(ctx, result); err != nil {
		return OnboardTenantResult{}, err
	}
	return result, nil
}

// hashAdminPassword runs Argon2id outside the DB tx — pure CPU +
// crypto/rand, no reason to hold a connection. Wraps the raw PHC
// string in the [person.PasswordHash] VO so domain validators have
// already passed by the time we hit the persist step.
func (s *TenantOnboardingService) hashAdminPassword(
	plain string,
) (person.PasswordHash, error) {
	rawHash, err := argon2.Hash(plain)
	if err != nil {
		return person.PasswordHash{}, fmt.Errorf("onboard: hash password: %w", err)
	}
	pwd, err := person.NewPasswordHash(rawHash)
	if err != nil {
		return person.PasswordHash{}, fmt.Errorf("onboard: wrap hash: %w", err)
	}
	return pwd, nil
}

// resolveExistingPerson is the pre-tx existence check. Returns:
//
//   - (existing, nil)  — Person exists, has NO Active Membership;
//                        caller will reuse the aggregate in tx.
//   - (nil, nil)       — no Person under this email; caller creates fresh.
//   - (nil, ErrEmailHasActiveMembership) — single-Active-Membership
//                        invariant blocks onboarding.
//   - (nil, wrapped err) — repository failure; bubble.
//
// The DB partial-unique-index is the authoritative gate inside the
// tx; this just produces a friendly typed error rather than a
// SQLSTATE 23505 surfaced as a generic conflict.
func (s *TenantOnboardingService) resolveExistingPerson(
	ctx context.Context,
	addr email.Address,
) (*person.Person, error) {
	existing, err := s.persons.GetByEmail(ctx, addr)
	switch {
	case errors.Is(err, person.ErrNotFound):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("onboard: lookup person by email: %w", err)
	}
	active, lerr := s.memberships.GetActiveForPerson(ctx, existing.ID())
	if lerr != nil && !errors.Is(lerr, membership.ErrNotFound) {
		return nil, fmt.Errorf("onboard: check active membership: %w", lerr)
	}
	if active != nil {
		return nil, ErrEmailHasActiveMembership
	}
	return existing, nil
}

// persistAggregatesInTx is the load-bearing single-tx step: Tenant
// + Person (find-or-create) + Membership inserted atomically under
// TxScopePlatform (the new tenant has no current_tenant context yet).
// All three aggregates' integration events drain to the outbox same-tx
// per ADR 0008.
func (s *TenantOnboardingService) persistAggregatesInTx(
	ctx context.Context,
	cmd OnboardTenantCommand,
	existing *person.Person,
	pwd person.PasswordHash,
) (OnboardTenantResult, error) {
	var result OnboardTenantResult
	err := s.tx.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		t, terr := tenant.New(
			tenant.ID(ids.NewV7().String()),
			cmd.Slug, cmd.LegalName, cmd.DisplayName, cmd.AdminEmail,
		)
		if terr != nil {
			return fmt.Errorf("construct tenant: %w", terr)
		}
		if terr := s.tenants.AddInTx(ctx, tx, t); terr != nil {
			return fmt.Errorf("persist tenant: %w", terr)
		}
		p, perr := s.findOrCreatePersonInTx(ctx, tx, cmd, existing, pwd)
		if perr != nil {
			return perr
		}
		m, merr := s.createMembershipInTx(ctx, tx, p.ID(), t.ID())
		if merr != nil {
			return merr
		}
		result = OnboardTenantResult{
			TenantID:     t.ID(),
			PersonID:     p.ID(),
			MembershipID: m.ID(),
		}
		return nil
	})
	if err != nil {
		return OnboardTenantResult{}, err
	}
	return result, nil
}

// findOrCreatePersonInTx returns the existing aggregate when the
// pre-tx check found one, otherwise constructs + persists a fresh
// Person. Translates the race-loss SQLSTATE-23505 path on duplicate
// email into the same friendly ErrEmailHasActiveMembership the pre-tx
// check would have surfaced — concurrent onboarding races resolve to
// the same observable error shape.
func (s *TenantOnboardingService) findOrCreatePersonInTx(
	ctx context.Context,
	tx pgx.Tx,
	cmd OnboardTenantCommand,
	existing *person.Person,
	pwd person.PasswordHash,
) (*person.Person, error) {
	if existing != nil {
		return existing, nil
	}
	p, err := person.New(
		person.ID(ids.NewV7().String()),
		cmd.AdminEmail, cmd.AdminFirstName, cmd.AdminLastName, pwd,
	)
	if err != nil {
		return nil, fmt.Errorf("construct person: %w", err)
	}
	if err := s.persons.AddInTx(ctx, tx, p); err != nil {
		if errors.Is(err, person.ErrEmailTaken) {
			return nil, ErrEmailHasActiveMembership
		}
		return nil, fmt.Errorf("persist person: %w", err)
	}
	return p, nil
}

// createMembershipInTx constructs the admin Membership (Active by
// construction per the [membership.New] factory) + persists it.
// Translates the partial-unique-index violation into
// ErrEmailHasActiveMembership for the same reason as
// findOrCreatePersonInTx — concurrent races resolve to one error shape.
func (s *TenantOnboardingService) createMembershipInTx(
	ctx context.Context,
	tx pgx.Tx,
	personID person.ID,
	tenantID tenant.ID,
) (*membership.Membership, error) {
	m, err := membership.New(membership.ID(ids.NewV7().String()), personID, tenantID)
	if err != nil {
		return nil, fmt.Errorf("construct membership: %w", err)
	}
	if err := s.memberships.AddInTx(ctx, tx, m); err != nil {
		if errors.Is(err, membership.ErrAlreadyActive) {
			return nil, ErrEmailHasActiveMembership
		}
		return nil, fmt.Errorf("persist membership: %w", err)
	}
	return m, nil
}

// seedRolesAndAssignOwner is the post-commit step: idempotent default-
// role seeding (each role.Repository.Add opens its own TxScopeTenant)
// + assignment of CompanyOwner to the admin Membership. Per
// `messaging.md` "outbox pattern over distributed transactions" —
// partial failure is operator-recoverable: re-run onboarding with the
// same TenantID, idempotent seed completes the work.
//
// Catalog drift surfaces as a 500-class error (CompanyOwner missing
// from the seeded list); operationally this means
// [seed.DefaultRoleCatalog] was edited without leaving CompanyOwner
// in place — should be caught at unit-test time via
// `seed.TestDefaultRoleCatalog_CompanyOwnerCarriesTenantAdmin`.
func (s *TenantOnboardingService) seedRolesAndAssignOwner(
	ctx context.Context,
	result OnboardTenantResult,
) error {
	tenantCtx := tenancy.WithID(ctx, tenancy.ID(result.TenantID.String()))
	seededRoles, err := seed.ApplyDefaultRoles(tenantCtx, s.roles, result.TenantID)
	if err != nil {
		return fmt.Errorf("onboard: seed default roles: %w", err)
	}
	owner, ok := findCompanyOwner(seededRoles)
	if !ok {
		return errors.New("onboard: CompanyOwner not in seeded catalog (catalog drift)")
	}
	err = s.memberships.UpdateByID(tenantCtx, result.MembershipID,
		func(m *membership.Membership) (bool, error) {
			return true, m.AssignRole(owner.ID())
		})
	if err != nil {
		return fmt.Errorf("onboard: assign CompanyOwner to admin membership: %w", err)
	}
	return nil
}

// findCompanyOwner picks the CompanyOwner role from a seeded catalog.
// Catalog ordering is wire-stable but we look up by name rather than
// index so a future spec reorder doesn't silently misassign authority.
func findCompanyOwner(roles []*role.Role) (*role.Role, bool) {
	for _, r := range roles {
		if r.Name() == role.SystemRoles.Tenant.CompanyOwner {
			return r, true
		}
	}
	return nil, false
}
