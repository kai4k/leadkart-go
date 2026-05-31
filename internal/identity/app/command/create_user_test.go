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

// failingMembersRepo injects errors on GetActiveForPerson + Add.
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

// failingPersonsRepoForUser injects errors on GetByEmail + Add.
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

// Test-local ID factories — fresh UUIDv7 per call (production shape).
func testNewPersonID() person.ID         { return person.ID(ids.NewV7().String()) }
func testNewMembershipID() membership.ID { return membership.ID(ids.NewV7().String()) }

// TestNewCreateUserHandler_PanicsOnNilDeps — uow, persons, memberships are all
// required at composition time.
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

// TestCreateUser_RejectsZeroTenantID — input guard before any repo/uow call.
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

// TestCreateUser_RejectsZeroEmail — guard-in-depth on the email VO.
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

// TestCreateUser_BrandNewPerson_HappyPath — new Person + Membership minted in
// one pass; PersonExisted == false.
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

// TestCreateUser_RejectsEmptyPassword — hashPassword short-circuits on empty
// password with "create user: password required".
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

// TestCreateUser_PersonsGetByEmail_NonNotFoundError_Wrapped — a generic lookup
// failure wraps as "create user: lookup person" and must NOT collapse to
// not-found (which would mint a duplicate Person).
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

// TestCreateUser_PreExistingPerson_ActiveLookupError_Wrapped — a generic
// active-membership lookup failure wraps as "create user: lookup active"
// (collapse to ErrEmailHasActiveMembership fires only on a real active row).
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

// TestCreateUser_PreExistingPerson_ActiveMembershipFound_RejectsAsHasActive —
// a Person already holding an Active Membership cannot be re-added.
func TestCreateUser_PreExistingPerson_ActiveMembershipFound_RejectsAsHasActive(t *testing.T) {
	t.Parallel()
	addr, err := email.New("recruit@example.test")
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}
	existing := newPersonWithEmail(t, addr.String(), "Recruit", "Existing")
	persons := seedPersonRepo(t, existing)
	members := newFakeMembershipRepo()
	// Seed an active Membership in another tenant — can't add a second.
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

// TestCreateUser_FindOrCreatePerson_AggregateError_Wrapped — empty FirstName
// → person.ErrInvalid, wrapped as "create user: construct person".
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

// TestCreateUser_PersonsAdd_EmailTakenRace_TranslatedToHasActive — a race-loss
// person.ErrEmailTaken on Add collapses to ErrEmailHasActiveMembership.
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

// TestCreateUser_MembershipsAdd_AlreadyActiveRace_TranslatedToHasActive — the
// Membership-side race (partial-unique-index) also collapses to
// ErrEmailHasActiveMembership.
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

// TestCreateUser_PreExistingPerson_NoActiveMembership_AttachesAndReports — a
// Person between jobs is reused (PersonExisted=true).
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

// newPersonWithEmail constructs a Person at the given email (the find-or-create
// arms need control over it, unlike newPersonWithPassword's fixed address).
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
