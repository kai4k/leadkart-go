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

// Handler-unit coverage for RegisterTenantHandler (ADR 0062 §6 / ADR 0047). The
// handler is a 3-step orchestrator: hashAdminPassword, persistAggregatesInTx
// (Tenant+Person+Membership in one UoW tx), seedRolesAndAssignOwner
// (post-commit). Tests cover the happy paths plus every error/race translation.
// File-local fakes use the b2* prefix.

// b2FakeUoW satisfies pg.UnitOfWork by running fn directly — no real tx.
type b2FakeUoW struct{}

func (b2FakeUoW) WithinTx(ctx context.Context, _ pg.TxScope, fn func(ctx context.Context) error) error {
	return fn(ctx)
}

// b2RegisterDeps groups in-memory fakes + the no-op UoW for the handler.
type b2RegisterDeps struct {
	uow         pg.UnitOfWork
	tenants     tenant.Repository
	persons     person.Repository
	memberships membership.Repository
	roles       role.Repository
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

// b2RegisterCmd builds a valid command; callers override fields per test.
func b2RegisterCmd(t *testing.T) command.RegisterTenantCommand {
	t.Helper()
	// Fresh-UUID slug suffix so parallel tests don't collide on the fake's
	// slug-uniqueness index.
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

// b2RegTenantRepo injects a per-call error on Add.
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

// b2RegPersonRepo injects per-call errors on GetByEmail + Add.
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

// b2RegMembershipRepo injects per-call errors on Add + UpdateByID.
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

// b2RegRoleRepo injects per-call errors on GetByTenantAndName + Add.
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

var errB2RegTenant = errors.New("b2: synthetic infrastructure failure (register_tenant)")

// b2PrebuiltPerson builds a Person at the given email for the existing-Person
// reuse + conflict branches.
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
	// Existing Person with no active membership → reuse, don't create.
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
	persons.errOnGetByEmail = errB2RegTenant
	deps.persons = persons
	h := b2NewRegisterHandler(deps)

	_, err := h.Handle(t.Context(), b2RegisterCmd(t))
	if !errors.Is(err, errB2RegTenant) {
		t.Fatalf("err = %v, want wrapped errB2RegTenant", err)
	}
	if errors.Is(err, command.ErrEmailHasActiveMembership) {
		t.Errorf("non-NotFound infra error MUST NOT collapse to ErrEmailHasActiveMembership")
	}
}

func TestRegisterTenant_PreExistingPersonWithActiveMembership_Rejected(t *testing.T) {
	// Existing Person + Active Membership → ErrEmailHasActiveMembership.
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
	// Empty DisplayName trips tenant.New; wrapped "construct tenant" still
	// carries tenant.ErrInvalid.
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
	tenants.errOnAdd = errB2RegTenant
	deps.tenants = tenants
	h := b2NewRegisterHandler(deps)

	_, err := h.Handle(t.Context(), b2RegisterCmd(t))
	if !errors.Is(err, errB2RegTenant) {
		t.Fatalf("err = %v, want wrapped errB2RegTenant", err)
	}
}

// ----- person construction + persist branches -----

func TestRegisterTenant_PersonsAdd_ErrEmailTaken_TranslatedToActiveMembership(t *testing.T) {
	// Race-loss ErrEmailTaken on Add collapses to ErrEmailHasActiveMembership.
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
	persons.errOnAdd = errB2RegTenant
	deps.persons = persons
	h := b2NewRegisterHandler(deps)

	_, err := h.Handle(t.Context(), b2RegisterCmd(t))
	if !errors.Is(err, errB2RegTenant) {
		t.Fatalf("err = %v, want wrapped errB2RegTenant", err)
	}
	if errors.Is(err, command.ErrEmailHasActiveMembership) {
		t.Errorf("non-EmailTaken infra error MUST NOT collapse to ErrEmailHasActiveMembership")
	}
}

// ----- membership construction + persist branches -----

func TestRegisterTenant_MembershipsAdd_ErrAlreadyActive_TranslatedToActiveMembership(t *testing.T) {
	// Race-loss ErrAlreadyActive on Add collapses to ErrEmailHasActiveMembership.
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
	memberships.errOnAdd = errB2RegTenant
	deps.memberships = memberships
	h := b2NewRegisterHandler(deps)

	_, err := h.Handle(t.Context(), b2RegisterCmd(t))
	if !errors.Is(err, errB2RegTenant) {
		t.Fatalf("err = %v, want wrapped errB2RegTenant", err)
	}
	if errors.Is(err, command.ErrEmailHasActiveMembership) {
		t.Errorf("non-AlreadyActive infra error MUST NOT collapse to ErrEmailHasActiveMembership")
	}
}

// ----- role seed + assignment branches -----

func TestRegisterTenant_ApplyDefaultRoles_Error_Wrapped(t *testing.T) {
	// A failing seed Add wraps as "seed default roles".
	t.Parallel()

	deps := b2NewRegisterDeps()
	roles := newB2RegRoleRepo()
	roles.errOnAdd = errB2RegTenant
	deps.roles = roles
	h := b2NewRegisterHandler(deps)

	_, err := h.Handle(t.Context(), b2RegisterCmd(t))
	if !errors.Is(err, errB2RegTenant) {
		t.Fatalf("err = %v, want wrapped errB2RegTenant", err)
	}
}

func TestRegisterTenant_RoleAssignmentUpdateByID_Error_Wrapped(t *testing.T) {
	// The post-commit CompanyOwner role-assignment (the only memberships
	// UpdateByID) fails and wraps.
	t.Parallel()

	deps := b2NewRegisterDeps()
	memberships := newB2RegMembershipRepo()
	deps.memberships = memberships
	h := b2NewRegisterHandler(deps)
	memberships.errOnUpdateByID = errB2RegTenant

	_, err := h.Handle(t.Context(), b2RegisterCmd(t))
	if !errors.Is(err, errB2RegTenant) {
		t.Fatalf("err = %v, want wrapped errB2RegTenant", err)
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
			fT := func() tenant.ID { return tenant.ID(ids.NewV7().String()) }
			fP := func() person.ID { return person.ID(ids.NewV7().String()) }
			fM := func() membership.ID { return membership.ID(ids.NewV7().String()) }
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
