// login_atomic_test.go — ADR 0068 D12 after the atomic-counter round: what the
// handler asks of its collaborators (verify arguments, one clock read after the
// verify, a refusal answered the same on every branch) and the Person-side
// behaviours the atomic statement must keep. The concurrency itself is proven
// against Postgres in login_concurrency_integration_test.go.

package command_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
)

// verifyCall is one argument pair the handler gave its password verifier.
type verifyCall struct{ password, encoded string }

type verifySpy struct {
	mu    sync.Mutex
	calls []verifyCall
	// err, when set, is what every verify answers instead of running.
	err error
	// onVerify runs after each recorded call (a test's stand-in for argon2 time).
	onVerify func()
}

func (s *verifySpy) verify(password, encoded string) error {
	s.mu.Lock()
	s.calls = append(s.calls, verifyCall{password, encoded})
	f, err := s.onVerify, s.err
	s.mu.Unlock()
	if f != nil {
		f()
	}
	if err != nil {
		return err
	}
	return argon2.Verify(password, encoded)
}

func (s *verifySpy) snapshot() []verifyCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]verifyCall(nil), s.calls...)
}

// handlerWith builds a failure-path handler over caller-supplied clock, dummy
// hash and phantom counter (loginHandlerWithPhantom fixes the first two).
func handlerWith(t *testing.T, router command.AuthRouter, persons person.Repository, phantom command.PhantomFailureCounter, dummy string, now func() time.Time) command.LoginHandler {
	t.Helper()
	return command.NewLoginHandler(router, nil, nil, persons, nil, nil, now, 0, 0, dummy,
		func() refreshtoken.FamilyID { return refreshtoken.FamilyID("never-minted-on-failure") }, phantom, fakeUoW{})
}

// Every branch that has no real hash to check runs its one verify against the
// handler's dummy hash WITH THE PASSWORD THE CALLER SENT: a verify of some other
// string would be cheaper or costlier and tell the branches apart. Counting the
// calls (the sibling test) cannot catch that; the arguments can.
func TestLogin_DummyVerifyUsesTheDummyHashAndTheSuppliedPassword(t *testing.T) {
	t.Parallel()
	const plain = "the-real-password"
	dummy, err := argon2.Hash("the-dummy-secret")
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range []struct {
		name     string
		build    func(t *testing.T) (command.AuthRouter, person.Repository, email.Address)
		password string
	}{
		{"locked real account, correct password", func(t *testing.T) (command.AuthRouter, person.Repository, email.Address) {
			p := newPersonAlreadyLocked(t, plain)
			return routerFor(p, someMembership(t, p)), seedPersonRepo(t, p), p.Email()
		}, plain},
		{"locked real account, wrong password", func(t *testing.T) (command.AuthRouter, person.Repository, email.Address) {
			p := newPersonAlreadyLocked(t, plain)
			return routerFor(p, someMembership(t, p)), seedPersonRepo(t, p), p.Email()
		}, "WRONG-1"},
		{"unknown email", func(t *testing.T) (command.AuthRouter, person.Repository, email.Address) {
			a, _ := email.New("nobody-here@flow.test")
			return &fakeAuthRouter{}, persontestRepo(t), a
		}, "WRONG-2"},
		{"inactive", func(t *testing.T) (command.AuthRouter, person.Repository, email.Address) {
			p := newPersonInactive(t, plain)
			return routerFor(p, nil), seedPersonRepo(t, p), p.Email()
		}, "WRONG-3"},
		{"anonymised", func(t *testing.T) (command.AuthRouter, person.Repository, email.Address) {
			p := newPersonAnonymised(t)
			return routerFor(p, nil), seedPersonRepo(t, p), p.Email()
		}, "WRONG-4"},
	} {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			router, repo, addr := b.build(t)
			spy := &verifySpy{}
			h := handlerWith(t, router, repo, newFakePhantomLockout(), dummy, func() time.Time { return testNow }).
				WithPasswordVerifier(spy.verify)
			if _, err := h.Handle(t.Context(), command.LoginCommand{Email: addr, Password: b.password}); err == nil {
				t.Fatal("login unexpectedly succeeded")
			}
			got := spy.snapshot()
			if len(got) != 1 {
				t.Fatalf("%d verifies, want 1", len(got))
			}
			if got[0].password != b.password || got[0].encoded != dummy {
				t.Fatalf("verify(%q, <dummy=%v>), want (%q, dummy hash)", got[0].password, got[0].encoded == dummy, b.password)
			}
		})
	}
}

