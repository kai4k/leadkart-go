// Package command holds Identity CQRS command handlers.
//
// Per TDL Wild Workouts canonical layout (verified Nov 2025): each
// handler is a concrete struct with a single Handle method. Handlers
// are aggregated as fields on the Application facade in
// `internal/identity/app/app.go`; HTTP ports call
// `app.Commands.X.Handle(...)` directly.
//
// Handler IS the orchestrator. Multi-aggregate atomic writes happen
// inside the handler's `tx.WithinTx` closure — no service abstraction
// underneath. Per TDL "Introducing Clean Architecture": *"CQRS
// replaces service interfaces with concrete handler structs."* Wild
// Workouts handlers run 30-100 lines comfortably; that IS the canon.
package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/common/tenancy"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/app/seed"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// RegisterTenantCommand carries the validated input for the
// register-tenant use case — a brand-new pharma company onboarding
// to LeadKart.
type RegisterTenantCommand struct {
	Slug           slug.Slug
	LegalName      string
	DisplayName    string
	AdminEmail     email.Address
	AdminPassword  string
	AdminFirstName string
	AdminLastName  string
}

// RegisterTenantResult carries the IDs of the three aggregates created
// during onboarding: Tenant, the admin Person, and the admin
// Membership that wires them together.
type RegisterTenantResult struct {
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
	"register tenant: admin email already has an active membership elsewhere",
)

// RegisterTenantHandler orchestrates the three-aggregate registration
// flow: hashes the admin password, finds-or-creates the global Person
// aggregate, constructs Tenant + admin Membership, persists ALL THREE
// in ONE transaction, then seeds default roles + assigns CompanyOwner.
//
// No saga. The three writes live in one Identity bounded context +
// one DB; per TDL "if you feel you need a saga between three services,
// maybe merge them" (plan §G.H.4). Same-tx atomicity is the handler's
// job per TDL strict.
//
// Boundary discipline (ADR 0047): handler depends on the domain
// repository INTERFACES + [pg.UnitOfWork] only. No pgx, no pgxpool,
// no concrete adapter struct. The UnitOfWork stashes the active
// pgx.Tx in ctx; repository .Add automatically joins the surrounding
// tx via [pg.TxFromContext] — multi-aggregate atomicity preserved
// without leaking the driver into the handler.
type RegisterTenantHandler struct {
	uow             pg.UnitOfWork
	tenants         tenant.Repository
	persons         person.Repository
	memberships     membership.Repository
	roles           role.Repository
	now             func() time.Time
	newTenantID     func() tenant.ID
	newPersonID     func() person.ID
	newMembershipID func() membership.ID
}

// NewRegisterTenantHandler wires the handler against domain
// repository interfaces + a UnitOfWork. Cheney "accept interfaces,
// return structs" — adapters implement the interfaces; this handler
// has no compile-time knowledge of pgx / sqlc.
//
// `now` is the explicit time source per the clock-injection refactor —
// composition root wires `time.Now`. Nil → time.Now.
//
// newTenantID / newPersonID / newMembershipID are the aggregate-ID
// factories per the `TestArch_HandlersInjectIDFactory` discipline.
// Production passes
// `func() <T>.ID { return <T>.ID(ids.NewV7().String()) }`; tests
// inject deterministic counters so the minted IDs are pinnable.
func NewRegisterTenantHandler(
	uow pg.UnitOfWork,
	tenants tenant.Repository,
	persons person.Repository,
	memberships membership.Repository,
	roles role.Repository,
	now func() time.Time,
	newTenantID func() tenant.ID,
	newPersonID func() person.ID,
	newMembershipID func() membership.ID,
) RegisterTenantHandler {
	if newTenantID == nil {
		panic("command: NewRegisterTenantHandler newTenantID required")
	}
	if newPersonID == nil {
		panic("command: NewRegisterTenantHandler newPersonID required")
	}
	if newMembershipID == nil {
		panic("command: NewRegisterTenantHandler newMembershipID required")
	}
	if now == nil {
		now = time.Now
	}
	return RegisterTenantHandler{
		uow:             uow,
		tenants:         tenants,
		persons:         persons,
		memberships:     memberships,
		roles:           roles,
		now:             now,
		newTenantID:     newTenantID,
		newPersonID:     newPersonID,
		newMembershipID: newMembershipID,
	}
}

