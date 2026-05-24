package command

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/domain/passwordpolicy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

// ConfirmPasswordResetCommand carries the user-presented plaintext
// reset token + the chosen new password.
//
// The plaintext token NEVER hits Postgres; the handler hashes it
// (SHA-256 hex) + looks the Person up via the partial UNIQUE index
// per migration 20260507000004. Hash-only round-trip = a leaked DB
// row cannot be replayed against the confirm endpoint without the
// out-of-band token from the email.
type ConfirmPasswordResetCommand struct {
	RawToken    string
	NewPassword string
}

// ErrResetTokenInvalid surfaces for ANY confirm-flow rejection: token
// mismatch, expiry, no pending reset, terminal Person state, etc. Per
// security.md "Password reset" + Auth0/Okta canon: the response shape
// MUST NOT distinguish causes — that lets an attacker probe the
// pending-reset state of arbitrary tokens via timing/error-shape.
var ErrResetTokenInvalid = errors.New("confirm_password_reset: token invalid or expired")

// ConfirmPasswordResetHandler runs the confirm-side of the reset flow.
type ConfirmPasswordResetHandler struct {
	persons       person.Repository
	breachChecker passwordpolicy.Checker
	now           func() time.Time
}

// NewConfirmPasswordResetHandler wires the handler. `now` is the
// explicit time source per the clock-injection refactor. Nil → time.Now.
func NewConfirmPasswordResetHandler(persons person.Repository, breachChecker passwordpolicy.Checker, now func() time.Time) ConfirmPasswordResetHandler {
	if persons == nil {
		panic("command: NewConfirmPasswordResetHandler persons repository required")
	}
	if breachChecker == nil {
		panic("command: NewConfirmPasswordResetHandler breach checker required")
	}
	if now == nil {
		now = time.Now
	}
	return ConfirmPasswordResetHandler{
		persons:       persons,
		breachChecker: breachChecker,
		now:           now,
	}
}

// Handle runs the confirm flow.
//
//  1. Validate inputs (raw token + new password non-empty).
//  2. Compute hash of presented plaintext.
//  3. Lookup Person by hash. Not found → ErrResetTokenInvalid (NEVER
//     leak which arm matched).
//  4. Reject anonymised / globally-suspended (mirror request-side).
//  5. Reject same-as-current (defeats no-op).
//  6. Run breach check on new password.
//  7. Hash new password.
//  8. UpdateByID closure: Person.ConfirmPasswordReset(presentedHash,
//     newHash). Aggregate enforces token mismatch + expiry checks +
//     rotates SecurityStamp + clears pending reset.
func (h ConfirmPasswordResetHandler) Handle(ctx context.Context, cmd ConfirmPasswordResetCommand) error {
	if cmd.RawToken == "" {
		return ErrResetTokenInvalid
	}
	if cmd.NewPassword == "" {
		return ErrNewPasswordRequired
	}

	hashHex := hashResetToken(cmd.RawToken)
	tokenHash, err := person.NewPasswordResetTokenHash(hashHex)
	if err != nil {
		// Hash format-rejection — should never happen because we
		// produced the hex ourselves. Treat as token invalid (not a
		// 5xx) so callers see the same shape regardless of arm.
		return ErrResetTokenInvalid
	}

	p, err := h.persons.GetByPasswordResetTokenHash(ctx, tokenHash)
	switch {
	case errors.Is(err, person.ErrNotFound):
		return ErrResetTokenInvalid
	case err != nil:
		return fmt.Errorf("confirm_password_reset: lookup: %w", err)
	}

	if p.IsAnonymised() || p.IsGloballySuspended() {
		return ErrResetTokenInvalid
	}

	// Reject same-as-current. The aggregate doesn't enforce this in
	// ConfirmPasswordReset — applying the rule here keeps the audit
	// log free of "user reset to the same password" no-ops.
	if argon2.Verify(cmd.NewPassword, p.PasswordHash().String()) == nil {
		return ErrPasswordSameAsCurrent
	}

	breached, err := h.breachChecker.IsBreached(ctx, cmd.NewPassword)
	if err != nil {
		return fmt.Errorf("confirm_password_reset: breach check: %w", err)
	}
	if breached {
		return ErrPasswordBreached
	}

	newHashStr, err := argon2.Hash(cmd.NewPassword)
	if err != nil {
		return fmt.Errorf("confirm_password_reset: hash new: %w", err)
	}
	newHash, err := person.NewPasswordHash(newHashStr)
	if err != nil {
		return fmt.Errorf("confirm_password_reset: wrap new hash: %w", err)
	}

	now := h.now()
	if err := h.persons.UpdateByID(ctx, p.ID(), func(loaded *person.Person) (bool, error) {
		if err := loaded.ConfirmPasswordReset(tokenHash, newHash, now); err != nil {
			return false, err
		}
		return true, nil
	}); err != nil {
		// Domain-layer rejections (token mismatch, expired, no pending
		// reset) collapse to ErrResetTokenInvalid per the same
		// enumeration-safety rule.
		if errors.Is(err, person.ErrInvalid) {
			return ErrResetTokenInvalid
		}
		return fmt.Errorf("confirm_password_reset: persist: %w", err)
	}
	return nil
}

func hashResetToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}
