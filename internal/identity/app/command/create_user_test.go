package command_test


import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership/membershiptest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person/persontest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// failingMembersRepo wraps the shared fake to surface errors from the
// targeted lookup/persist branches. Defaults to delegating to the
// embedded fake; opt-in failures via the per-method err overrides.
type failingMembersRepo struct {
	*membershiptest.FakeRepository
	getActiveErr error // returned from GetActiveForPerson when non-nil
	addErr       error // returned from Add when non-nil
}

func (r *failingMembersRepo) GetActiveForPerson(ctx context.Context, pid person.ID) (*membership.Membership, error) {
	if r.getActiveErr != nil {
		return nil, r.getActiveErr
	}
	return r.FakeRepository.GetActiveForPerson(ctx, pid)
}

func (r *failingMembersRepo) Add(ctx context.Context, m *membership.Membership) error {
	if r.addErr != nil {
		return r.addErr
	}
	return r.FakeRepository.Add(ctx, m)
}

// failingPersonsRepoForUser mirrors the wrapper in
// password_reset_flow_test.go but adds the per-method overrides
// CreateUserHandler exercises (GetByEmail, Add). Kept separate so the
// password-reset wrapper stays focused.
type failingPersonsRepoForUser struct {
	*persontest.FakeRepository
	getByEmailErr error
	addErr        error
}

func (r *failingPersonsRepoForUser) GetByEmail(ctx context.Context, e email.Address) (*person.Person, error) {
	if r.getByEmailErr != nil {
		return nil, r.getByEmailErr
	}
	return r.FakeRepository.GetByEmail(ctx, e)
}

func (r *failingPersonsRepoForUser) Add(ctx context.Context, p *person.Person) error {
	if r.addErr != nil {
		return r.addErr
	}
	return r.FakeRepository.Add(ctx, p)
}

// Test-local factories — tests construct fresh UUIDv7s per call (same
// shape as production wiring). Deterministic pinning happens per-test
// via captured closures when needed.
func testNewPersonID() person.ID         { return person.ID(ids.NewV7().String()) }
func testNewMembershipID() membership.ID { return membership.ID(ids.NewV7().String()) }

// TestNewCreateUserHandler_PanicsOnNilDeps locks the wiring
// contract: NewCreateUserHandler panics fast if any of its three
// required deps (uow, persons, memberships) is nil. Composition
// errors should never reach request time per CLAUDE.md
// "Constructor patterns".
func TestNewCreateUserHandler_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		fn   func()
	}{
		{
			name: "nil uow",
			fn: func() {
				_ = command.NewCreateUserHandler(nil, seedPersonRepo(t, nil), newFakeMembershipRepo(), func() time.Time { return testNow }, testNewPersonID, testNewMembershipID) // arch-test:ignore-err - test fixture setup
			},
		},
		{
			name: "nil persons",
			fn: func() {
				_ = command.NewCreateUserHandler(fakeUoW{}, nil, newFakeMembershipRepo(), func() time.Time { return testNow }, testNewPersonID, testNewMembershipID) // arch-test:ignore-err - test fixture setup
			},
		},
		{
			name: "nil memberships",
			fn: func() {
				_ = command.NewCreateUserHandler(fakeUoW{}, seedPersonRepo(t, nil), nil, func() time.Time { return testNow }, testNewPersonID, testNewMembershipID) // arch-test:ignore-err - test fixture setup
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if r := recover(); r == nil {
					t.Error("expected panic on nil dep")
				}
			}()
			c.fn()
		})
	}
}

// TestCreateUser_RejectsZeroTenantID exercises the input-shape
// guard before any repository or uow is touched. Sibling
// integration tests in flow_integration_test.go drive the happy
// path against a real testcontainers DB.
func TestCreateUser_RejectsZeroTenantID(t *testing.T) {
	t.Parallel()
	addr, err := email.New("recruit@example.test")
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}
	h := command.NewCreateUserHandler(fakeUoW{}, seedPersonRepo(t, nil), newFakeMembershipRepo(), func() time.Time { return testNow }, testNewPersonID, testNewMembershipID)

	_, err = h.Handle(t.Context(), command.CreateUserCommand{
		TenantID: tenant.ID(""),
		Email:    addr,
		Password: "Tr0ub4dor&3-fresh-passphrase",
	})
	if err == nil {
		t.Fatal("expected error for zero tenant id, got nil")
	}
}

