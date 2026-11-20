//go:build integration

// arch-test:no-timeout-needed — every request carries t.Context().
//
// arch-test:raw-sql-justified — the stored state of a pending reset (the token
//   hash and the refused-attempt counter) has no HTTP surface; the test reads it to
//   prove the expired token was cleared and the counter persisted.

// Fix round 13, item 2: through the real HTTP stack an expired reset token is dead
// before any password work, whatever the body; a valid token survives refused
// passwords only up to the attempt bound; resetting to the current password is
// accepted (no oracle).
package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/ports"
)

func (f e2eFixture) pendingReset(t *testing.T, personID string) (hasToken bool, attempts int) {
	t.Helper()
	if err := f.Pool.QueryRow(t.Context(),
		`SELECT password_reset_token_hash IS NOT NULL, password_reset_failed_attempts FROM identity.persons WHERE id = $1`, personID).Scan(&hasToken, &attempts); err != nil {
		t.Fatalf("read pending reset: %v", err)
	}
	return hasToken, attempts
}

func TestE2E_PasswordPolicy_ExpiredResetTokenIsDeadWhateverTheBody(t *testing.T) {
	clock := newGraceClock()
	f := newE2EFixtureAt(t, clock.now)
	owner := f.registerAndLogin(t, "pwexpired")
	token := f.seedResetToken(t, owner.PersonID)
	clock.advance(command.PasswordResetTokenTTL + time.Minute)

	// A weak password, the CURRENT password (the old oracle) and a good one: all the
	// same 400 reset_token_invalid, never the 422 policy answer.
	for name, pw := range map[string]string{"weak": weakPw, "current": owner.Password, "good": "Reset-Pass-2026!late"} {
		r := f.postJSON(t, "/api/v1/auth/reset-password", ports.ResetPasswordRequest{Token: token, NewPassword: pw})
		if r.status != http.StatusBadRequest || decodeError(t, r.body).Error != ports.ErrCodeResetTokenInvalid {
			t.Fatalf("expired token, %s password: %d %s, want 400 reset_token_invalid", name, r.status, r.body)
		}
	}
	if has, _ := f.pendingReset(t, owner.PersonID); has {
		t.Fatal("the expired reset must be cleared from the row")
	}
	// The credential is untouched: the original password still signs in.
	f.loginPair(t, owner.Email, owner.Password)
}

func TestE2E_PasswordPolicy_ResetTokenAttemptsAreBounded_AndTheCurrentPasswordIsAccepted(t *testing.T) {
	f := newE2EFixture(t)
	owner := f.registerAndLogin(t, "pwattempts")
	token := f.seedResetToken(t, owner.PersonID)

	for i := 1; i <= command.PasswordResetMaxAttempts; i++ {
		f.requirePolicyViolation(t, f.postJSON(t, "/api/v1/auth/reset-password",
			ports.ResetPasswordRequest{Token: token, NewPassword: weakPw}), "new_password", weakRules)
		if has, n := f.pendingReset(t, owner.PersonID); i < command.PasswordResetMaxAttempts && (!has || n != i) {
			t.Fatalf("after refusal %d: token pending=%v attempts=%d", i, has, n)
		}
	}
	// The limit invalidated the token: even a compliant password is now refused.
	r := f.postJSON(t, "/api/v1/auth/reset-password", ports.ResetPasswordRequest{Token: token, NewPassword: "Reset-Pass-2026!limit"})
	if r.status != http.StatusBadRequest || decodeError(t, r.body).Error != ports.ErrCodeResetTokenInvalid {
		t.Fatalf("after the attempt limit: %d %s, want 400 reset_token_invalid", r.status, r.body)
	}
	if has, _ := f.pendingReset(t, owner.PersonID); has {
		t.Fatal("the token must be gone after the limit")
	}

	// A fresh token, then the CURRENT password: accepted (no same-as-current oracle).
	fresh := f.seedResetToken(t, owner.PersonID)
	if r := f.postJSON(t, "/api/v1/auth/reset-password", ports.ResetPasswordRequest{Token: fresh, NewPassword: owner.Password}); r.status != http.StatusNoContent {
		t.Fatalf("reset to the current password: %d %s, want 204", r.status, r.body)
	}
}
