// Package command holds Identity CQRS command handlers.
//
// Per TDL Wild Workouts canonical layout (verified Nov 2025): each
// handler is a concrete struct with a single Handle method. Handlers
// are aggregated as fields on the Application facade in
// `internal/identity/app/app.go`; HTTP ports call
// `app.Commands.X.Handle(...)` directly.
//
// Handlers depend on domain repository INTERFACES — never adapter
// concrete types. This keeps the application layer testable without DB
// (a fake repo passes the interface; the test never touches Postgres).
package command

import (
	"context"
	"errors"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// RegisterTenantCommand carries the validated input for the
// register-tenant use case — a brand-new pharma company onboarding to
// LeadKart. Creates: Tenant, the admin Person (or attaches to existing
// Person by email), and the admin TenantMembership in one orchestrated
// flow.
//
// The handler validates that the email is not already attached to an
// active Membership in any tenant (single-Active-Membership invariant
// per multi-tenancy.md "Identity model"). The actual DB-level invariant
// is the partial unique index `WHERE status='active'`, but we surface a
// typed error before hitting the constraint for clean error messages.
type RegisterTenantCommand struct {
	Slug          slug.Slug
	LegalName     string
	DisplayName   string
	AdminEmail    email.Address
	AdminPassword string // plaintext; hashed inside the handler
	AdminFirstName string
	AdminLastName  string
}

// RegisterTenantResult is what the handler returns on success — the IDs
// the HTTP port maps into the response DTO.
type RegisterTenantResult struct {
	TenantID     tenant.ID
	PersonID     person.ID
	MembershipID membership.ID
}

// ----- Handler errors --------------------------------------------------------

// ErrEmailHasActiveMembership is returned when the AdminEmail belongs
// to a Person who already has an Active Membership in another tenant.
// The single-Active-Membership invariant blocks immediate dual-tenancy;
// the user must Deactivate elsewhere first.
var ErrEmailHasActiveMembership = errors.New(
	"register tenant: admin email already has an active membership elsewhere",
)

// ----- Handler ---------------------------------------------------------------

// RegisterTenantHandler orchestrates Tenant + Person + Membership
// creation. Per TDL canon: depends on domain interfaces only.
type RegisterTenantHandler struct {
	tenants     tenant.Repository
	persons     person.Repository
	memberships membership.Repository
}

// NewRegisterTenantHandler wires the handler against the three repos.
func NewRegisterTenantHandler(
	tenants tenant.Repository,
	persons person.Repository,
	memberships membership.Repository,
) RegisterTenantHandler {
	return RegisterTenantHandler{
		tenants:     tenants,
		persons:     persons,
		memberships: memberships,
	}
}

// Handle executes the use case. Steps:
//
//  1. Hash AdminPassword (Argon2id).
//  2. Find-or-create Person by email globally.
//     - If Person exists + has an Active Membership elsewhere → fail.
//     - If Person exists + no Active Membership → reuse the row (the
//       same human re-onboarding to a new tenant after job change).
//     - If Person doesn't exist → create.
//  3. Construct Tenant + persist (TenantRepository.Add — runs under
//     platform scope; emits TenantRegisteredEvent to outbox).
//  4. Construct Membership in StatusActive + persist (MembershipRepository.Add
//     under tenant scope of the just-created tenant — emits CreatedEvent).
//
// The orchestration intentionally uses three separate transactions
// (one per aggregate). State leaks between them are tolerable in the
// onboarding flow: the application layer would compensate on partial
// failure (TBD — Phase 6 introduces Sagas where required).
func (h RegisterTenantHandler) Handle(
	ctx context.Context,
	cmd RegisterTenantCommand,
) (RegisterTenantResult, error) {
	hash, err := argon2.Hash(cmd.AdminPassword)
	if err != nil {
		return RegisterTenantResult{}, fmt.Errorf("hash password: %w", err)
	}
	pwd, err := person.NewPasswordHash(hash)
	if err != nil {
		return RegisterTenantResult{}, fmt.Errorf("wrap hash: %w", err)
	}

	// 2. Resolve-or-create Person by email.
	//
	//    GetByEmail runs cross-tenant by design (persons table is
	//    non-RLS); ErrNotFound is the "create" branch.
	existing, err := h.persons.GetByEmail(ctx, cmd.AdminEmail)
	var p *person.Person
	switch {
	case err == nil:
		// Person exists. Block if they have an Active Membership.
		active, lerr := h.memberships.GetActiveForPerson(ctx, existing.ID())
		if lerr != nil && !errors.Is(lerr, membership.ErrNotFound) {
			return RegisterTenantResult{}, fmt.Errorf("check active membership: %w", lerr)
		}
		if active != nil {
			return RegisterTenantResult{}, ErrEmailHasActiveMembership
		}
		p = existing
	case errors.Is(err, person.ErrNotFound):
		newP, perr := person.New(
			person.ID(ids.NewV7().String()),
			cmd.AdminEmail,
			cmd.AdminFirstName,
			cmd.AdminLastName,
			pwd,
		)
		if perr != nil {
			return RegisterTenantResult{}, fmt.Errorf("construct person: %w", perr)
		}
		if perr := h.persons.Add(ctx, newP); perr != nil {
			return RegisterTenantResult{}, fmt.Errorf("persist person: %w", perr)
		}
		p = newP
	default:
		return RegisterTenantResult{}, fmt.Errorf("lookup person by email: %w", err)
	}

	// 3. Construct + persist Tenant.
	tn, err := tenant.New(
		tenant.ID(ids.NewV7().String()),
		cmd.Slug,
		cmd.LegalName,
		cmd.DisplayName,
		cmd.AdminEmail,
	)
	if err != nil {
		return RegisterTenantResult{}, fmt.Errorf("construct tenant: %w", err)
	}
	if err := h.tenants.Add(ctx, tn); err != nil {
		return RegisterTenantResult{}, fmt.Errorf("persist tenant: %w", err)
	}

	// 4. Construct + persist Membership in StatusActive.
	//
	//    MembershipRepository.Add runs under TxScopeTenant — needs
	//    tenancy.WithID on ctx to populate app.tenant_id. The
	//    just-created Tenant becomes the scope for this insert.
	m, err := membership.New(
		membership.ID(ids.NewV7().String()),
		p.ID(),
		tn.ID(),
	)
	if err != nil {
		return RegisterTenantResult{}, fmt.Errorf("construct membership: %w", err)
	}
	tenantCtx := tenancy.WithID(ctx, tenancy.ID(tn.ID().String()))
	if err := h.memberships.Add(tenantCtx, m); err != nil {
		return RegisterTenantResult{}, fmt.Errorf("persist membership: %w", err)
	}

	return RegisterTenantResult{
		TenantID:     tn.ID(),
		PersonID:     p.ID(),
		MembershipID: m.ID(),
	}, nil
}