// Handle executes the multi-aggregate registration flow as a thin
// narrative — each numbered step delegates to a private method that
// owns the per-step concern + error translation.
//
// Steps:
//
//  1. Argon2id-hash the admin password OUTSIDE the tx (CPU-bound;
//     no reason to hold a connection).
//  2. Pre-tx existence check: same-email Person already has an Active
//     Membership? Surfaces ErrEmailHasActiveMembership early so the
//     caller sees a friendly error, not a SQLSTATE 23505 conflict.
//     The DB partial unique index is the authoritative gate inside
//     the tx.
//  3. Single-tx persist of Tenant + Person (find-or-create) +
//     Membership. All three integration events fire from one outbox
//     batch — subscribers see a consistent post-onboarding state.
//  4. Post-commit: idempotent default-role seeding + CompanyOwner
//     assignment. Split from main tx because role.Repository.Add
//     opens its own TxScopeTenant tx; each step is independently
//     idempotent so partial failure is operator-recoverable per
//     `messaging.md` "outbox pattern over distributed transactions".
func (h RegisterTenantHandler) Handle(
	ctx context.Context,
	cmd RegisterTenantCommand,
) (RegisterTenantResult, error) {
	pwd, err := h.hashAdminPassword(cmd.AdminPassword)
	if err != nil {
		return RegisterTenantResult{}, err
	}

	// Pre-tx existence check: lookup Person by email, then guard the
	// single-Active-Membership invariant. nil-existing = create-fresh
	// path inside the tx.
	existing, err := h.persons.GetByEmail(ctx, cmd.AdminEmail)
	switch {
	case errors.Is(err, person.ErrNotFound):
		existing = nil
	case err != nil:
		return RegisterTenantResult{}, fmt.Errorf("register tenant: lookup person: %w", err)
	}
	if existing != nil {
		if err := h.assertNoActiveMembership(ctx, existing.ID()); err != nil {
			return RegisterTenantResult{}, err
		}
	}

	now := h.now()
	result, err := h.persistAggregatesInTx(ctx, cmd, existing, pwd, now)
	if err != nil {
		return RegisterTenantResult{}, err
	}
	if err := h.seedRolesAndAssignOwner(ctx, result, now); err != nil {
		return RegisterTenantResult{}, err
	}
	return result, nil
}

// hashAdminPassword runs Argon2id outside the DB tx — pure CPU +
// crypto/rand, no reason to hold a connection. Wraps the raw PHC
// string in the [person.PasswordHash] VO so domain validators have
// already passed by the time we hit the persist step.
func (h RegisterTenantHandler) hashAdminPassword(plain string) (person.PasswordHash, error) {
	rawHash, err := argon2.Hash(plain)
	if err != nil {
		return person.PasswordHash{}, fmt.Errorf("register tenant: hash password: %w", err)
	}
	pwd, err := person.NewPasswordHash(rawHash)
	if err != nil {
		return person.PasswordHash{}, fmt.Errorf("register tenant: wrap hash: %w", err)
	}
	return pwd, nil
}

// assertNoActiveMembership guards the single-Active-Membership
// invariant per `multi-tenancy.md` "Identity model": a Person can hold
// AT MOST ONE Active Membership across all tenants. The DB partial-
// unique-index is the authoritative gate inside the onboarding tx;
// this pre-tx check just produces a friendly typed error rather than
// a SQLSTATE 23505 surfaced as a generic conflict.
func (h RegisterTenantHandler) assertNoActiveMembership(
	ctx context.Context,
	personID person.ID,
) error {
	active, err := h.memberships.GetActiveForPerson(ctx, personID)
	switch {
	case errors.Is(err, membership.ErrNotFound):
		return nil
	case err != nil:
		return fmt.Errorf("register tenant: check active membership: %w", err)
	}
	if active != nil {
		return ErrEmailHasActiveMembership
	}
	return nil
}

