// login_enumeration_test.go — ADR 0068 D12: a failed login must not reveal
// whether the email has an account. Every branch without a usable account runs
// a phantom lockout counter with the real thresholds, and the locked branch
// costs the same as a wrong password. Timing parity is asserted structurally
// (number and target of argon2 verifies), never against a wall-clock threshold.

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
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

// fakePhantomLockout mirrors the Postgres statement's policy (the same one the
// Person aggregate applies): the count restarts when the previous failure is older
// than the window, a lock in force is kept, reaching person.MaxFailedLogins locks
// for person.LockoutDuration. It counts every call it receives and records the
// addresses it was asked about.
type fakePhantomLockout struct {
	mu        sync.Mutex
	rows      map[string]*phantomRow
	calls     int
	registers int
	seen      []string
}

type phantomRow struct {
	count       int
	lastFailed  time.Time
	lockedUntil time.Time
}

func newFakePhantomLockout() *fakePhantomLockout {
	return &fakePhantomLockout{rows: map[string]*phantomRow{}}
}

func (f *fakePhantomLockout) LockedUntil(_ context.Context, e email.Address, now time.Time) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.seen = append(f.seen, e.String())
	if r := f.rows[e.String()]; r != nil && now.Before(r.lockedUntil) {
		return r.lockedUntil, nil
	}
	return time.Time{}, nil
}

func (f *fakePhantomLockout) RegisterFailure(_ context.Context, e email.Address, now time.Time) (person.LockoutOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.registers++
	f.seen = append(f.seen, e.String())
	r := f.rows[e.String()]
	if r == nil {
		r = &phantomRow{}
		f.rows[e.String()] = r
	}
	elapsed := !r.lastFailed.IsZero() && now.Sub(r.lastFailed) > person.LockoutWindow
	if elapsed {
		r.count = 0
	}
	r.count++
	r.lastFailed = now
	stillLocked := now.Before(r.lockedUntil)
	switch {
	case stillLocked:
	case r.count >= person.MaxFailedLogins:
		r.lockedUntil = now.Add(person.LockoutDuration)
	case elapsed:
		r.lockedUntil = time.Time{}
	}
	return person.LockoutOutcome{FailedCount: r.count, LockedUntil: r.lockedUntil, JustLocked: !stillLocked && now.Before(r.lockedUntil)}, nil
}

func (f *fakePhantomLockout) registerCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.registers
}

func (f *fakePhantomLockout) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type outcome string

const (
	outcomeInvalid outcome = "invalid_credentials"
	outcomeLocked  outcome = "locked"
)

func classify(t *testing.T, err error) outcome {
	t.Helper()
	switch {
	case errors.Is(err, command.ErrAccountLocked):
		if command.LockedUntilFromError(err).IsZero() {
			t.Fatal("locked outcome without a LockedUntil (Retry-After would be blank)")
		}
		return outcomeLocked
	case errors.Is(err, command.ErrInvalidCredentials):
		return outcomeInvalid
	default:
		t.Fatalf("unexpected error %v", err)
		return ""
	}
}

// attempts sends n wrong-password logins for addr and returns each outcome.
func attempts(t *testing.T, h command.LoginHandler, addr email.Address, n int) []outcome {
	t.Helper()
	out := make([]outcome, 0, n)
	for range n {
		_, err := h.Handle(t.Context(), command.LoginCommand{Email: addr, Password: "WRONG-password"})
		out = append(out, classify(t, err))
	}
	return out
}

// lockoutSurface is the sequence every branch must produce: invalid for
// attempts 1..MaxFailedLogins-1, locked from the MaxFailedLogins-th on.
func lockoutSurface() []outcome {
	out := make([]outcome, 0, person.MaxFailedLogins+1)
	for range person.MaxFailedLogins - 1 {
		out = append(out, outcomeInvalid)
	}
	return append(out, outcomeLocked, outcomeLocked)
}

func equalOutcomes(a, b []outcome) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func routerFor(p *person.Person, m *membership.Membership) *fakeAuthRouter {
	return &fakeAuthRouter{byEmail: map[email.Address]struct {
		p *person.Person
		m *membership.Membership
	}{p.Email(): {p: p, m: m}}}
}

func someMembership(t *testing.T, p *person.Person) *membership.Membership {
	t.Helper()
	return activeMembership(t, p, newTenantWithSlug(t, "acme"))
}

