package command_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership/membershiptest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/passwordpolicy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken/refreshtokentest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant/tenanttest"
)

// saturatedGate returns a one-slot gate whose only slot is held for the whole
// test, with a short queue timeout: any password work through it must refuse.
func saturatedGate(t *testing.T) *argon2.Gate {
	t.Helper()
	g := argon2.NewGate(1, 20*time.Millisecond)
	release, err := g.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return g
}

// Each command that hashes or verifies a password refuses with argon2.ErrBusy,
// without doing the work or changing state, when the gate is saturated.
func TestPasswordCommands_RefuseWithErrBusyWhenTheGateIsSaturated(t *testing.T) {
	t.Parallel()
	now := func() time.Time { return testNow }

	t.Run("change password", func(t *testing.T) {
		t.Parallel()
		p := newPersonWithPassword(t, "real-current-pw")
		before := p.PasswordHash().String()
		repo := seedPersonRepo(t, p)
		h := command.NewChangePasswordHandler(repo, refreshtokentest.NewFakeRepository(), tenanttest.NewFakeRepository(), fakeUoW{}, passwordpolicy.Noop{}, now).
			WithArgonGate(saturatedGate(t))
		err := h.Handle(t.Context(), command.ChangePasswordCommand{PersonID: p.ID(), CurrentPassword: "real-current-pw", NewPassword: "A-Brand-New-Passphrase-9"})
		if !errors.Is(err, argon2.ErrBusy) {
			t.Fatalf("err = %v, want ErrBusy", err)
		}
		if p.PasswordHash().String() != before {
			t.Fatal("the password changed although the gate refused")
		}
	})

	t.Run("confirm password reset", func(t *testing.T) {
		t.Parallel()
		addr, _ := email.New("alice@example.test")
		p := newPersonWithPassword(t, "current-pw")
		repo := newResettableRepo(t, p)
		if err := command.NewRequestPasswordResetHandler(repo, now).Handle(t.Context(), command.RequestPasswordResetCommand{Email: addr}); err != nil {
			t.Fatal(err)
		}
		raw := repo.emailRequestedToken(t)
		stamp := p.SecurityStamp().String()
		h := command.NewConfirmPasswordResetHandler(repo, refreshtokentest.NewFakeRepository(), membershiptest.NewFakeRepository(), tenanttest.NewFakeRepository(), fakeUoW{}, passwordpolicy.Noop{}, now).
			WithArgonGate(saturatedGate(t))
		err := h.Handle(t.Context(), command.ConfirmPasswordResetCommand{RawToken: raw, NewPassword: "Tr0ub4dor&3-newly-strong"})
		if !errors.Is(err, argon2.ErrBusy) {
			t.Fatalf("err = %v, want ErrBusy", err)
		}
		if p.SecurityStamp().String() != stamp || p.PendingPasswordReset().IsZero() {
			t.Fatal("the reset was applied although the gate refused")
		}
	})

	t.Run("create user", func(t *testing.T) {
		t.Parallel()
		addr, _ := email.New("recruit@example.test")
		persons := seedPersonRepo(t, nil)
		h := newCreateUserHandlerT(fakeUoW{}, persons, newFakeMembershipRepo(), defaultTenantRepo(t), now, testNewPersonID, testNewMembershipID).
			WithArgonGate(saturatedGate(t))
		_, err := h.Handle(t.Context(), command.CreateUserCommand{Actor: superActor(t),
			TenantID: tenant.ID("11111111-1111-1111-1111-111111111111"), Email: addr,
			Password: "Tr0ub4dor&3-fresh-passphrase", FirstName: "Recruit", LastName: "Test",
		})
		if !errors.Is(err, argon2.ErrBusy) {
			t.Fatalf("err = %v, want ErrBusy", err)
		}
		if _, e := persons.GetByEmail(t.Context(), addr); e == nil {
			t.Fatal("the person was created although the gate refused")
		}
	})
}