// persistAggregatesInTx is the load-bearing single-tx step: Tenant +
// Person (find-or-create) + Membership inserted atomically under
// TxScopePlatform (the new tenant has no current_tenant context yet).
// All three aggregates' integration events drain to the outbox same-tx
// per ADR 0008.
func (h RegisterTenantHandler) persistAggregatesInTx(
	ctx context.Context,
	cmd RegisterTenantCommand,
	existing *person.Person,
	pwd person.PasswordHash,
	now time.Time,
) (RegisterTenantResult, error) {
	var result RegisterTenantResult
	err := h.uow.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context) error {
		t, terr := tenant.New(
			h.newTenantID(),
			cmd.Slug, cmd.LegalName, cmd.DisplayName, cmd.AdminEmail,
			now,
		)
		if terr != nil {
			return fmt.Errorf("construct tenant: %w", terr)
		}
		// ctx carries the active tx — repository .Add joins it via
		// pg.TxFromContext. Boundary stays clean of pgx.
		if terr := h.tenants.Add(ctx, t); terr != nil {
			return fmt.Errorf("persist tenant: %w", terr)
		}
		p, perr := h.findOrCreatePerson(ctx, cmd, existing, pwd, now)
		if perr != nil {
			return perr
		}
		m, merr := h.createMembership(ctx, p.ID(), t.ID(), now)
		if merr != nil {
			return merr
		}
		result = RegisterTenantResult{
			TenantID:     t.ID(),
			PersonID:     p.ID(),
			MembershipID: m.ID(),
		}
		return nil
	})
	if err != nil {
		return RegisterTenantResult{}, err
	}
	return result, nil
}

// findOrCreatePerson returns the existing aggregate when the pre-tx
// check found one, otherwise constructs + persists a fresh Person on
// the surrounding UnitOfWork tx. Translates the race-loss SQLSTATE-
// 23505 path on duplicate email into ErrEmailHasActiveMembership —
// concurrent onboarding races resolve to the same observable error
// shape.
func (h RegisterTenantHandler) findOrCreatePerson(
	ctx context.Context,
	cmd RegisterTenantCommand,
	existing *person.Person,
	pwd person.PasswordHash,
	now time.Time,
) (*person.Person, error) {
	if existing != nil {
		return existing, nil
	}
	// BRD line 241 + ADR 0053 — admin/operator-provisioned credential.
	// The admin chose this initial password during tenant onboarding;
	// force the new admin Person through the change-password flow on
	// first login. Cleared on self-change / self-reset paths.
	p, err := person.NewWithMustChangePassword(
		h.newPersonID(),
		cmd.AdminEmail, cmd.AdminFirstName, cmd.AdminLastName, pwd,
		now,
	)
	if err != nil {
		return nil, fmt.Errorf("construct person: %w", err)
	}
	if err := h.persons.Add(ctx, p); err != nil {
		if errors.Is(err, person.ErrEmailTaken) {
			return nil, ErrEmailHasActiveMembership
		}
		return nil, fmt.Errorf("persist person: %w", err)
	}
	return p, nil
}

// createMembership constructs the admin Membership (Active by
// construction per the [membership.New] factory) + persists it on the
// surrounding UnitOfWork tx. Translates partial-unique-index violation
// into ErrEmailHasActiveMembership for the same race-tolerance reason.
func (h RegisterTenantHandler) createMembership(
	ctx context.Context,
	personID person.ID,
	tenantID tenant.ID,
	now time.Time,
) (*membership.Membership, error) {
	// createdBy = zero — RegisterTenant's first admin is self-bootstrapped
	// (no pre-existing Membership invited them). Distinguishes
	// onboarding-time admin from later-invited users in audit queries.
	m, err := membership.New(h.newMembershipID(), personID, tenantID, membership.ID(""), now)
	if err != nil {
		return nil, fmt.Errorf("construct membership: %w", err)
	}
	if err := h.memberships.Add(ctx, m); err != nil {
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
// in place — caught at unit-test time by
// `seed.TestDefaultRoleCatalog_CompanyOwnerCarriesTenantAdmin`.
func (h RegisterTenantHandler) seedRolesAndAssignOwner(
	ctx context.Context,
	result RegisterTenantResult,
	now time.Time,
) error {
	tenantCtx := tenancy.WithID(ctx, tenancy.ID(result.TenantID.String()))
	seededRoles, err := seed.ApplyDefaultRoles(tenantCtx, h.roles, result.TenantID, now)
	if err != nil {
		return fmt.Errorf("register tenant: seed default roles: %w", err)
	}
	owner, ok := findCompanyOwner(seededRoles)
	if !ok {
		return errors.New("register tenant: CompanyOwner not in seeded catalog (catalog drift)")
	}
	err = h.memberships.UpdateByID(tenantCtx, result.TenantID, result.MembershipID,
		func(m *membership.Membership) (bool, error) {
			return true, m.AssignRole(owner.ID(), now)
		})
	if err != nil {
		return fmt.Errorf("register tenant: assign CompanyOwner to admin membership: %w", err)
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
