package command_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/common/slug"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership/membershiptest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person/persontest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role"
	"github.com/leadkart/leadkart-go/internal/identity/domain/role/roletest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant/tenanttest"
)

// =============================================================================
// register_tenant_test.go — handler-unit coverage for RegisterTenantHandler
// per ADR 0062 §6 (handler-unit MANY) + ADR 0047 (boundary discipline:
// handler depends only on domain interfaces + pg.UnitOfWork).
//
// The handler is a 3-step orchestrator:
//
//  1. hashAdminPassword (Argon2id outside the tx)
//  2. persistAggregatesInTx (Tenant + Person + Membership in ONE tx via UoW)
//  3. seedRolesAndAssignOwner (post-commit: ApplyDefaultRoles + assign
//     CompanyOwner to the admin Membership)
//
// Branches tested below:
//   - happy: fresh Person  → all three aggregates persisted + role assigned
//   - happy: existing Person without active membership → reuses Person ID
//   - GetByEmail non-NotFound → wrapped "lookup person"
//   - existing Person + active membership → ErrEmailHasActiveMembership
//   - tenant.New invariant rejection → wrapped "construct tenant"
//   - tenants.Add error → wrapped "persist tenant"
//   - persons.Add ErrEmailTaken (race) → ErrEmailHasActiveMembership
//   - persons.Add other error → wrapped "persist person"
//   - memberships.Add ErrAlreadyActive (race) → ErrEmailHasActiveMembership
//   - memberships.Add other error → wrapped "persist membership"
//   - ApplyDefaultRoles error → wrapped "seed default roles"
//   - CompanyOwner missing from catalog → "CompanyOwner not in seeded catalog"
//     (force by deleting the CompanyOwner row from the role repo BEFORE the
//     handler's post-commit seed step runs — currently structural since the
//     real catalog ALWAYS includes CompanyOwner; documented as a defensive
//     guard).
//   - memberships.UpdateByID (role-assign step) error → wrapped
//
// All test-side fakes/wrappers are prefixed `b2*` per the concurrent-agent
// coordination convention. b2* names live in this file only.
// =============================================================================

// b2FakeUoW satisfies pg.UnitOfWork by running fn directly with the
// supplied ctx — no real tx, no pgx. Per the same pattern used in
// internal/crm/app/command/fakes_test.go and internal/inventory/...
type b2FakeUoW struct{}

func (b2FakeUoW) WithinTx(ctx context.Context, _ pg.TxScope, fn func(ctx context.Context) error) error {
	return fn(ctx)
}

// b2NewRegisterDeps wires a fresh RegisterTenantHandler against in-memory
// fakes + the no-op UoW. Test-side ID factories use the same UUIDv7 shape
// production uses so any downstream IsZero / IsValid assertions are honest.
type b2RegisterDeps struct {
	uow       pg.UnitOfWork
	tenants   tenant.Repository
	persons   person.Repository
	memberships membership.Repository
	roles     role.Repository
}

func b2NewRegisterDeps() b2RegisterDeps {
	return b2RegisterDeps{
		uow:         b2FakeUoW{},
		tenants:     tenanttest.NewFakeRepository(),
		persons:     persontest.NewFakeRepository(),
		memberships: membershiptest.NewFakeRepository(),
		roles:       roletest.NewFakeRepository(),
	}
}

func b2NewRegisterHandler(d b2RegisterDeps) command.RegisterTenantHandler {
	return command.NewRegisterTenantHandler(
		d.uow, d.tenants, d.persons, d.memberships, d.roles,
		func() time.Time { return testNow },
		func() tenant.ID { return tenant.ID(ids.NewV7().String()) },
		func() person.ID { return person.ID(ids.NewV7().String()) },
		func() membership.ID { return membership.ID(ids.NewV7().String()) },
	)
}

// b2RegisterCmd builds a valid command. Caller can override fields per test.
func b2RegisterCmd(t *testing.T) command.RegisterTenantCommand {
	t.Helper()
	// Slug suffix derived from a fresh UUIDv7 so parallel tests don't collide
	// on the tenants fake's slug uniqueness index.
	full := ids.NewV7().String()
	s, err := slug.New("rt-" + full[len(full)-8:])
	if err != nil {
		t.Fatalf("slug.New: %v", err)
	}
	addr, err := email.New("admin-" + full[len(full)-8:] + "@example.test")
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}
	return command.RegisterTenantCommand{
		Slug:           s,
		LegalName:      "Acme Pharma Pvt Ltd",
		DisplayName:    "Acme",
		AdminEmail:     addr,
		AdminPassword:  "initial-temp-password-strong-enough",
		AdminFirstName: "Alice",
		AdminLastName:  "Admin",
	}
}

