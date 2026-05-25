package command_test


import (
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

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
