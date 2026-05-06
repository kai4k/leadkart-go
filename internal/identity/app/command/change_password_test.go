package command_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/clock"
	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/platform/breach"
)

// fakePersonRepo is the minimum [person.Repository] surface the
// ChangePasswordHandler exercises — GetByID + UpdateByID. Add /
// GetByEmail are unused; we still implement them so the type
// satisfies the interface and the compile-time assertion below
// catches drift.
type fakePersonRepo struct {
	person *person.Person

	// updateCalls counts UpdateByID invocations that committed
	// (closure returned true). False-return closures (no-op) +
	// error returns don't bump the counter.
	updateCalls int

	getErr error
}

func (f *fakePersonRepo) Add(_ context.Context, _ *person.Person) error { return nil }

func (f *fakePersonRepo) GetByID(_ context.Context, id person.ID) (*person.Person, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.person == nil || f.person.ID() != id {
		return nil, person.ErrNotFound
	}
	return f.person, nil
}

func (f *fakePersonRepo) GetByEmail(_ context.Context, _ email.Address) (*person.Person, error) {
	return nil, person.ErrNotFound
}

func (f *fakePersonRepo) GetByPasswordResetTokenHash(_ context.Context, _ person.PasswordResetTokenHash) (*person.Person, error) {
	return nil, person.ErrNotFound
}

func (f *fakePersonRepo) GetByEmailChangeTokenHash(_ context.Context, _ person.EmailChangeTokenHash) (*person.Person, error) {
	return nil, person.ErrNotFound
}

func (f *fakePersonRepo) UpdateByID(_ context.Context, id person.ID, fn func(*person.Person) (bool, error)) error {
	if f.person == nil || f.person.ID() != id {
		return person.ErrNotFound
	}
	commit, err := fn(f.person)
	if err != nil {
		return err
	}
	if commit {
		f.updateCalls++
	}
	return nil
}

var _ person.Repository = (*fakePersonRepo)(nil)

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
	p, err := person.New(pid, addr, "Alice", "Test", hash)
	if err != nil {
		t.Fatalf("person.New: %v", err)
	}
	return p
}

func freezeClock(t *testing.T) {
	t.Helper()
	clock.Set(time.Date(2026, 5, 7, 12, 0, 0, 0, time.UTC))
	t.Cleanup(clock.Reset)
}