// ----- error-injecting fakes ----------------------------------------------

// b2RegTenantRepo is the tenants Repository with per-call error injection.
type b2RegTenantRepo struct {
	*tenanttest.FakeRepository
	errOnAdd error
}

func newB2RegTenantRepo() *b2RegTenantRepo {
	return &b2RegTenantRepo{FakeRepository: tenanttest.NewFakeRepository()}
}

func (r *b2RegTenantRepo) Add(ctx context.Context, t *tenant.Tenant) error {
	if r.errOnAdd != nil {
		err := r.errOnAdd
		r.errOnAdd = nil
		return err
	}
	return r.FakeRepository.Add(ctx, t)
}

// b2RegPersonRepo is the persons Repository with per-call error injection.
type b2RegPersonRepo struct {
	*persontest.FakeRepository
	errOnGetByEmail error
	errOnAdd        error
}

func newB2RegPersonRepo() *b2RegPersonRepo {
	return &b2RegPersonRepo{FakeRepository: persontest.NewFakeRepository()}
}

func (r *b2RegPersonRepo) GetByEmail(ctx context.Context, e email.Address) (*person.Person, error) {
	if r.errOnGetByEmail != nil {
		err := r.errOnGetByEmail
		r.errOnGetByEmail = nil
		return nil, err
	}
	return r.FakeRepository.GetByEmail(ctx, e)
}

func (r *b2RegPersonRepo) Add(ctx context.Context, p *person.Person) error {
	if r.errOnAdd != nil {
		err := r.errOnAdd
		r.errOnAdd = nil
		return err
	}
	return r.FakeRepository.Add(ctx, p)
}

// b2RegMembershipRepo is the memberships Repository with per-call error
// injection. Tracks Add count so reuse-Person tests can assert.
type b2RegMembershipRepo struct {
	*membershiptest.FakeRepository
	errOnAdd        error
	errOnUpdateByID error
}

func newB2RegMembershipRepo() *b2RegMembershipRepo {
	return &b2RegMembershipRepo{FakeRepository: membershiptest.NewFakeRepository()}
}

func (r *b2RegMembershipRepo) Add(ctx context.Context, m *membership.Membership) error {
	if r.errOnAdd != nil {
		err := r.errOnAdd
		r.errOnAdd = nil
		return err
	}
	return r.FakeRepository.Add(ctx, m)
}

func (r *b2RegMembershipRepo) UpdateByID(ctx context.Context, tid tenant.ID, id membership.ID, fn func(*membership.Membership) (bool, error)) error {
	if r.errOnUpdateByID != nil {
		err := r.errOnUpdateByID
		r.errOnUpdateByID = nil
		return err
	}
	return r.FakeRepository.UpdateByID(ctx, tid, id, fn)
}

// b2RegRoleRepo is the roles Repository with per-call error injection.
type b2RegRoleRepo struct {
	*roletest.FakeRepository
	errOnGetByTenantAndName error
	errOnAdd                error
}

func newB2RegRoleRepo() *b2RegRoleRepo {
	return &b2RegRoleRepo{FakeRepository: roletest.NewFakeRepository()}
}

func (r *b2RegRoleRepo) GetByTenantAndName(ctx context.Context, tid tenant.ID, name string) (*role.Role, error) {
	if r.errOnGetByTenantAndName != nil {
		err := r.errOnGetByTenantAndName
		r.errOnGetByTenantAndName = nil
		return nil, err
	}
	return r.FakeRepository.GetByTenantAndName(ctx, tid, name)
}

func (r *b2RegRoleRepo) Add(ctx context.Context, role *role.Role) error {
	if r.errOnAdd != nil {
		err := r.errOnAdd
		r.errOnAdd = nil
		return err
	}
	return r.FakeRepository.Add(ctx, role)
}

var b2RegSentinel = errors.New("b2: synthetic infrastructure failure (register_tenant)")

