package command_test

import (
	"errors"
	"sync/atomic"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

// A login that verified the OLD password must not become a session once the
// password was changed in the gap between its verify and its family insert
// (fix round 12, item 1). The verifier hook is the deterministic barrier: the
// change commits right after the old password verified, exactly in the window
// the review described.
func TestLogin_PasswordChangedBetweenVerifyAndInsert_IsRefused(t *testing.T) {
	t.Parallel()
	rig := newLoginRig(t)

	const plain = "correct horse battery staple"
	p := newPersonWithPassword(t, plain)
	tn := newTenantWithSlug(t, "acme")
	m := activeMembership(t, p, tn)
	seedLoginCandidate(t, rig, p, m)
	if err := rig.tenants.Add(t.Context(), tn); err != nil {
		t.Fatalf("tenants.Add: %v", err)
	}
	oldHash := p.PasswordHash().String()

	newHashStr, err := argon2.Hash("the-new-password-after-change")
	if err != nil {
		t.Fatal(err)
	}
	newHash, err := person.NewPasswordHash(newHashStr)
	if err != nil {
		t.Fatal(err)
	}

	var changed atomic.Bool
	h := rig.handler.WithPasswordVerifier(func(password, encoded string) error {
		verr := argon2.Verify(password, encoded)
		if verr == nil && encoded == oldHash && changed.CompareAndSwap(false, true) {
			// The victim changes the password right now: after the old one
			// verified, before the login persisted its session.
			if err := rig.persons.UpdateByID(t.Context(), p.ID(), func(loaded *person.Person) (bool, error) {
				return true, loaded.ChangePassword(newHash, testNow)
			}); err != nil {
				t.Errorf("password change in the gap: %v", err)
			}
		}
		return verr
	})

	_, err = h.Handle(t.Context(), command.LoginCommand{Email: p.Email(), Password: plain, DeviceLabel: "attacker"})
	if !errors.Is(err, command.ErrInvalidCredentials) {
		t.Fatalf("login that verified the old password after the change = %v, want ErrInvalidCredentials", err)
	}
	fams, err := rig.families.ListActiveForPerson(t.Context(), p.ID())
	if err != nil {
		t.Fatal(err)
	}
	if len(fams) != 0 {
		t.Fatalf("%d active families exist for a login that was refused, want 0", len(fams))
	}
}
