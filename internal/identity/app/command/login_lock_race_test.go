package command_test

import (
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

// A correct password whose verify finished AFTER concurrent failures locked the
// account must not sign in and must not clear the lock (fix round 12, item 2). The
// verifier hook is the barrier: the failures land exactly between this attempt's
// registration and its success reset.
func TestLogin_CorrectPasswordWhileConcurrentFailuresLockTheAccount_IsRefusedAndTheLockSurvives(t *testing.T) {
	t.Parallel()
	rig := newLoginRig(t)
	const plain = "correct horse battery staple"
	p := newPersonWithPassword(t, plain)
	tn := newTenantWithSlug(t, "acme")
	seedLoginCandidate(t, rig, p, activeMembership(t, p, tn))
	if err := rig.tenants.Add(t.Context(), tn); err != nil {
		t.Fatal(err)
	}
	realHash := p.PasswordHash().String()

	var raced atomic.Bool
	h := rig.handler.WithPasswordVerifier(func(password, encoded string) error {
		verr := argon2.Verify(password, encoded)
		if verr == nil && encoded == realHash && raced.CompareAndSwap(false, true) {
			// The rest of a batch of wrong guesses lands while this correct one is
			// being verified, and crosses the threshold.
			for range person.MaxFailedLogins {
				if _, err := rig.persons.UpdateAfterFailedLogin(t.Context(), p.ID(), testNow); err != nil {
					t.Errorf("concurrent failure: %v", err)
				}
			}
		}
		return verr
	})

	_, err := h.Handle(t.Context(), command.LoginCommand{Email: p.Email(), Password: plain, DeviceLabel: "test"})
	if !errors.Is(err, command.ErrAccountLocked) {
		t.Fatalf("a correct password verified after concurrent failures locked the account = %v, want ErrAccountLocked", err)
	}
	if !p.IsLocked(testNow) {
		t.Fatal("the success reset cleared a lock that concurrent failures had set")
	}
	fams, err := rig.families.ListActiveForPerson(t.Context(), p.ID())
	if err != nil {
		t.Fatal(err)
	}
	if len(fams) != 0 {
		t.Fatalf("%d sessions exist for a login that was answered locked", len(fams))
	}
}

// Nine wrong passwords and then the right one signs in (the threshold-crossing
// attempt's password is still checked); and ten wrong ones lock. Both are the
// behaviour before the registration moved ahead of the verify.
func TestLogin_NineWrongThenTheRightOneSignsIn_AndTheLockItSetItselfIsCleared(t *testing.T) {
	t.Parallel()
	rig := newLoginRig(t)
	const plain = "correct horse battery staple"
	p := newPersonWithPassword(t, plain)
	tn := newTenantWithSlug(t, "acme")
	seedLoginCandidate(t, rig, p, activeMembership(t, p, tn))
	if err := rig.tenants.Add(t.Context(), tn); err != nil {
		t.Fatal(err)
	}
	for range person.MaxFailedLogins - 1 {
		if _, err := rig.handler.Handle(t.Context(), command.LoginCommand{Email: p.Email(), Password: "wrong"}); !errors.Is(err, command.ErrInvalidCredentials) {
			t.Fatalf("wrong password = %v", err)
		}
	}
	if _, err := rig.handler.Handle(t.Context(), command.LoginCommand{Email: p.Email(), Password: plain, DeviceLabel: "test"}); err != nil {
		t.Fatalf("the right password at the threshold-crossing attempt = %v, want success", err)
	}
	if p.IsLocked(testNow) || p.FailedLoginCount() != 0 {
		t.Fatalf("count %d locked=%v after a successful login, want cleared", p.FailedLoginCount(), p.IsLocked(testNow))
	}
}

// A1: a password over the size bound is answered exactly like a wrong one on every
// branch: one verify, against the DUMMY hash and never the real one, the attempt
// registered once, and the same outcome.
func TestLogin_OverlongPassword_IsAWrongPasswordOnEveryBranch_NeverHashedAgainstTheRealHash(t *testing.T) {
	t.Parallel()
	const plain = "the-real-password"
	over := strings.Repeat("x", tenant.MaxPasswordBytes+1)
	type branch struct {
		name  string
		build func(t *testing.T) (command.AuthRouter, person.Repository, email.Address, string)
	}
	for _, b := range []branch{
		{"real account", func(t *testing.T) (command.AuthRouter, person.Repository, email.Address, string) {
			p := newPersonWithPassword(t, plain)
			return routerFor(p, someMembership(t, p)), seedPersonRepo(t, p), p.Email(), p.PasswordHash().String()
		}},
		{"real account, no membership", func(t *testing.T) (command.AuthRouter, person.Repository, email.Address, string) {
			p := newPersonWithPassword(t, plain)
			return routerFor(p, nil), seedPersonRepo(t, p), p.Email(), p.PasswordHash().String()
		}},
		{"locked real account", func(t *testing.T) (command.AuthRouter, person.Repository, email.Address, string) {
			p := newPersonAlreadyLocked(t, plain)
			return routerFor(p, someMembership(t, p)), seedPersonRepo(t, p), p.Email(), p.PasswordHash().String()
		}},
		{"unknown email", func(t *testing.T) (command.AuthRouter, person.Repository, email.Address, string) {
			a, _ := email.New("nobody-here@flow.test")
			return &fakeAuthRouter{}, persontestRepo(t), a, ""
		}},
		{"deactivated", func(t *testing.T) (command.AuthRouter, person.Repository, email.Address, string) {
			p := newPersonInactive(t, plain)
			return routerFor(p, nil), seedPersonRepo(t, p), p.Email(), p.PasswordHash().String()
		}},
	} {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			router, repo, addr, realHash := b.build(t)
			phantom := newFakePhantomLockout()
			spy := &verifySpy{}
			dummy, err := argon2.Hash("dummy-for-the-overlong-test")
			if err != nil {
				t.Fatal(err)
			}
			h := handlerWith(t, router, repo, phantom, dummy, func() time.Time { return testNow }).WithPasswordVerifier(spy.verify)
			_, gotErr := h.Handle(t.Context(), command.LoginCommand{Email: addr, Password: over})

			calls := spy.snapshot()
			if len(calls) != 1 {
				t.Fatalf("%d verifies, want exactly one", len(calls))
			}
			if calls[0].encoded != dummy {
				t.Fatalf("the over-long password was verified against a hash that is not the dummy (real hash: %v)", calls[0].encoded == realHash)
			}
			if len(calls[0].password) > tenant.MaxPasswordBytes {
				t.Fatalf("a %d-byte password reached the verifier", len(calls[0].password))
			}
			want := classify(t, func() error {
				// The same attempt with a short wrong password, on a fresh handler.
				router2, repo2, addr2, _ := b.build(t)
				h2 := handlerWith(t, router2, repo2, newFakePhantomLockout(), dummy, func() time.Time { return testNow })
				_, e := h2.Handle(t.Context(), command.LoginCommand{Email: addr2, Password: "short-wrong"})
				return e
			}())
			if got := classify(t, gotErr); got != want {
				t.Fatalf("over-long password answered %v, a wrong password answers %v", got, want)
			}
		})
	}
}