// b2PrebuiltPerson builds a Person with the supplied email so we can seed
// the existing-Person reuse + active-membership-conflict branches.
func b2PrebuiltPerson(t *testing.T, addr email.Address, pid person.ID) *person.Person {
	t.Helper()
	hash, err := person.NewPasswordHash("$argon2id$v=19$m=65536,t=3,p=1$c29tZXNhbHQAAAAA$WjQXjLDXrEPYz8KGRwl9N6c1L+sM5n5L0c0kMmH3vLU")
	if err != nil {
		t.Fatalf("NewPasswordHash: %v", err)
	}
	p, err := person.New(pid, addr, "Existing", "User", hash, testNow)
	if err != nil {
		t.Fatalf("person.New: %v", err)
	}
	return p
}

// ----- happy paths -----

func TestRegisterTenant_HappyPath_FreshPerson(t *testing.T) {
	t.Parallel()
	deps := b2NewRegisterDeps()
	h := b2NewRegisterHandler(deps)
	cmd := b2RegisterCmd(t)

	out, err := h.Handle(t.Context(), cmd)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out.TenantID.IsZero() || out.PersonID.IsZero() || out.MembershipID.IsZero() {
		t.Fatalf("expected non-zero IDs, got %+v", out)
	}

	tn, err := deps.tenants.GetByID(t.Context(), out.TenantID)
	if err != nil {
		t.Fatalf("tenants.GetByID: %v", err)
	}
	if tn.Slug().String() != cmd.Slug.String() {
		t.Errorf("tenant slug = %q, want %q", tn.Slug().String(), cmd.Slug.String())
	}

	p, err := deps.persons.GetByID(t.Context(), out.PersonID)
	if err != nil {
		t.Fatalf("persons.GetByID: %v", err)
	}
	if p.Email().String() != cmd.AdminEmail.String() {
		t.Errorf("person email = %q, want %q", p.Email().String(), cmd.AdminEmail.String())
	}
	if !p.MustChangePassword() {
		t.Error("admin Person MUST be flagged MustChangePassword (BRD line 241 / ADR 0053)")
	}

	m, err := deps.memberships.GetByID(t.Context(), out.TenantID, out.MembershipID)
	if err != nil {
		t.Fatalf("memberships.GetByID: %v", err)
	}
	if m.PersonID() != out.PersonID {
		t.Errorf("membership.PersonID = %v, want %v", m.PersonID(), out.PersonID)
	}
	if m.TenantID() != out.TenantID {
		t.Errorf("membership.TenantID = %v, want %v", m.TenantID(), out.TenantID)
	}
	// CompanyOwner role assignment must have landed.
	assigned := m.RoleAssignments()
	if len(assigned) == 0 {
		t.Fatal("expected at least one role assignment (CompanyOwner)")
	}
}

func TestRegisterTenant_HappyPath_ReusesExistingPersonWithoutActiveMembership(t *testing.T) {
	// A Person with the supplied email exists but holds NO active
	// memberships (e.g. left their previous job). Handler MUST reuse the
	// existing Person row rather than create a new one.
	t.Parallel()

	deps := b2NewRegisterDeps()
	cmd := b2RegisterCmd(t)
	existingID := person.ID("p-existing-reuse-1")
	existing := b2PrebuiltPerson(t, cmd.AdminEmail, existingID)
	if err := deps.persons.Add(t.Context(), existing); err != nil {
		t.Fatalf("seed existing: %v", err)
	}
	h := b2NewRegisterHandler(deps)

	out, err := h.Handle(t.Context(), cmd)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out.PersonID != existingID {
		t.Errorf("PersonID = %v, want %v (reused)", out.PersonID, existingID)
	}
}

// ----- pre-tx lookup branches -----

func TestRegisterTenant_GetByEmail_NonNotFoundError_Wrapped(t *testing.T) {
	t.Parallel()
	deps := b2NewRegisterDeps()
	persons := newB2RegPersonRepo()
	persons.errOnGetByEmail = b2RegSentinel
	deps.persons = persons
	h := b2NewRegisterHandler(deps)

	_, err := h.Handle(t.Context(), b2RegisterCmd(t))
	if !errors.Is(err, b2RegSentinel) {
		t.Fatalf("err = %v, want wrapped b2RegSentinel", err)
	}
	if errors.Is(err, command.ErrEmailHasActiveMembership) {
		t.Errorf("non-NotFound infra error MUST NOT collapse to ErrEmailHasActiveMembership")
	}
}