// TestCreateUser_RejectsZeroEmail mirrors the tenant-id guard for
// the email VO. Per CLAUDE.md ADR 0022 — DDD ctor validation in
// the domain; HTTP DTO validation at the boundary; the handler
// still asserts both because mid-stack guard-in-depth is cheap.
func TestCreateUser_RejectsZeroEmail(t *testing.T) {
	t.Parallel()
	h := command.NewCreateUserHandler(fakeUoW{}, seedPersonRepo(t, nil), newFakeMembershipRepo(), func() time.Time { return testNow }, testNewPersonID, testNewMembershipID)

	_, err := h.Handle(t.Context(), command.CreateUserCommand{
		TenantID: tenant.ID("11111111-1111-1111-1111-111111111111"),
		Email:    email.Address{},
		Password: "Tr0ub4dor&3-fresh-passphrase",
	})
	if err == nil {
		t.Fatal("expected error for zero email, got nil")
	}
}

// TestCreateUser_BrandNewPerson_HappyPath exercises the
// find-or-create-by-email flow against in-memory fakes. New Person
// + new Membership minted in one fakeUoW pass; PersonExisted == false.
func TestCreateUser_BrandNewPerson_HappyPath(t *testing.T) {
	t.Parallel()
	addr, err := email.New("recruit@example.test")
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}
	persons := seedPersonRepo(t, nil)
	members := newFakeMembershipRepo()
	h := command.NewCreateUserHandler(fakeUoW{}, persons, members, func() time.Time { return testNow }, testNewPersonID, testNewMembershipID)

	res, err := h.Handle(t.Context(), command.CreateUserCommand{
		TenantID:  tenant.ID("11111111-1111-1111-1111-111111111111"),
		Email:     addr,
		Password:  "Tr0ub4dor&3-fresh-passphrase",
		FirstName: "Recruit",
		LastName:  "Test",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if res.PersonExisted {
		t.Error("PersonExisted = true, want false (brand-new Person path)")
	}
	if res.PersonID.IsZero() {
		t.Error("PersonID is zero — handler should mint a fresh person.ID")
	}
	if res.MembershipID == membership.ID("") {
		t.Error("MembershipID is empty — handler should mint a fresh membership.ID")
	}
}

// TestCreateUser_RejectsEmptyPassword exercises the password-required
// guard inside hashPassword (the empty-string short-circuit BEFORE
// argon2 is asked to hash). Surface is the generic
// `"create user: password required"` error.
func TestCreateUser_RejectsEmptyPassword(t *testing.T) {
	t.Parallel()
	addr, err := email.New("recruit@example.test")
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}
	h := command.NewCreateUserHandler(fakeUoW{}, seedPersonRepo(t, nil), newFakeMembershipRepo(), func() time.Time { return testNow }, testNewPersonID, testNewMembershipID)
	_, err = h.Handle(t.Context(), command.CreateUserCommand{
		TenantID: tenant.ID("11111111-1111-1111-1111-111111111111"),
		Email:    addr,
		Password: "", // empty
	})
	if err == nil {
		t.Fatal("expected error for empty password, got nil")
	}
	if !strings.Contains(err.Error(), "password required") {
		t.Errorf("err = %v, want contains 'password required'", err)
	}
}

// TestCreateUser_PersonsGetByEmail_NonNotFoundError_Wrapped — generic
// lookup failure (driver / network / context cancellation) wraps with
// `"create user: lookup person: %w"`. Critical that the handler does
// NOT collapse this to "not found" — that would mint a duplicate
// Person row.
func TestCreateUser_PersonsGetByEmail_NonNotFoundError_Wrapped(t *testing.T) {
	t.Parallel()
	addr, err := email.New("recruit@example.test")
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}
	persons := &failingPersonsRepoForUser{
		FakeRepository: persontest.NewFakeRepository(),
		getByEmailErr:  errBoom,
	}
	h := command.NewCreateUserHandler(fakeUoW{}, persons, newFakeMembershipRepo(), func() time.Time { return testNow }, testNewPersonID, testNewMembershipID)
	_, err = h.Handle(t.Context(), command.CreateUserCommand{
		TenantID: tenant.ID("11111111-1111-1111-1111-111111111111"),
		Email:    addr,
		Password: "Tr0ub4dor&3-strong",
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want chain includes errBoom", err)
	}
}

