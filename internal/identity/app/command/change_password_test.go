package command_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/identity/adapters"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/passwordpolicy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person/persontest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken/refreshtokentest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant/tenanttest"
)

// seedPersonRepo returns a fresh persontest fake seeded with p (if non-nil).
func seedPersonRepo(t *testing.T, p *person.Person) *persontest.FakeRepository {
	t.Helper()
	repo := persontest.NewFakeRepository()
	if p != nil {
		if err := repo.Add(t.Context(), p); err != nil {
			t.Fatalf("seedPersonRepo: Add: %v", err)
		}
	}
	return repo
}

func newPersonWithPassword(t *testing.T, plain string) *person.Person {
	t.Helper()
	hashStr, err := argon2.Hash(plain)
	if err != nil {
		t.Fatalf("argon2.Hash: %v", err)
	}
	hash, err := person.NewPasswordHash(hashStr)
	if err != nil {
		t.Fatalf("NewPasswordHash: %v", err)
	}
	addr, err := email.New("alice@example.test")
	if err != nil {
		t.Fatalf("email.New: %v", err)
	}
	pid := person.ID("p-test-123")
	p, err := person.New(pid, addr, "Alice", "Test", hash, testNow)
	if err != nil {
		t.Fatalf("person.New: %v", err)
	}
	return p
}

