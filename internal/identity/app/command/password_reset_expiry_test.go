package command_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership/membershiptest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/passwordpolicy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken/refreshtokentest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant/tenanttest"
)

// resetRig is a person with a pending reset (requested at testNow) and a confirm
// handler whose clock the test chooses.
type resetRig struct {
	p     *person.Person
	repo  *resettableRepo
	raw   string
	stamp string
	hash  string
}

func newResetRig(t *testing.T, currentPassword string) resetRig {
	t.Helper()
	addr, _ := email.New("alice@example.test")
	p := newPersonWithPassword(t, currentPassword)
	repo := newResettableRepo(t, p)
	if err := command.NewRequestPasswordResetHandler(repo, func() time.Time { return testNow }).Handle(
		t.Context(), command.RequestPasswordResetCommand{Email: addr}); err != nil {
		t.Fatalf("request: %v", err)
	}
	return resetRig{p: p, repo: repo, raw: repo.emailRequestedToken(t), stamp: p.SecurityStamp().String(), hash: p.PasswordHash().String()}
}

func (r resetRig) confirmAt(at time.Time, checker passwordpolicy.Checker) command.ConfirmPasswordResetHandler {
	return command.NewConfirmPasswordResetHandler(r.repo, refreshtokentest.NewFakeRepository(), membershiptest.NewFakeRepository(),
		tenanttest.NewFakeRepository(), fakeUoW{}, checker, func() time.Time { return at })
}

// Fix round 13, item 2: an expired reset token is dead BEFORE any password work.
// Before the fix it earned the authenticated gate share, the policy answer and the
// same-as-current verify (a guessing oracle for the CURRENT password with no login
// lockout) up to the write that finally refused it.
func TestConfirmPasswordReset_ExpiredToken_IsInvalidWhateverTheBody_AndRunsNoPasswordWork(t *testing.T) {
	t.Parallel()
	const current = "Current-Pw-Of-The-Victim-9!"
	expired := testNow.Add(command.PasswordResetTokenTTL + time.Second)
	for name, body := range map[string]string{
		"policy violation":     "abc",
		"the current password": current,
		"a good password":      "Tr0ub4dor&3-newly-strong",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newResetRig(t, current)
			// A saturated gate refuses ANY hash or verify: reaching one would surface as
			// ErrBusy instead of the token answer.
			h := r.confirmAt(expired, passwordpolicy.Noop{}).WithArgonGate(saturatedGate(t))
			err := h.Handle(t.Context(), command.ConfirmPasswordResetCommand{RawToken: r.raw, NewPassword: body})
			if !errors.Is(err, command.ErrResetTokenInvalid) {
				t.Fatalf("expired token with %s: %v, want ErrResetTokenInvalid", name, err)
			}
			if !r.p.PendingPasswordReset().IsZero() {
				t.Fatal("the expired reset stayed pending (it must be cleared, persisted)")
			}
			if r.p.PasswordHash().String() != r.hash || r.p.SecurityStamp().String() != r.stamp {
				t.Fatal("an expired token changed the credential")
			}
		})
	}
}

// With a valid token the current password is accepted: the same-as-current check
// on RESET would be a silent oracle for the current password. (Change-password
// keeps the check: its caller has just proved the current password.)
func TestConfirmPasswordReset_CurrentPassword_Succeeds_NoOracle(t *testing.T) {
	t.Parallel()
	const current = "Current-Pw-Of-The-Victim-9!"
	r := newResetRig(t, current)
	if err := r.confirmAt(testNow.Add(time.Minute), passwordpolicy.Noop{}).Handle(t.Context(),
		command.ConfirmPasswordResetCommand{RawToken: r.raw, NewPassword: current}); err != nil {
		t.Fatalf("reset to the current password: %v, want success", err)
	}
	if r.p.SecurityStamp().String() == r.stamp {
		t.Fatal("a successful reset must rotate the stamp")
	}
}