// TestCreateUser_PreExistingPerson_ActiveLookupError_Wrapped — when
// the Person exists but the active-membership lookup fails with a
// generic error, the handler MUST wrap as `"create user: lookup
// active: %w"`. The collapse-to-ErrEmailHasActiveMembership branch
// fires ONLY on a real ErrNotFound + an actual active row.
func TestCreateUser_PreExistingPerson_ActiveLookupError_Wrapped(t *testing.T) {
	t.Parallel()
	addr, err := email.New("recruit@example.test")
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}
	// Seed a Person globally so the GetByEmail path returns it.
	existing := newPersonWithEmail(t, addr.String(), "Recruit", "Existing")
	persons := seedPersonRepo(t, existing)
	members := &failingMembersRepo{
		FakeRepository: membershiptest.NewFakeRepository(),
		getActiveErr:   errBoom,
	}

	h := command.NewCreateUserHandler(fakeUoW{}, persons, members, func() time.Time { return testNow }, testNewPersonID, testNewMembershipID)
	_, err = h.Handle(t.Context(), command.CreateUserCommand{
		TenantID: tenant.ID("11111111-1111-1111-1111-111111111111"),
		Email:    addr,
		Password: "Tr0ub4dor&3-strong",
	})
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want chain includes errBoom", err)
	}
}

// TestCreateUser_PreExistingPerson_ActiveMembershipFound_RejectsAsHasActive
// exercises the single-Active-Membership invariant: a Person already
// holding an Active Membership somewhere cannot be re-added.
func TestCreateUser_PreExistingPerson_ActiveMembershipFound_RejectsAsHasActive(t *testing.T) {
	t.Parallel()
	addr, err := email.New("recruit@example.test")
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}
	existing := newPersonWithEmail(t, addr.String(), "Recruit", "Existing")
	persons := seedPersonRepo(t, existing)
	members := newFakeMembershipRepo()
	// Seed an active Membership for the existing Person in some OTHER
	// tenant — single-Active-Membership invariant: cannot add to a
	// second tenant while the first is still Active.
	otherTenantID := tenant.ID("22222222-2222-2222-2222-222222222222")
	active, mErr := membership.New(
		membership.ID(ids.NewV7().String()),
		existing.ID(),
		otherTenantID,
		membership.ID(""),
		testNow,
	)
	if mErr != nil {
		t.Fatalf("membership.New: %v", mErr)
	}
	_ = active.PullEvents()
	if addErr := members.Add(t.Context(), active); addErr != nil {
		t.Fatalf("seed Add: %v", addErr)
	}

	h := command.NewCreateUserHandler(fakeUoW{}, persons, members, func() time.Time { return testNow }, testNewPersonID, testNewMembershipID)
	_, err = h.Handle(t.Context(), command.CreateUserCommand{
		TenantID: tenant.ID("11111111-1111-1111-1111-111111111111"),
		Email:    addr,
		Password: "Tr0ub4dor&3-strong",
	})
	if !errors.Is(err, command.ErrEmailHasActiveMembership) {
		t.Fatalf("err = %v, want ErrEmailHasActiveMembership", err)
	}
}

// TestCreateUser_FindOrCreatePerson_AggregateError_Wrapped — when the
// CreateUserCommand carries an empty FirstName (per BRD line 241
// admin-provisioned credentials still require name fields) the
// NewWithMustChangePassword aggregate rejects with person.ErrInvalid.
// Handler wraps as `"create user: construct person: %w"`.
func TestCreateUser_FindOrCreatePerson_AggregateError_Wrapped(t *testing.T) {
	t.Parallel()
	addr, err := email.New("recruit@example.test")
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}
	h := command.NewCreateUserHandler(fakeUoW{}, seedPersonRepo(t, nil), newFakeMembershipRepo(), func() time.Time { return testNow }, testNewPersonID, testNewMembershipID)
	_, err = h.Handle(t.Context(), command.CreateUserCommand{
		TenantID:  tenant.ID("11111111-1111-1111-1111-111111111111"),
		Email:     addr,
		Password:  "Tr0ub4dor&3-strong",
		FirstName: "", // aggregate ctor rejects empty first name
		LastName:  "x",
	})
	if !errors.Is(err, person.ErrInvalid) {
		t.Fatalf("err = %v, want wraps person.ErrInvalid", err)
	}
}

