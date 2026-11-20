package command_test

import (
	"time"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/passwordpolicy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken/refreshtokentest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant/tenanttest"
)

// A change-password that verified the OLD password must not overwrite a
// password set in the gap between its verify and its write (fix round 12,
// item 5): otherwise an attacker's in-flight change, started with the stolen
// password, commits after the victim's reset and takes the account back.
func TestChangePassword_PasswordReplacedBetweenVerifyAndWrite_IsRefused(t *testing.T) {
	t.Parallel()
	const current = "correct horse battery staple"
	p := newPersonWithPassword(t, current)
	repo := seedPersonRepo(t, p)
	oldHash := p.PasswordHash().String()

	victimHashStr, err := argon2.Hash("the-victims-fresh-password-1")
	if err != nil {
		t.Fatal(err)
	}
	victimHash, err := person.NewPasswordHash(victimHashStr)
	if err != nil {
		t.Fatal(err)
	}

	var raced atomic.Bool
	h := command.NewChangePasswordHandler(repo, refreshtokentest.NewFakeRepository(), tenanttest.NewFakeRepository(), fakeUoW{}, passwordpolicy.Noop{}, func() time.Time { return testNow }).
		WithPasswordVerifier(func(password, encoded string) error {
			verr := argon2.Verify(password, encoded)
			if verr == nil && encoded == oldHash && raced.CompareAndSwap(false, true) {
				// The victim resets the password now: after the attacker's
				// verify passed, before the attacker's write.
				if err := repo.UpdateByID(t.Context(), p.ID(), func(loaded *person.Person) (bool, error) {
					return true, loaded.ChangePassword(victimHash, testNow)
				}); err != nil {
					t.Errorf("victim's change in the gap: %v", err)
				}
			}
			return verr
		})

	err = h.Handle(t.Context(), command.ChangePasswordCommand{
		PersonID:        p.ID(),
		CurrentPassword: current,
		NewPassword:     "Attackers-Own-Password-Chosen-77!",
	})
	if !errors.Is(err, command.ErrIncorrectCurrentPassword) {
		t.Fatalf("change that verified a replaced password = %v, want ErrIncorrectCurrentPassword", err)
	}
	got, err := repo.GetByID(t.Context(), p.ID())
	if err != nil {
		t.Fatal(err)
	}
	if got.PasswordHash().String() != victimHash.String() {
		t.Fatal("the stored password is not the victim's: the stale change overwrote it")
	}
}