func TestChangePassword_Succeeds(t *testing.T) {
	t.Parallel()
	freezeClock(t)
	currentPlain := "correct horse battery staple"
	repo := &fakePersonRepo{person: newPersonWithPassword(t, currentPlain)}
	h := command.NewChangePasswordHandler(repo, breach.Noop{})

	err := h.Handle(t.Context(), command.ChangePasswordCommand{
		PersonID:        repo.person.ID(),
		CurrentPassword: currentPlain,
		NewPassword:     "Tr0ub4dor&3-newly-strong-passphrase!",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if repo.updateCalls != 1 {
		t.Errorf("updateCalls = %d, want 1", repo.updateCalls)
	}
	// New hash must verify against new plaintext, NOT the old one.
	if vErr := argon2.Verify("Tr0ub4dor&3-newly-strong-passphrase!", repo.person.PasswordHash().String()); vErr != nil {
		t.Errorf("verify new password: %v", vErr)
	}
	if vErr := argon2.Verify(currentPlain, repo.person.PasswordHash().String()); vErr == nil {
		t.Error("verify OLD password against new hash unexpectedly succeeded")
	}
}

func TestChangePassword_RejectsIncorrectCurrentPassword(t *testing.T) {
	t.Parallel()
	freezeClock(t)
	repo := &fakePersonRepo{person: newPersonWithPassword(t, "real-current-pw")}
	h := command.NewChangePasswordHandler(repo, breach.Noop{})

	err := h.Handle(t.Context(), command.ChangePasswordCommand{
		PersonID:        repo.person.ID(),
		CurrentPassword: "wrong-current-pw",
		NewPassword:     "anything-stronger-than-floor",
	})
	if !errors.Is(err, command.ErrIncorrectCurrentPassword) {
		t.Fatalf("err = %v, want ErrIncorrectCurrentPassword", err)
	}
	if repo.updateCalls != 0 {
		t.Errorf("update should NOT happen on wrong current password; got %d calls", repo.updateCalls)
	}
}

func TestChangePassword_PersonNotFound_ReturnsIncorrectCurrentPassword(t *testing.T) {
	// Per security.md "Login flow — enumeration safety": collapse
	// "no such person" + "wrong password" into the same error to
	// defeat ID-enumeration via change-password timing/error shape.
	t.Parallel()
	freezeClock(t)
	repo := &fakePersonRepo{} // no Person seeded
	h := command.NewChangePasswordHandler(repo, breach.Noop{})

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
	freezeClock(t)
	currentPlain := "real-current-pw"
	repo := &fakePersonRepo{person: newPersonWithPassword(t, currentPlain)}
	// Use offline list — "password" is in defaultBreachedSet.
	h := command.NewChangePasswordHandler(repo, breach.NewOfflineList())

	err := h.Handle(t.Context(), command.ChangePasswordCommand{
		PersonID:        repo.person.ID(),
		CurrentPassword: currentPlain,
		NewPassword:     "password",
	})
	if !errors.Is(err, command.ErrPasswordBreached) {
		t.Fatalf("err = %v, want ErrPasswordBreached", err)
	}
	if repo.updateCalls != 0 {
		t.Errorf("breached password must not commit; got %d update calls", repo.updateCalls)
	}
}

func TestChangePassword_RejectsSameAsCurrent(t *testing.T) {
	t.Parallel()
	freezeClock(t)
	plain := "exact-same-passphrase"
	repo := &fakePersonRepo{person: newPersonWithPassword(t, plain)}
	h := command.NewChangePasswordHandler(repo, breach.Noop{})

	err := h.Handle(t.Context(), command.ChangePasswordCommand{
		PersonID:        repo.person.ID(),
		CurrentPassword: plain,
		NewPassword:     plain,
	})
	if !errors.Is(err, command.ErrPasswordSameAsCurrent) {
		t.Fatalf("err = %v, want ErrPasswordSameAsCurrent", err)
	}
	if repo.updateCalls != 0 {
		t.Errorf("same-as-current must not commit; got %d update calls", repo.updateCalls)
	}
}

func TestChangePassword_AnonymisedPerson_ReturnsIncorrectCurrentPassword(t *testing.T) {
	// Anonymised Persons have a scrubbed password hash + cannot
	// authenticate. Surface as "incorrect current password" rather
	// than "anonymised" to avoid leaking account state.
	t.Parallel()
	freezeClock(t)
	currentPlain := "irrelevant"
	p := newPersonWithPassword(t, currentPlain)
	if err := p.Anonymise(); err != nil {
		t.Fatalf("Anonymise: %v", err)
	}
	repo := &fakePersonRepo{person: p}
	h := command.NewChangePasswordHandler(repo, breach.Noop{})

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
	repo := &fakePersonRepo{}
	h := command.NewChangePasswordHandler(repo, breach.Noop{})

	err := h.Handle(t.Context(), command.ChangePasswordCommand{
		PersonID:        person.ID(""),
		CurrentPassword: "x",
		NewPassword:     "y",
	})
	if err == nil || !strings.Contains(err.Error(), "person id required") {
		t.Fatalf("err = %v, want 'person id required'", err)
	}
}

func TestChangePassword_RejectsEmptyNewPassword(t *testing.T) {
	t.Parallel()
	freezeClock(t)
	repo := &fakePersonRepo{person: newPersonWithPassword(t, "real-current-pw")}
	h := command.NewChangePasswordHandler(repo, breach.Noop{})

	err := h.Handle(t.Context(), command.ChangePasswordCommand{
		PersonID:        repo.person.ID(),
		CurrentPassword: "real-current-pw",
		NewPassword:     "",
	})
	if err == nil || !strings.Contains(err.Error(), "new password required") {
		t.Fatalf("err = %v, want 'new password required'", err)
	}
}

func TestNewChangePasswordHandler_PanicsOnNilDeps(t *testing.T) {
	t.Parallel()
	t.Run("nil persons", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic on nil persons repo")
			}
		}()
		_ = command.NewChangePasswordHandler(nil, breach.Noop{})
	})
	t.Run("nil breach checker", func(t *testing.T) {
		defer func() {
			if r := recover(); r == nil {
				t.Error("expected panic on nil breach checker")
			}
		}()
		_ = command.NewChangePasswordHandler(&fakePersonRepo{}, nil)
	})
}
