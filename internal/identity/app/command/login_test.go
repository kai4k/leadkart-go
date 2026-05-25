// login_test.go — handler-unit tests for the LoginHandler failure
// paths (unknown email + wrong password). The negative branches are
// pure handler orchestration over the AuthRouter contract + argon2
// verify + best-effort lockout persistence; no SQL contract is
// exercised, so per TDL canon §6 + ADR 0062 the coverage belongs at
// the fake-backed unit layer, not the integration tier.
//
// Replaces TestFlow_LoginUnknownEmail_GenericFailure +
// TestFlow_LoginWrongPassword_GenericFailure in
// flow_integration_test.go — both asserted only `errors.Is(err,
// ErrInvalidCredentials)`, an observable mirror-able by the fake.
// Wall time: ~200ms (two argon2.Verify calls) vs. testcontainer +
// miniredis + JWT issuer boot for the integration equivalents.

package command_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// fakeAuthRouter is a minimal in-memory AuthRouter for handler-unit
// tests. The contract method `ResolveByEmail` is the single seam the
// Login failure paths exercise; the fake mirrors the SQL adapter's
// observable shape (returns person.ErrNotFound for unknown email;
// returns the seeded (person, membership) tuple for known email).
type fakeAuthRouter struct {
	byEmail map[email.Address]struct {
		p *person.Person
		m *membership.Membership
	}
}

func (f *fakeAuthRouter) ResolveByEmail(_ context.Context, e email.Address) (*person.Person, *membership.Membership, error) {
	row, ok := f.byEmail[e]
	if !ok {
		return nil, nil, person.ErrNotFound
	}
	return row.p, row.m, nil
}

// loginHandlerForFailurePaths constructs a LoginHandler wired with the
// minimum deps needed for the failure-only branches: the AuthRouter +
// person repo (for best-effort lockout persistence on wrong-password)
// + a real dummyHash (so the timing-equalization argon2.Verify runs
// against a valid PHC string). The rest of the deps (refresh families,
// tenants, resolver, jwt) are not reached on failure paths — passing
// nil exposes any drift where the failure branch accidentally falls
// through to the success path.
func loginHandlerForFailurePaths(t *testing.T, router command.AuthRouter, persons person.Repository) command.LoginHandler {
	t.Helper()
	dummy, err := argon2.Hash("dummy-handler-unit-test")
	if err != nil {
		t.Fatalf("dummyHash: %v", err)
	}
	return command.NewLoginHandler(
		router,
		nil, // families — not reached on failure
		nil, // tenants — not reached on failure
		persons,
		nil, // resolver — not reached on failure
		nil, // jwt issuer — not reached on failure
		func() time.Time { return testNow },
		0, // refreshTTL — not reached on failure
		dummy,
		func() refreshtoken.FamilyID { return refreshtoken.FamilyID("never-minted-on-failure") },
	)
}

// TestLogin_UnknownEmail_ReturnsInvalidCredentials covers the
// unknown-email branch: AuthRouter returns person.ErrNotFound; the
// handler runs argon2.Verify against the dummy hash for timing
// equalization (OWASP API §A07 — credential enumeration defense); the
// observable is ErrInvalidCredentials.
//
// SQL coverage of `ErrNotFound` lives in the adapter integration
// tests (membership_repository_pg_test). This test only proves the
// HANDLER ORCHESTRATION on the unknown-email branch.
func TestLogin_UnknownEmail_ReturnsInvalidCredentials(t *testing.T) {
	t.Parallel()
	persons := persontestRepo(t) // empty
	router := &fakeAuthRouter{byEmail: map[email.Address]struct {
		p *person.Person
		m *membership.Membership
	}{}}
	h := loginHandlerForFailurePaths(t, router, persons)

	addr, _ := email.New("nobody@flow.test")
	_, err := h.Handle(t.Context(), command.LoginCommand{
		Email:    addr,
		Password: "any-password",
	})
	if !errors.Is(err, command.ErrInvalidCredentials) {
		t.Fatalf("Handle: got %v, want ErrInvalidCredentials", err)
	}
}

// TestLogin_WrongPassword_ReturnsInvalidCredentials covers the
// wrong-password branch: AuthRouter returns the real (person,
// membership); argon2.Verify on the supplied plaintext returns
// ErrMismatch; the handler records the failed attempt via
// persons.UpdateLockoutState; observable is ErrInvalidCredentials.
//
// State assertion: the seeded Person's FailedLoginCount must increment
// by 1 (the canonical post-condition of RegisterFailedLogin).
func TestLogin_WrongPassword_ReturnsInvalidCredentials(t *testing.T) {
	t.Parallel()
	p := newPersonWithPassword(t, "the-real-password")
	persons := seedPersonRepo(t, p)

	// Person needs an Active Membership for the wrong-password branch
	// (m != nil); else the handler takes the "no Active Membership"
	// branch (which is a separate test case).
	m, err := membership.New(
		membership.ID("11111111-1111-1111-1111-111111111111"),
		p.ID(),
		tenant.ID("33333333-3333-3333-3333-333333333333"),
		membership.ID(""),
		testNow,
	)
	if err != nil {
		t.Fatalf("membership.New: %v", err)
	}

	router := &fakeAuthRouter{byEmail: map[email.Address]struct {
		p *person.Person
		m *membership.Membership
	}{
		p.Email(): {p: p, m: m},
	}}
	h := loginHandlerForFailurePaths(t, router, persons)

	priorFailed := p.FailedLoginCount()
	_, err = h.Handle(t.Context(), command.LoginCommand{
		Email:    p.Email(),
		Password: "WRONG-password",
	})
	if !errors.Is(err, command.ErrInvalidCredentials) {
		t.Fatalf("Handle: got %v, want ErrInvalidCredentials", err)
	}
	if got := p.FailedLoginCount(); got != priorFailed+1 {
		t.Fatalf("FailedLoginCount: got %d, want %d (RegisterFailedLogin not invoked)", got, priorFailed+1)
	}
}

// persontestRepo is a one-line alias so the test reads "empty repo for
// the unknown-email scenario" rather than the longer call chain.
func persontestRepo(t *testing.T) person.Repository {
	t.Helper()
	return seedPersonRepo(t, nil)
}