// A real account's wrong password verifies the supplied password against ITS hash.
func TestLogin_WrongPasswordVerifiesTheSuppliedPasswordAgainstTheRealHash(t *testing.T) {
	t.Parallel()
	p := newPersonWithPassword(t, "the-real-password")
	spy := &verifySpy{}
	h := loginHandlerForFailurePaths(t, routerFor(p, someMembership(t, p)), seedPersonRepo(t, p)).WithPasswordVerifier(spy.verify)
	if _, err := h.Handle(t.Context(), command.LoginCommand{Email: p.Email(), Password: "WRONG-5"}); !errors.Is(err, command.ErrInvalidCredentials) {
		t.Fatalf("err = %v", err)
	}
	got := spy.snapshot()
	if len(got) != 1 || got[0].password != "WRONG-5" || got[0].encoded != p.PasswordHash().String() {
		t.Fatalf("verify calls = %+v", got)
	}
}

// A correct password with nine failures behind it still signs in and clears the
// counter: the lock is reached on the TENTH failure, and a success resets.
func TestLogin_CorrectPasswordAtNineFailuresSucceedsAndResetsTheCounter(t *testing.T) {
	t.Parallel()
	const plain = "the-real-password"
	rig := newLoginRig(t)
	p := newPersonWithPassword(t, plain)
	tn := newTenantWithSlug(t, "acme")
	seedLoginCandidate(t, rig, p, activeMembership(t, p, tn))
	if err := rig.tenants.Add(t.Context(), tn); err != nil {
		t.Fatalf("tenants.Add: %v", err)
	}
	for range person.MaxFailedLogins - 1 {
		if _, err := rig.handler.Handle(t.Context(), command.LoginCommand{Email: p.Email(), Password: "WRONG-6"}); !errors.Is(err, command.ErrInvalidCredentials) {
			t.Fatalf("failure: %v", err)
		}
	}
	if got := p.FailedLoginCount(); got != person.MaxFailedLogins-1 {
		t.Fatalf("failed count before the correct login = %d, want %d", got, person.MaxFailedLogins-1)
	}
	if _, err := rig.handler.Handle(t.Context(), command.LoginCommand{Email: p.Email(), Password: plain, DeviceLabel: "test"}); err != nil {
		t.Fatalf("the correct password at %d failures was refused: %v", person.MaxFailedLogins-1, err)
	}
	if p.FailedLoginCount() != 0 || p.IsLocked(testNow) {
		t.Fatalf("after success: count=%d locked=%v, want 0 / unlocked", p.FailedLoginCount(), p.IsLocked(testNow))
	}
}

// The clock is read ONCE, after the verify, on the real and the phantom branch:
// the tenth failure's lock then starts at the same offset from the verify on both,
// so Retry-After and the lock length match to the second. A path that read the
// clock before the verify would differ by exactly the verify's duration.
func TestLogin_RealAndPhantomLocksStartAtTheSameInstantRelativeToTheVerify(t *testing.T) {
	t.Parallel()
	const plain = "the-real-password"
	dummy, err := argon2.Hash("dummy-for-clock-test")
	if err != nil {
		t.Fatal(err)
	}
	const verifyTime = 7 * time.Second
	run := func(real bool) time.Duration {
		var mu sync.Mutex
		clock := testNow
		now := func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
		spy := &verifySpy{onVerify: func() { mu.Lock(); clock = clock.Add(verifyTime); mu.Unlock() }}
		var router command.AuthRouter = &fakeAuthRouter{}
		repo := persontestRepo(t)
		addr, _ := email.New("clock-test@flow.test")
		if real {
			p := newPersonWithPassword(t, plain)
			router, repo, addr = routerFor(p, someMembership(t, p)), seedPersonRepo(t, p), p.Email()
		}
		h := handlerWith(t, router, repo, newFakePhantomLockout(), dummy, now).WithPasswordVerifier(spy.verify)
		var until time.Time
		var lastVerifyEnd time.Time
		for range person.MaxFailedLogins {
			_, err := h.Handle(t.Context(), command.LoginCommand{Email: addr, Password: "WRONG-7"})
			mu.Lock()
			lastVerifyEnd = clock
			mu.Unlock()
			if errors.Is(err, command.ErrAccountLocked) {
				until = command.LockedUntilFromError(err)
			}
		}
		if until.IsZero() {
			t.Fatalf("real=%v: never locked", real)
		}
		return until.Add(-person.LockoutDuration).Sub(lastVerifyEnd)
	}
	realOffset, phantomOffset := run(true), run(false)
	if realOffset != phantomOffset {
		t.Fatalf("the lock starts %v after the last verify on a real account and %v on an unknown address", realOffset, phantomOffset)
	}
}

