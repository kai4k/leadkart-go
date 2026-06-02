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
	h := command.NewChangePasswordHandler(repo, passwordpolicy.Noop{}, func() time.Time { return testNow })

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
	h := command.NewChangePasswordHandler(repo, passwordpolicy.Noop{}, func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.ChangePasswordCommand{
		PersonID:        p.ID(),
		CurrentPassword: "wrong-current-pw",
		NewPassword:     "anything-stronger-than-floor",
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
	h := command.NewChangePasswordHandler(repo, passwordpolicy.Noop{}, func() time.Time { return testNow })

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
	// Offline list — "password" is in defaultBreachedSet.
	h := command.NewChangePasswordHandler(repo, adapters.NewOfflinePasswordList(), func() time.Time { return testNow })

	err := h.Handle(t.Context(), command.ChangePasswordCommand{
		PersonID:        p.ID(),
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
	plain := "exact-same-passphrase"
	p := newPersonWithPassword(t, plain)
	originalHash := p.PasswordHash().String()
	repo := seedPersonRepo(t, p)
	h := command.NewChangePasswordHandler(repo, passwordpolicy.Noop{}, func() time.Time { return testNow })

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
	if err := p.Anonymise(testNow); err != nil {
		t.Fatalf("Anonymise: %v", err)
	}
	repo := seedPersonRepo(t, p)
	h := command.NewChangePasswordHandler(repo, passwordpolicy.Noop{}, func() time.Time { return testNow })

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
	h := command.NewChangePasswordHandler(repo, passwordpolicy.Noop{}, func() time.Time { return testNow })

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
	h := command.NewChangePasswordHandler(repo, passwordpolicy.Noop{}, func() time.Time { return testNow })

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
		_ = command.NewChangePasswordHandler(nil, passwordpolicy.Noop{}, func() time.Time { return testNow }) // arch-test:ignore-err - test fixture setup
	})
	t.Run("nil breach checker", func(t *testing.T) {
		t.Parallel()
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic on nil breach checker")
			}
		}()
		_ = command.NewChangePasswordHandler(persontest.NewFakeRepository(), nil, func() time.Time { return testNow }) // arch-test:ignore-err - test fixture setup
	})
}