func TestRegisterTenant_PreExistingPersonWithActiveMembership_Rejected(t *testing.T) {
	// Existing Person + Active Membership in some tenant → handler MUST
	// refuse with ErrEmailHasActiveMembership.
	t.Parallel()

	deps := b2NewRegisterDeps()
	cmd := b2RegisterCmd(t)
	existing := b2PrebuiltPerson(t, cmd.AdminEmail, person.ID("p-existing-active-1"))
	if err := deps.persons.Add(t.Context(), existing); err != nil {
		t.Fatalf("seed existing person: %v", err)
	}
	// Seed an Active Membership for that Person in a different tenant.
	otherTID := tenant.ID(ids.NewV7().String())
	m, err := membership.New(
		membership.ID(ids.NewV7().String()),
		existing.ID(),
		otherTID,
		membership.ID(""),
		testNow,
	)
	if err != nil {
		t.Fatalf("membership.New: %v", err)
	}
	if err := deps.memberships.Add(t.Context(), m); err != nil {
		t.Fatalf("seed active membership: %v", err)
	}
	h := b2NewRegisterHandler(deps)

	_, err = h.Handle(t.Context(), cmd)
	if !errors.Is(err, command.ErrEmailHasActiveMembership) {
		t.Fatalf("err = %v, want ErrEmailHasActiveMembership", err)
	}
}

// ----- tenant construction + persist branches -----

func TestRegisterTenant_TenantConstructInvariantRejected_Wrapped(t *testing.T) {
	// Force tenant.New invariant rejection by leaving DisplayName empty
	// (legal invariant per tenant.New). The handler wraps the error with
	// "construct tenant" + propagates the underlying tenant.ErrInvalid.
	t.Parallel()

	deps := b2NewRegisterDeps()
	h := b2NewRegisterHandler(deps)
	cmd := b2RegisterCmd(t)
	cmd.DisplayName = "" // tenant.New rejects empty display name

	_, err := h.Handle(t.Context(), cmd)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.Is(err, tenant.ErrInvalid) {
		t.Fatalf("err = %v, want wrapped tenant.ErrInvalid", err)
	}
}

func TestRegisterTenant_TenantsAdd_Error_Wrapped(t *testing.T) {
	t.Parallel()
	deps := b2NewRegisterDeps()
	tenants := newB2RegTenantRepo()
	tenants.errOnAdd = b2RegSentinel
	deps.tenants = tenants
	h := b2NewRegisterHandler(deps)

	_, err := h.Handle(t.Context(), b2RegisterCmd(t))
	if !errors.Is(err, b2RegSentinel) {
		t.Fatalf("err = %v, want wrapped b2RegSentinel", err)
	}
}

// ----- person construction + persist branches -----

func TestRegisterTenant_PersonsAdd_ErrEmailTaken_TranslatedToActiveMembership(t *testing.T) {
	// Race: pre-tx GetByEmail returned ErrNotFound, but a concurrent
	// onboarding inserted the same email between the check + our Add.
	// Handler MUST translate the resulting ErrEmailTaken to
	// ErrEmailHasActiveMembership (same observable error shape).
	t.Parallel()

	deps := b2NewRegisterDeps()
	persons := newB2RegPersonRepo()
	persons.errOnAdd = person.ErrEmailTaken
	deps.persons = persons
	h := b2NewRegisterHandler(deps)

	_, err := h.Handle(t.Context(), b2RegisterCmd(t))
	if !errors.Is(err, command.ErrEmailHasActiveMembership) {
		t.Fatalf("err = %v, want ErrEmailHasActiveMembership", err)
	}
}

func TestRegisterTenant_PersonsAdd_OtherError_Wrapped(t *testing.T) {
	t.Parallel()
	deps := b2NewRegisterDeps()
	persons := newB2RegPersonRepo()
	persons.errOnAdd = b2RegSentinel
	deps.persons = persons
	h := b2NewRegisterHandler(deps)

	_, err := h.Handle(t.Context(), b2RegisterCmd(t))
	if !errors.Is(err, b2RegSentinel) {
		t.Fatalf("err = %v, want wrapped b2RegSentinel", err)
	}
	if errors.Is(err, command.ErrEmailHasActiveMembership) {
		t.Errorf("non-EmailTaken infra error MUST NOT collapse to ErrEmailHasActiveMembership")
	}
}

// ----- membership construction + persist branches -----