func TestLogin_UnknownEmail_LocksOnTheSameScheduleAsARealAccount(t *testing.T) {
	t.Parallel()
	addr, _ := email.New("nobody-here@flow.test")
	h := loginHandlerForFailurePaths(t, &fakeAuthRouter{}, persontestRepo(t))
	got := attempts(t, h, addr, person.MaxFailedLogins+1)
	if want := lockoutSurface(); !equalOutcomes(got, want) {
		t.Fatalf("unknown email: %v, want %v", got, want)
	}
	// A different unknown email has its own counter.
	other, _ := email.New("someone-else@flow.test")
	if got := attempts(t, h, other, 1); got[0] != outcomeInvalid {
		t.Fatalf("a fresh unknown email answered %v", got[0])
	}
}

func TestLogin_RealAccountWrongPassword_HasTheSameSurfaceAsUnknownEmail(t *testing.T) {
	t.Parallel()
	for name, withMembership := range map[string]bool{"active membership": true, "no active membership": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := newPersonWithPassword(t, "the-real-password")
			var m *membership.Membership
			if withMembership {
				m = someMembership(t, p)
			}
			h := loginHandlerForFailurePaths(t, routerFor(p, m), seedPersonRepo(t, p))
			got := attempts(t, h, p.Email(), person.MaxFailedLogins+1)
			if want := lockoutSurface(); !equalOutcomes(got, want) {
				t.Fatalf("real account: %v, want the unknown-email schedule %v", got, want)
			}
		})
	}
}

func TestLogin_InactiveAndAnonymisedPersons_LockLikeAnUnknownEmail(t *testing.T) {
	t.Parallel()
	for name, mk := range map[string]func(*testing.T) *person.Person{
		"inactive":   func(t *testing.T) *person.Person { return newPersonInactive(t, "pw") },
		"anonymised": newPersonAnonymised,
		"suspended":  func(t *testing.T) *person.Person { return newPersonSuspended(t, "pw") },
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := mk(t)
			h := loginHandlerForFailurePaths(t, routerFor(p, nil), seedPersonRepo(t, p))
			if got, want := attempts(t, h, p.Email(), person.MaxFailedLogins+1), lockoutSurface(); !equalOutcomes(got, want) {
				t.Fatalf("%s: %v, want %v", name, got, want)
			}
			if p.FailedLoginCount() != 0 {
				t.Fatalf("a terminal Person's real counter moved to %d", p.FailedLoginCount())
			}
		})
	}
}

// A person who was locked before being deactivated must not answer 423 on the
// first attempt: that is the one signal an unknown email never produces.
func TestLogin_InactivePersonWithALeftoverRealLock_AnswersLikeAnUnknownEmail(t *testing.T) {
	t.Parallel()
	locked := newPersonAlreadyLocked(t, "pw")
	p := person.UnmarshalFromDB(person.Snapshot{
		ID: locked.ID(), Email: locked.Email(), FirstName: locked.FirstName(), LastName: locked.LastName(),
		PasswordHash: locked.PasswordHash(), SecurityStamp: locked.SecurityStamp(), IsActive: false,
		CreatedAt: locked.CreatedAt(), FailedLoginCount: locked.FailedLoginCount(),
		LockedUntil: locked.LockedUntil(), LastFailedLoginAt: testNow,
	})
	if !p.IsLocked(testNow) {
		t.Fatal("setup: the deactivated Person's real lock did not survive rehydration")
	}
	h := loginHandlerForFailurePaths(t, routerFor(p, nil), seedPersonRepo(t, p))
	if got := attempts(t, h, p.Email(), 1); got[0] != outcomeInvalid {
		t.Fatalf("first attempt on a deactivated, formerly locked account: %v, want invalid_credentials", got[0])
	}
}

// Fix round 12 (item 2) changed this deliberately: every attempt registers
// atomically BEFORE its password is checked, the locked ones included (the phantom
// counter does exactly the same, so the two stay one shape). What must never move
// is the LOCK: it is not extended and a correct password does not clear it.
func TestLogin_LockedRealAccount_CorrectPasswordStaysLocked_LockNeverMoves(t *testing.T) {
	t.Parallel()
	const plain = "the-real-password"
	p := newPersonAlreadyLocked(t, plain)
	before, until := p.FailedLoginCount(), p.LockedUntil()
	h := loginHandlerForFailurePaths(t, routerFor(p, someMembership(t, p)), seedPersonRepo(t, p))
	_, err := h.Handle(t.Context(), command.LoginCommand{Email: p.Email(), Password: plain})
	if classify(t, err) != outcomeLocked {
		t.Fatalf("got %v, want locked", err)
	}
	if !p.LockedUntil().Equal(until) || !p.IsLocked(testNow) {
		t.Fatalf("a locked attempt moved the lock: lockedUntil %v -> %v", until, p.LockedUntil())
	}
	if p.FailedLoginCount() != before+1 {
		t.Fatalf("a locked attempt registered %d attempts, want exactly one (%d -> %d)",
			p.FailedLoginCount()-before, before, p.FailedLoginCount())
	}
}