// When the password work cannot start (the gate is saturated) EVERY branch answers
// the same refusal, and none of them counts an attempt: a refusal that only the
// unknown-email (or only the real-account) branch gave would be an oracle. The slot
// is taken BEFORE the attempt is registered (fix round 12, item 2), so the refusal
// is injected at the acquisition; the assertions are unchanged.
func TestLogin_AGateRefusalIsTheSameOnEveryBranchAndCountsNothing(t *testing.T) {
	t.Parallel()
	const plain = "the-real-password"
	for _, b := range []struct {
		name  string
		build func(t *testing.T) (command.AuthRouter, person.Repository, email.Address)
	}{
		{"real account, wrong password", func(t *testing.T) (command.AuthRouter, person.Repository, email.Address) {
			p := newPersonWithPassword(t, plain)
			return routerFor(p, someMembership(t, p)), seedPersonRepo(t, p), p.Email()
		}},
		{"real account, no membership", func(t *testing.T) (command.AuthRouter, person.Repository, email.Address) {
			p := newPersonWithPassword(t, plain)
			return routerFor(p, nil), seedPersonRepo(t, p), p.Email()
		}},
		{"locked real account", func(t *testing.T) (command.AuthRouter, person.Repository, email.Address) {
			p := newPersonAlreadyLocked(t, plain)
			return routerFor(p, someMembership(t, p)), seedPersonRepo(t, p), p.Email()
		}},
		{"unknown email", func(t *testing.T) (command.AuthRouter, person.Repository, email.Address) {
			a, _ := email.New("nobody-here@flow.test")
			return &fakeAuthRouter{}, persontestRepo(t), a
		}},
		{"deactivated", func(t *testing.T) (command.AuthRouter, person.Repository, email.Address) {
			p := newPersonInactive(t, plain)
			return routerFor(p, nil), seedPersonRepo(t, p), p.Email()
		}},
		{"anonymised", func(t *testing.T) (command.AuthRouter, person.Repository, email.Address) {
			p := newPersonAnonymised(t)
			return routerFor(p, nil), seedPersonRepo(t, p), p.Email()
		}},
	} {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			router, repo, addr := b.build(t)
			phantom := newFakePhantomLockout()
			spy := &verifySpy{}
			h := loginHandlerWithPhantom(t, router, repo, phantom).WithPasswordVerifier(spy.verify).
				WithGateAcquire(func(context.Context) (func(), error) { return nil, argon2.ErrBusy })
			_, err := h.Handle(t.Context(), command.LoginCommand{Email: addr, Password: "WRONG-8"})
			if !errors.Is(err, argon2.ErrBusy) {
				t.Fatalf("err = %v, want argon2.ErrBusy on every branch", err)
			}
			if phantom.registerCount() != 0 {
				t.Fatalf("a refused attempt registered %d phantom failures", phantom.registerCount())
			}
			if p, e := repo.GetByEmail(t.Context(), addr); e == nil && p.FailedLoginCount() != 0 && !p.IsLocked(testNow) {
				t.Fatalf("a refused attempt bumped the real counter to %d", p.FailedLoginCount())
			}
		})
	}
}

// A request that is gone before its slot is taken is a refusal too, not a wrong
// password: nothing was registered. (A request that goes away MID-verify had
// started its work and is counted: the attempt was registered before the verify.)
func TestLogin_ACancelledRequestIsNotCountedAsAFailedAttempt(t *testing.T) {
	t.Parallel()
	p := newPersonWithPassword(t, "the-real-password")
	repo := seedPersonRepo(t, p)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	h := loginHandlerForFailurePaths(t, routerFor(p, someMembership(t, p)), repo).
		WithGateAcquire(func(ctx context.Context) (func(), error) { return nil, ctx.Err() })
	_, err := h.Handle(ctx, command.LoginCommand{Email: p.Email(), Password: "WRONG-9"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if p.FailedLoginCount() != 0 {
		t.Fatalf("a cancelled request counted as a failure: %d", p.FailedLoginCount())
	}
}

func TestLogin_ARequestCancelledMidVerifyIsCountedOnce(t *testing.T) {
	t.Parallel()
	p := newPersonWithPassword(t, "the-real-password")
	repo := seedPersonRepo(t, p)
	ctx, cancel := context.WithCancel(t.Context())
	spy := &verifySpy{onVerify: cancel, err: context.Canceled}
	h := loginHandlerForFailurePaths(t, routerFor(p, someMembership(t, p)), repo).WithPasswordVerifier(spy.verify)
	_, err := h.Handle(ctx, command.LoginCommand{Email: p.Email(), Password: "WRONG-9"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if p.FailedLoginCount() != 1 {
		t.Fatalf("a request cancelled mid-verify was registered %d times, want exactly once", p.FailedLoginCount())
	}
}