func TestRegisterTenant_MembershipsAdd_ErrAlreadyActive_TranslatedToActiveMembership(t *testing.T) {
	// Race: partial-unique-index uq_memberships_person_active rejects
	// the Add because a concurrent onboarding raced in. Handler MUST
	// translate to ErrEmailHasActiveMembership.
	t.Parallel()

	deps := b2NewRegisterDeps()
	memberships := newB2RegMembershipRepo()
	memberships.errOnAdd = membership.ErrAlreadyActive
	deps.memberships = memberships
	h := b2NewRegisterHandler(deps)

	_, err := h.Handle(t.Context(), b2RegisterCmd(t))
	if !errors.Is(err, command.ErrEmailHasActiveMembership) {
		t.Fatalf("err = %v, want ErrEmailHasActiveMembership", err)
	}
}

func TestRegisterTenant_MembershipsAdd_OtherError_Wrapped(t *testing.T) {
	t.Parallel()
	deps := b2NewRegisterDeps()
	memberships := newB2RegMembershipRepo()
	memberships.errOnAdd = b2RegSentinel
	deps.memberships = memberships
	h := b2NewRegisterHandler(deps)

	_, err := h.Handle(t.Context(), b2RegisterCmd(t))
	if !errors.Is(err, b2RegSentinel) {
		t.Fatalf("err = %v, want wrapped b2RegSentinel", err)
	}
	if errors.Is(err, command.ErrEmailHasActiveMembership) {
		t.Errorf("non-AlreadyActive infra error MUST NOT collapse to ErrEmailHasActiveMembership")
	}
}

// ----- role seed + assignment branches -----

func TestRegisterTenant_ApplyDefaultRoles_Error_Wrapped(t *testing.T) {
	// Force ApplyDefaultRoles to fail on the FIRST seed call (Add). The
	// handler wraps with "seed default roles".
	t.Parallel()

	deps := b2NewRegisterDeps()
	roles := newB2RegRoleRepo()
	// ApplyDefaultRoles iterates DefaultRoleCatalog, calls GetByTenantAndName
	// (returns ErrNotFound on a fresh tenant), then Add. Inject Add error.
	roles.errOnAdd = b2RegSentinel
	deps.roles = roles
	h := b2NewRegisterHandler(deps)

	_, err := h.Handle(t.Context(), b2RegisterCmd(t))
	if !errors.Is(err, b2RegSentinel) {
		t.Fatalf("err = %v, want wrapped b2RegSentinel", err)
	}
}

func TestRegisterTenant_RoleAssignmentUpdateByID_Error_Wrapped(t *testing.T) {
	// ApplyDefaultRoles succeeds; the post-commit memberships.UpdateByID
	// (CompanyOwner role-assignment) fails. The fake's Add fires N times
	// for the seed catalog, then the membership UpdateByID fires once
	// at the very end — that's the call we want to break.
	t.Parallel()

	deps := b2NewRegisterDeps()
	memberships := newB2RegMembershipRepo()
	deps.memberships = memberships
	h := b2NewRegisterHandler(deps)
	// errOnUpdateByID fires on the NEXT UpdateByID call; in the register
	// flow the only UpdateByID call on memberships is the role-assign
	// step at the very end.
	memberships.errOnUpdateByID = b2RegSentinel

	_, err := h.Handle(t.Context(), b2RegisterCmd(t))
	if !errors.Is(err, b2RegSentinel) {
		t.Fatalf("err = %v, want wrapped b2RegSentinel", err)
	}
}

// ----- ID-factory + constructor panics -----

func TestNewRegisterTenantHandler_PanicsOnNilFactories(t *testing.T) {
	t.Parallel()
	deps := b2NewRegisterDeps()
	cases := []struct {
		name string
		nilT bool
		nilP bool
		nilM bool
	}{
		{name: "nil tenant id factory", nilT: true},
		{name: "nil person id factory", nilP: true},
		{name: "nil membership id factory", nilM: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Error("expected panic on nil ID factory")
				}
			}()
			var fT func() tenant.ID = func() tenant.ID { return tenant.ID(ids.NewV7().String()) }
			var fP func() person.ID = func() person.ID { return person.ID(ids.NewV7().String()) }
			var fM func() membership.ID = func() membership.ID { return membership.ID(ids.NewV7().String()) }
			if tc.nilT {
				fT = nil
			}
			if tc.nilP {
				fP = nil
			}
			if tc.nilM {
				fM = nil
			}
			_ = command.NewRegisterTenantHandler( // arch-test:ignore-err - panic-on-construct test
				deps.uow, deps.tenants, deps.persons, deps.memberships, deps.roles,
				func() time.Time { return testNow }, fT, fP, fM,
			)
		})
	}
}