func TestChangePassword_Succeeds(t *testing.T) {
	t.Parallel()
	currentPlain := "correct horse battery staple"
	p := newPersonWithPassword(t, currentPlain)
	repo := seedPersonRepo(t, p)
	h := command.NewChangePasswordHandler(repo, refreshtokentest.NewFakeRepository(), tenanttest.NewFakeRepository(), fakeUoW{}, passwordpolicy.Noop{}, func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.ChangePasswordCommand{
		PersonID:        p.ID(),
		CurrentPassword: currentPlain,
		NewPassword:     "Tr0ub4dor&3-newly-strong-passphrase!",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// State-based assertion: the persisted Person carries the new hash.
	got, err := repo.GetByID(t.Context(), p.ID())
	if err != nil {
		t.Fatalf("GetByID after change: %v", err)
	}
	if vErr := argon2.Verify("Tr0ub4dor&3-newly-strong-passphrase!", got.PasswordHash().String()); vErr != nil {
		t.Errorf("verify new password: %v", vErr)
	}
	if vErr := argon2.Verify(currentPlain, got.PasswordHash().String()); vErr == nil {
		t.Error("verify OLD password against new hash unexpectedly succeeded")
	}
}

func TestChangePassword_RejectsIncorrectCurrentPassword(t *testing.T) {
	t.Parallel()
	p := newPersonWithPassword(t, "real-current-pw")
	originalHash := p.PasswordHash().String()
	repo := seedPersonRepo(t, p)
	h := command.NewChangePasswordHandler(repo, refreshtokentest.NewFakeRepository(), tenanttest.NewFakeRepository(), fakeUoW{}, passwordpolicy.Noop{}, func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.ChangePasswordCommand{
		PersonID:        p.ID(),
		CurrentPassword: "wrong-current-pw",
		NewPassword:     "Anything-Stronger-Than-Floor-9!",
	})
	if !errors.Is(err, command.ErrIncorrectCurrentPassword) {
		t.Fatalf("err = %v, want ErrIncorrectCurrentPassword", err)
	}
	// State-based assertion: hash unchanged on rejection.
	got, _ := repo.GetByID(t.Context(), p.ID())
	if got.PasswordHash().String() != originalHash {
		t.Error("password hash should not change on wrong current password")
	}
}

func TestChangePassword_PersonNotFound_ReturnsIncorrectCurrentPassword(t *testing.T) {
	// "no such person" collapses to "wrong password" to defeat ID-enumeration.
	t.Parallel()
	repo := seedPersonRepo(t, nil) // no Person seeded
	h := command.NewChangePasswordHandler(repo, refreshtokentest.NewFakeRepository(), tenanttest.NewFakeRepository(), fakeUoW{}, passwordpolicy.Noop{}, func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.ChangePasswordCommand{
		PersonID:        person.ID("p-does-not-exist"),
		CurrentPassword: "anything",
		NewPassword:     "anything-else",
	})
	if !errors.Is(err, command.ErrIncorrectCurrentPassword) {
		t.Fatalf("err = %v, want ErrIncorrectCurrentPassword", err)
	}
}

func TestChangePassword_RejectsBreachedPassword(t *testing.T) {
	t.Parallel()
	currentPlain := "real-current-pw"
	p := newPersonWithPassword(t, currentPlain)
	originalHash := p.PasswordHash().String()
	repo := seedPersonRepo(t, p)
	// Offline list — "password" is in defaultBreachedSet. The tenant's policy is
	// length-only (8), so the password reaches the breach check.
	tenants, tid := lenientTenantRepo(t)
	h := command.NewChangePasswordHandler(repo, refreshtokentest.NewFakeRepository(), tenants, fakeUoW{}, adapters.NewOfflinePasswordList(), func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.ChangePasswordCommand{
		PersonID:        p.ID(),
		TenantID:        tid,
		CurrentPassword: currentPlain,
		NewPassword:     "password",
	})
	if !errors.Is(err, command.ErrPasswordBreached) {
		t.Fatalf("err = %v, want ErrPasswordBreached", err)
	}
	got, _ := repo.GetByID(t.Context(), p.ID())
	if got.PasswordHash().String() != originalHash {
		t.Error("breached password must not change hash")
	}
}

func TestChangePassword_RejectsSameAsCurrent(t *testing.T) {
	t.Parallel()
	plain := "Exact-Same-Passphrase-9!"
	p := newPersonWithPassword(t, plain)
	originalHash := p.PasswordHash().String()
	repo := seedPersonRepo(t, p)
	h := command.NewChangePasswordHandler(repo, refreshtokentest.NewFakeRepository(), tenanttest.NewFakeRepository(), fakeUoW{}, passwordpolicy.Noop{}, func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.ChangePasswordCommand{
		PersonID:        p.ID(),
		CurrentPassword: plain,
		NewPassword:     plain,
	})
	if !errors.Is(err, command.ErrPasswordSameAsCurrent) {
		t.Fatalf("err = %v, want ErrPasswordSameAsCurrent", err)
	}
	got, _ := repo.GetByID(t.Context(), p.ID())
	if got.PasswordHash().String() != originalHash {
		t.Error("same-as-current must not change hash")
	}
}

func TestChangePassword_AnonymisedPerson_ReturnsIncorrectCurrentPassword(t *testing.T) {
	// Anonymised Persons surface as "incorrect current password", not
	// "anonymised", to avoid leaking account state.
	t.Parallel()
	currentPlain := "irrelevant"
	p := newPersonWithPassword(t, currentPlain)
	if err := p.Anonymise("test reason", testNow); err != nil {
		t.Fatalf("Anonymise: %v", err)
	}
	repo := seedPersonRepo(t, p)
	h := command.NewChangePasswordHandler(repo, refreshtokentest.NewFakeRepository(), tenanttest.NewFakeRepository(), fakeUoW{}, passwordpolicy.Noop{}, func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.ChangePasswordCommand{
		PersonID:        p.ID(),
		CurrentPassword: currentPlain,
		NewPassword:     "anything-stronger",
	})
	if !errors.Is(err, command.ErrIncorrectCurrentPassword) {
		t.Fatalf("err = %v, want ErrIncorrectCurrentPassword", err)
	}
}

func TestChangePassword_RejectsZeroPersonID(t *testing.T) {
	t.Parallel()
	repo := seedPersonRepo(t, nil)
	h := command.NewChangePasswordHandler(repo, refreshtokentest.NewFakeRepository(), tenanttest.NewFakeRepository(), fakeUoW{}, passwordpolicy.Noop{}, func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.ChangePasswordCommand{
		PersonID:        person.ID(""),
		CurrentPassword: "x",
		NewPassword:     "y",
	})
	if !errors.Is(err, command.ErrPersonIDRequired) {
		t.Fatalf("err = %v, want ErrPersonIDRequired", err)
	}
}

func TestChangePassword_RejectsEmptyNewPassword(t *testing.T) {
	t.Parallel()
	p := newPersonWithPassword(t, "real-current-pw")
	repo := seedPersonRepo(t, p)
	h := command.NewChangePasswordHandler(repo, refreshtokentest.NewFakeRepository(), tenanttest.NewFakeRepository(), fakeUoW{}, passwordpolicy.Noop{}, func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.ChangePasswordCommand{
		PersonID:        p.ID(),
		CurrentPassword: "real-current-pw",
		NewPassword:     "",
	})
	if !errors.Is(err, command.ErrNewPasswordRequired) {
		t.Fatalf("err = %v, want ErrNewPasswordRequired", err)
	}
}

func TestNewChangePasswordHandler_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()
	t.Run("nil persons", func(t *testing.T) {
		t.Parallel()
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic on nil persons repo")
			}
		}()
		_ = command.NewChangePasswordHandler(nil, refreshtokentest.NewFakeRepository(), tenanttest.NewFakeRepository(), fakeUoW{}, passwordpolicy.Noop{}, func() time.Time { return testNow }) // arch-test:ignore-err - test fixture setup
	})
	t.Run("nil families", func(t *testing.T) {
		t.Parallel()
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic on nil families repo")
			}
		}()
		_ = command.NewChangePasswordHandler(persontest.NewFakeRepository(), nil, tenanttest.NewFakeRepository(), fakeUoW{}, passwordpolicy.Noop{}, func() time.Time { return testNow }) // arch-test:ignore-err - test fixture setup
	})
	t.Run("nil unit of work", func(t *testing.T) {
		t.Parallel()
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic on nil unit of work")
			}
		}()
		_ = command.NewChangePasswordHandler(persontest.NewFakeRepository(), refreshtokentest.NewFakeRepository(), tenanttest.NewFakeRepository(), nil, passwordpolicy.Noop{}, func() time.Time { return testNow }) // arch-test:ignore-err - test fixture setup
	})
	t.Run("nil breach checker", func(t *testing.T) {
		t.Parallel()
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic on nil breach checker")
			}
		}()
		_ = command.NewChangePasswordHandler(persontest.NewFakeRepository(), refreshtokentest.NewFakeRepository(), tenanttest.NewFakeRepository(), fakeUoW{}, nil, func() time.Time { return testNow }) // arch-test:ignore-err - test fixture setup
	})
}