func TestLogin_RealAccounts_NeverTouchThePhantomCounter(t *testing.T) {
	t.Parallel()
	p := newPersonWithPassword(t, "the-real-password")
	phantom := newFakePhantomLockout()
	h := loginHandlerWithPhantom(t, routerFor(p, someMembership(t, p)), seedPersonRepo(t, p), phantom)
	attempts(t, h, p.Email(), person.MaxFailedLogins+1)
	if n := phantom.callCount(); n != 0 {
		t.Fatalf("the phantom counter was consulted %d times for a real account", n)
	}
}

// Timing parity by structure: every failure branch costs exactly one argon2
// verify, and only the wrong-password branches verify against the real hash.
func TestLogin_EveryFailureBranchRunsExactlyOneVerify_RealHashOnlyForWrongPasswords(t *testing.T) {
	t.Parallel()
	const plain = "the-real-password"
	type rig struct {
		router command.AuthRouter
		repo   person.Repository
		p      *person.Person // nil = unknown email
		warmup int            // failed attempts to send first (reaches the phantom lock)
	}
	realAccount := func(t *testing.T, p *person.Person, m *membership.Membership) rig {
		return rig{router: routerFor(p, m), repo: seedPersonRepo(t, p), p: p}
	}
	for _, b := range []struct {
		name     string
		build    func(t *testing.T) rig
		password string
		realHash bool // the single verify targets the Person's real hash
	}{
		{"wrong password", func(t *testing.T) rig {
			p := newPersonWithPassword(t, plain)
			return realAccount(t, p, someMembership(t, p))
		}, "WRONG", true},
		{"locked real account, correct password", func(t *testing.T) rig {
			p := newPersonAlreadyLocked(t, plain)
			return realAccount(t, p, someMembership(t, p))
		}, plain, false},
		{"locked real account, wrong password", func(t *testing.T) rig {
			p := newPersonAlreadyLocked(t, plain)
			return realAccount(t, p, someMembership(t, p))
		}, "WRONG", false},
		{"no active membership, wrong password", func(t *testing.T) rig {
			return realAccount(t, newPersonWithPassword(t, plain), nil)
		}, "WRONG", true},
		{"inactive", func(t *testing.T) rig { return realAccount(t, newPersonInactive(t, plain), nil) }, plain, false},
		{"anonymised", func(t *testing.T) rig { return realAccount(t, newPersonAnonymised(t), nil) }, plain, false},
		{"globally suspended", func(t *testing.T) rig {
			p := newPersonSuspended(t, plain)
			return realAccount(t, p, someMembership(t, p))
		}, plain, false},
		{"unknown email", func(t *testing.T) rig {
			return rig{router: &fakeAuthRouter{}, repo: persontestRepo(t)}
		}, plain, false},
		{"unknown email, already phantom-locked", func(t *testing.T) rig {
			return rig{router: &fakeAuthRouter{}, repo: persontestRepo(t), warmup: person.MaxFailedLogins}
		}, plain, false},
	} {
		t.Run(b.name, func(t *testing.T) {
			t.Parallel()
			r := b.build(t)
			addr, _ := email.New("nobody-here@flow.test")
			if r.p != nil {
				addr = r.p.Email()
			}
			var targets []string
			h := loginHandlerForFailurePaths(t, r.router, r.repo).WithPasswordVerifier(func(pw, enc string) error {
				targets = append(targets, enc)
				return argon2.Verify(pw, enc)
			})
			attempts(t, h, addr, r.warmup)
			targets = nil
			if _, err := h.Handle(t.Context(), command.LoginCommand{Email: addr, Password: b.password}); err == nil {
				t.Fatal("login unexpectedly succeeded")
			}
			if len(targets) != 1 {
				t.Fatalf("%d password verifies, want exactly 1 (timing parity)", len(targets))
			}
			if usedReal := r.p != nil && targets[0] == r.p.PasswordHash().String(); usedReal != b.realHash {
				t.Fatalf("verify against the real hash = %v, want %v", usedReal, b.realHash)
			}
		})
	}
}