// A valid token with a rejected password stays usable, but only for
// command.PasswordResetMaxAttempts rejections: the token cannot be probed forever.
func TestConfirmPasswordReset_RejectedAttempts_AreBounded(t *testing.T) {
	t.Parallel()
	good := "Tr0ub4dor&3-newly-strong"
	at := testNow.Add(time.Minute)

	t.Run("a policy violation leaves the token usable", func(t *testing.T) {
		t.Parallel()
		r := newResetRig(t, "Current-Pw-Of-The-Victim-9!")
		h := r.confirmAt(at, passwordpolicy.Noop{})
		for range command.PasswordResetMaxAttempts - 1 {
			err := h.Handle(t.Context(), command.ConfirmPasswordResetCommand{RawToken: r.raw, NewPassword: "abc"})
			if !errors.Is(err, tenant.ErrPasswordPolicy) {
				t.Fatalf("weak password: %v, want the policy error", err)
			}
		}
		if err := h.Handle(t.Context(), command.ConfirmPasswordResetCommand{RawToken: r.raw, NewPassword: good}); err != nil {
			t.Fatalf("a good password after %d rejections: %v, want success", command.PasswordResetMaxAttempts-1, err)
		}
	})

	t.Run("the limit invalidates the token", func(t *testing.T) {
		t.Parallel()
		r := newResetRig(t, "Current-Pw-Of-The-Victim-9!")
		h := r.confirmAt(at, passwordpolicy.Noop{})
		for i := range command.PasswordResetMaxAttempts {
			err := h.Handle(t.Context(), command.ConfirmPasswordResetCommand{RawToken: r.raw, NewPassword: "abc"})
			if !errors.Is(err, tenant.ErrPasswordPolicy) {
				t.Fatalf("rejection %d: %v, want the policy error", i+1, err)
			}
		}
		err := h.Handle(t.Context(), command.ConfirmPasswordResetCommand{RawToken: r.raw, NewPassword: good})
		if !errors.Is(err, command.ErrResetTokenInvalid) {
			t.Fatalf("after the limit: %v, want ErrResetTokenInvalid", err)
		}
		if !r.p.PendingPasswordReset().IsZero() || r.p.PasswordHash().String() != r.hash {
			t.Fatal("the token must be gone and the credential untouched")
		}
	})

	t.Run("a breached password counts too", func(t *testing.T) {
		t.Parallel()
		r := newResetRig(t, "Current-Pw-Of-The-Victim-9!")
		h := r.confirmAt(at, breachingChecker{})
		for range command.PasswordResetMaxAttempts {
			err := h.Handle(t.Context(), command.ConfirmPasswordResetCommand{RawToken: r.raw, NewPassword: good})
			if !errors.Is(err, command.ErrPasswordBreached) {
				t.Fatalf("breached: %v, want ErrPasswordBreached", err)
			}
		}
		if err := r.confirmAt(at, passwordpolicy.Noop{}).Handle(t.Context(),
			command.ConfirmPasswordResetCommand{RawToken: r.raw, NewPassword: good}); !errors.Is(err, command.ErrResetTokenInvalid) {
			t.Fatalf("after the limit: %v, want ErrResetTokenInvalid", err)
		}
	})
}

// A new request supersedes the old token and its attempt count.
func TestRequestPasswordReset_ANewRequestResetsTheAttemptCount(t *testing.T) {
	t.Parallel()
	r := newResetRig(t, "Current-Pw-Of-The-Victim-9!")
	h := r.confirmAt(testNow.Add(time.Minute), passwordpolicy.Noop{})
	for range command.PasswordResetMaxAttempts - 1 {
		if err := h.Handle(t.Context(), command.ConfirmPasswordResetCommand{RawToken: r.raw, NewPassword: "abc"}); !errors.Is(err, tenant.ErrPasswordPolicy) {
			t.Fatalf("weak password: %v, want the policy error", err)
		}
	}
	if got := r.p.PendingPasswordReset().FailedAttempts(); got != command.PasswordResetMaxAttempts-1 {
		t.Fatalf("the rejections were counted as %d, want %d", got, command.PasswordResetMaxAttempts-1)
	}
	addr, _ := email.New("alice@example.test")
	if err := command.NewRequestPasswordResetHandler(r.repo, func() time.Time { return testNow.Add(2 * time.Minute) }).Handle(
		t.Context(), command.RequestPasswordResetCommand{Email: addr}); err != nil {
		t.Fatal(err)
	}
	if got := r.p.PendingPasswordReset().FailedAttempts(); got != 0 {
		t.Fatalf("a fresh request carries %d failed attempts, want 0", got)
	}
}