// spyAttempts is a budget of n attempts per key that records every key it was
// asked about.
type spyAttempts struct {
	left int
	keys []string
}

func (s *spyAttempts) Allow(key string) (bool, time.Duration) {
	s.keys = append(s.keys, key)
	if s.left <= 0 {
		return false, 12*time.Second + 300*time.Millisecond
	}
	s.left--
	return true, 0
}

// The current password is checked at most as often as the person's budget allows;
// a refused attempt does no password work at all and changes nothing.
func TestChangePassword_PerPersonAttemptBudget(t *testing.T) {
	t.Parallel()
	p := newPersonWithPassword(t, "real-current-pw")
	originalHash := p.PasswordHash().String()
	repo := seedPersonRepo(t, p)
	budget := &spyAttempts{left: 2}
	h := command.NewChangePasswordHandler(repo, refreshtokentest.NewFakeRepository(), tenanttest.NewFakeRepository(), fakeUoW{}, passwordpolicy.Noop{}, func() time.Time { return testNow }).
		WithAttemptLimiter(budget)
	guess := func() error {
		return h.Handle(t.Context(), command.ChangePasswordCommand{PersonID: p.ID(), CurrentPassword: "wrong-guess", NewPassword: "A-New-Strong-Passphrase-1"})
	}
	for i := range 2 {
		if err := guess(); !errors.Is(err, command.ErrIncorrectCurrentPassword) {
			t.Fatalf("attempt %d: %v, want the ordinary wrong-password answer", i+1, err)
		}
	}
	err := guess()
	var tooMany *command.TooManyAttemptsError
	if !errors.As(err, &tooMany) || tooMany.RetryAfter != 12*time.Second+300*time.Millisecond {
		t.Fatalf("third attempt: %v, want TooManyAttemptsError carrying the wait", err)
	}
	for _, k := range budget.keys {
		if k != p.ID().String() {
			t.Fatalf("budget keyed by %q, want the person id", k)
		}
	}
	got, _ := repo.GetByID(t.Context(), p.ID())
	if got.PasswordHash().String() != originalHash {
		t.Error("a refused attempt changed the hash")
	}
}

// An invalid command spends no attempt (it never reaches the password check).
func TestChangePassword_InvalidCommandsSpendNoAttempt(t *testing.T) {
	t.Parallel()
	p := newPersonWithPassword(t, "real-current-pw")
	budget := &spyAttempts{left: 5}
	h := command.NewChangePasswordHandler(seedPersonRepo(t, p), refreshtokentest.NewFakeRepository(), tenanttest.NewFakeRepository(), fakeUoW{}, passwordpolicy.Noop{}, func() time.Time { return testNow }).
		WithAttemptLimiter(budget)
	if err := h.Handle(t.Context(), command.ChangePasswordCommand{PersonID: p.ID(), CurrentPassword: "x"}); !errors.Is(err, command.ErrNewPasswordRequired) {
		t.Fatalf("err = %v", err)
	}
	if len(budget.keys) != 0 {
		t.Fatalf("budget consulted for a command that never checks a password: %v", budget.keys)
	}
}

// lenientTenantRepo seeds one tenant whose policy is length-only (8), and returns
// the repository and the tenant's id.
func lenientTenantRepo(t *testing.T) (*tenanttest.FakeRepository, tenant.ID) {
	t.Helper()
	repo := defaultTenantRepo(t)
	id := tenant.ID("11111111-1111-1111-1111-111111111111")
	policy, err := tenant.NewPasswordPolicy(8, false, false, false, false, 5, 15)
	if err != nil {
		t.Fatalf("NewPasswordPolicy: %v", err)
	}
	if err := repo.UpdateByID(t.Context(), id, func(tn *tenant.Tenant) (bool, error) {
		return true, tn.UpdateSettings(tenant.NewSettings(policy), testNow)
	}); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}
	return repo, id
}