// Item 6: the two hydration reads (role assignments, overrides) belong to a login
// that verified; no failing branch pays for them, so no branch can be told apart by
// the queries it ran.
func TestLogin_MembershipIsHydratedOnlyAfterTheVerify(t *testing.T) {
	t.Parallel()
	rig := newLoginRig(t)
	const plain = "correct horse battery staple"
	p := newPersonWithPassword(t, plain)
	tn := newTenantWithSlug(t, "acme")
	seedLoginCandidate(t, rig, p, activeMembership(t, p, tn))
	if err := rig.tenants.Add(t.Context(), tn); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.handler.Handle(t.Context(), command.LoginCommand{Email: p.Email(), Password: "wrong"}); !errors.Is(err, command.ErrInvalidCredentials) {
		t.Fatalf("wrong password = %v", err)
	}
	if n := rig.router.loads.Load(); n != 0 {
		t.Fatalf("a wrong password hydrated the membership %d times", n)
	}
	if _, err := rig.handler.Handle(t.Context(), command.LoginCommand{Email: p.Email(), Password: plain, DeviceLabel: "t"}); err != nil {
		t.Fatalf("correct password: %v", err)
	}
	if n := rig.router.loads.Load(); n != 1 {
		t.Fatalf("a successful login hydrated the membership %d times, want once", n)
	}
}