// TestCreateUser_PersonsAdd_EmailTakenRace_TranslatedToHasActive
// exercises the race-loss arm: another concurrent create-user committed
// the Person before us. Pre-tx lookup said NotFound (so we tried to
// Add), but the Add now hits person.ErrEmailTaken. Handler translates
// to ErrEmailHasActiveMembership — same wire-shape as the pre-tx win.
func TestCreateUser_PersonsAdd_EmailTakenRace_TranslatedToHasActive(t *testing.T) {
	t.Parallel()
	addr, err := email.New("recruit@example.test")
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}
	persons := &failingPersonsRepoForUser{
		FakeRepository: persontest.NewFakeRepository(),
		addErr:         person.ErrEmailTaken,
	}
	h := command.NewCreateUserHandler(fakeUoW{}, persons, newFakeMembershipRepo(), func() time.Time { return testNow }, testNewPersonID, testNewMembershipID)
	_, err = h.Handle(t.Context(), command.CreateUserCommand{
		TenantID:  tenant.ID("11111111-1111-1111-1111-111111111111"),
		Email:     addr,
		Password:  "Tr0ub4dor&3-strong",
		FirstName: "Recruit",
		LastName:  "Test",
	})
	if !errors.Is(err, command.ErrEmailHasActiveMembership) {
		t.Fatalf("err = %v, want ErrEmailHasActiveMembership (race-loss collapse)", err)
	}
}

// TestCreateUser_MembershipsAdd_AlreadyActiveRace_TranslatedToHasActive
// — same shape as the Person-side race-loss but on the Membership
// partial-unique-index (uq_memberships_person_active). Surface is the
// same ErrEmailHasActiveMembership friendly error.
func TestCreateUser_MembershipsAdd_AlreadyActiveRace_TranslatedToHasActive(t *testing.T) {
	t.Parallel()
	addr, err := email.New("recruit@example.test")
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}
	members := &failingMembersRepo{
		FakeRepository: membershiptest.NewFakeRepository(),
		addErr:         membership.ErrAlreadyActive,
	}
	h := command.NewCreateUserHandler(fakeUoW{}, seedPersonRepo(t, nil), members, func() time.Time { return testNow }, testNewPersonID, testNewMembershipID)
	_, err = h.Handle(t.Context(), command.CreateUserCommand{
		TenantID:  tenant.ID("11111111-1111-1111-1111-111111111111"),
		Email:     addr,
		Password:  "Tr0ub4dor&3-strong",
		FirstName: "Recruit",
		LastName:  "Test",
	})
	if !errors.Is(err, command.ErrEmailHasActiveMembership) {
		t.Fatalf("err = %v, want ErrEmailHasActiveMembership (membership race-loss)", err)
	}
}

// TestCreateUser_PreExistingPerson_NoActiveMembership_AttachesAndReports
// — happy path for the attach-to-existing arm: Person exists globally
// but is currently between Active Memberships. The handler reuses the
// Person ID + reports PersonExisted=true.
func TestCreateUser_PreExistingPerson_NoActiveMembership_AttachesAndReports(t *testing.T) {
	t.Parallel()
	addr, err := email.New("recruit@example.test")
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}
	existing := newPersonWithEmail(t, addr.String(), "Recruit", "Returning")
	persons := seedPersonRepo(t, existing)
	members := newFakeMembershipRepo() // no Active Memberships seeded

	h := command.NewCreateUserHandler(fakeUoW{}, persons, members, func() time.Time { return testNow }, testNewPersonID, testNewMembershipID)
	res, err := h.Handle(t.Context(), command.CreateUserCommand{
		TenantID:  tenant.ID("11111111-1111-1111-1111-111111111111"),
		Email:     addr,
		Password:  "Tr0ub4dor&3-strong",
		FirstName: "Recruit",
		LastName:  "Test",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if !res.PersonExisted {
		t.Error("PersonExisted = false, want true (attach-to-existing path)")
	}
	if res.PersonID != existing.ID() {
		t.Errorf("PersonID = %s, want %s (reuse existing)", res.PersonID, existing.ID())
	}
	if res.MembershipID.IsZero() {
		t.Error("MembershipID is empty — handler should mint a fresh membership.ID")
	}
}

// newPersonWithEmail constructs a Person at the supplied email. Distinct
// from newPersonWithPassword which uses a hard-coded "alice@example.test"
// — create_user tests need to control the email to exercise the
// find-or-create arms.
func newPersonWithEmail(t *testing.T, addrStr, first, last string) *person.Person {
	t.Helper()
	addr, err := email.New(addrStr)
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}
	hash, err := person.NewPasswordHash("$argon2id$v=19$m=65536,t=3,p=4$YWJjZGVmZ2g$ZS1zdHViLWhhc2gtdGVzdC1maXh0dXJlLW9ubHk")
	if err != nil {
		t.Fatalf("NewPasswordHash: %v", err)
	}
	p, err := person.New(person.ID(ids.NewV7().String()), addr, first, last, hash, testNow)
	if err != nil {
		t.Fatalf("person.New: %v", err)
	}
	_ = p.PullEvents()
	return p
}
