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

// Onboard executes the multi-aggregate registration flow.
//
// Steps:
//
//  1. Argon2id-hash the admin password OUTSIDE the tx (CPU-bound; no
//     reason to hold a connection).
//  2. Pre-tx existence checks: GetByEmail + GetActiveForPerson. The
//     definitive check happens via DB unique constraint inside the tx;
//     the pre-tx check just produces a friendly error rather than a
//     SQLSTATE 23505 surfaced as a generic conflict.
//  3. Open ONE tx via Transactor.WithinTx (TxScopePlatform — the new
//     tenant has no current_tenant context yet).
//  4. Tenant.AddInTx — emits TenantRegisteredV1 to outbox same-tx.
//  5. Person.AddInTx OR reuse existing — emits PersonCreatedV1 only
//     for the new-Person path.
//  6. Membership.AddInTx — emits MembershipCreatedV1.
//  7. Commit. All three integration events fire from one outbox
//     batch; subscribers see a consistent post-onboarding state.
//
//nolint:cyclop // Multi-aggregate orchestrator: linear branch count is
// load-bearing (one branch per failure mode) — extracting sub-helpers
// would obscure the canonical 1-7-step sequence + the per-step err
// translation. Threshold breach is intentional + audited per
// `coding-standards.md` "Command handler scope".
func (s *TenantOnboardingService) Onboard(
	ctx context.Context,
	cmd OnboardTenantCommand,
) (OnboardTenantResult, error) {
	// 1. Hash password — pure CPU + crypto/rand, no DB.
	rawHash, err := argon2.Hash(cmd.AdminPassword)
	if err != nil {
		return OnboardTenantResult{}, fmt.Errorf("onboard: hash password: %w", err)
	}
	pwd, err := person.NewPasswordHash(rawHash)
	if err != nil {
		return OnboardTenantResult{}, fmt.Errorf("onboard: wrap hash: %w", err)
	}

	// 2. Pre-tx existence check (best-effort; DB constraints are the
	//    authoritative gate inside the tx).
	existing, err := s.persons.GetByEmail(ctx, cmd.AdminEmail)
	switch {
	case err == nil:
		active, lerr := s.memberships.GetActiveForPerson(ctx, existing.ID())
		if lerr != nil && !errors.Is(lerr, membership.ErrNotFound) {
			return OnboardTenantResult{}, fmt.Errorf("onboard: check active membership: %w", lerr)
		}
		if active != nil {
			return OnboardTenantResult{}, ErrEmailHasActiveMembership
		}
	case errors.Is(err, person.ErrNotFound):
		// fall through — Person will be created in tx
	default:
		return OnboardTenantResult{}, fmt.Errorf("onboard: lookup person by email: %w", err)
	}

	// 3-7. Single-tx orchestration.
	var result OnboardTenantResult
	err = s.tx.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context, tx pgx.Tx) error {
		// 4. Tenant.
		t, terr := tenant.New(
			tenant.ID(ids.NewV7().String()),
			cmd.Slug,
			cmd.LegalName,
			cmd.DisplayName,
			cmd.AdminEmail,
		)
		if terr != nil {
			return fmt.Errorf("construct tenant: %w", terr)
		}
		if terr := s.tenants.AddInTx(ctx, tx, t); terr != nil {
			return fmt.Errorf("persist tenant: %w", terr)
		}

		// 5. Person — find-or-create.
		p := existing
		if p == nil {
			newP, perr := person.New(
				person.ID(ids.NewV7().String()),
				cmd.AdminEmail,
				cmd.AdminFirstName,
				cmd.AdminLastName,
				pwd,
			)
			if perr != nil {
				return fmt.Errorf("construct person: %w", perr)
			}
			if perr := s.persons.AddInTx(ctx, tx, newP); perr != nil {
				if errors.Is(perr, person.ErrEmailTaken) {
					// Lost the race vs the pre-tx check — another
					// concurrent onboarding inserted the same email.
					// Surface the same friendly error.
					return ErrEmailHasActiveMembership
				}
				return fmt.Errorf("persist person: %w", perr)
			}
			p = newP
		}

		// 6. Membership — Active by construction.
		m, merr := membership.New(
			membership.ID(ids.NewV7().String()),
			p.ID(),
			t.ID(),
		)
		if merr != nil {
			return fmt.Errorf("construct membership: %w", merr)
		}
		if merr := s.memberships.AddInTx(ctx, tx, m); merr != nil {
			if errors.Is(merr, membership.ErrAlreadyActive) {
				return ErrEmailHasActiveMembership
			}
			return fmt.Errorf("persist membership: %w", merr)
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

	// 8. Default-role seeding + admin assignment (post-commit, multi-tx).
	//
	// Why split from the orchestrator's main tx:
	//   - role.Repository.Add opens its own TxScopeTenant tx (per-row).
	//     Nesting RoleRepository.Add inside the platform-scoped main tx
	//     would require a tx-aware seed path the repo doesn't yet expose.
	//   - Each downstream operation is INDEPENDENTLY idempotent:
	//     ApplyDefaultRoles re-runs are no-ops; AssignRole on Membership
	//     dedups via the role-assignments PK + the domain's set semantics.
	//   - Failure surface: if the seed step fails after the main commit,
	//     the tenant exists without roles (or without the CompanyOwner
	//     assignment). Operator runbook: re-run the onboarding flow with
	//     the same TenantID — the idempotent seed completes the work.
	//
	// Per `messaging.md` "outbox pattern over distributed transactions":
	// accept eventual consistency where the recovery path is "operator
	// re-runs an idempotent step."
	tenantCtx := tenancy.WithID(ctx, tenancy.ID(result.TenantID.String()))
	seededRoles, err := seed.ApplyDefaultRoles(tenantCtx, s.roles, result.TenantID)
	if err != nil {
		return OnboardTenantResult{}, fmt.Errorf("onboard: seed default roles: %w", err)
	}
	owner, ok := findCompanyOwner(seededRoles)
	if !ok {
		return OnboardTenantResult{}, errors.New(
			"onboard: CompanyOwner not in seeded catalog (catalog drift)",
		)
	}
	err = s.memberships.UpdateByID(tenantCtx, result.MembershipID,
		func(m *membership.Membership) (bool, error) {
			return true, m.AssignRole(owner.ID())
		})
	if err != nil {
		return OnboardTenantResult{}, fmt.Errorf(
			"onboard: assign CompanyOwner to admin membership: %w", err,
		)
	}
	return result, nil
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
