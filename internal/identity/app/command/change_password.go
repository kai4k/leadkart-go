package command

import (
	"context"
	"errors"
	"fmt"

	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/passwordpolicy"
)

// ChangePasswordCommand carries the plaintext credentials. Per
// security.md "Password change" + Auth0 / Okta canon: every password-
// touching flow MUST verify the current password before applying a
// new one — even when authenticated, otherwise an attacker with a
// stolen access token could permanently take over an account.
//
// PersonID comes from the verified JWT (set by RequireAuth middleware
// + tenancy.WithID bridge per A.2.3); it's NOT supplied in the request
// body. The Application command layer trusts the boundary check.
type ChangePasswordCommand struct {
	PersonID        person.ID
	CurrentPassword string
	NewPassword     string
}

// ----- Handler errors --------------------------------------------------------

// ErrIncorrectCurrentPassword surfaces when the supplied current
// password fails Argon2id verification. Same error code as login's
// invalid_credentials; the HTTP layer maps to 401 to match the
// security.md "Login flow" enumeration-resistance posture.
var ErrIncorrectCurrentPassword = errors.New("change_password: current password incorrect")

// ErrPasswordBreached surfaces when [passwordpolicy.Checker.IsBreached] reports
// the new password has appeared in known breaches. Per security.md
// "HIBP+Argon2id+JWT" — every new password is checked.
var ErrPasswordBreached = errors.New("change_password: new password has appeared in known breaches")

// ErrPasswordSameAsCurrent surfaces when the user supplies a new
// password identical to the current one. Catches accidental no-ops
// + reduces the audit-log noise. (Strictly optional — Auth0 + Okta
// reject; some products allow.) Per LeadKart parent canon: reject.
var ErrPasswordSameAsCurrent = errors.New("change_password: new password same as current")

// ----- Handler ---------------------------------------------------------------

// ChangePasswordHandler applies a new password to a Person via the
// authenticated change-password flow per security.md "Password change":
//
//  1. Load person by ID (from JWT).
//  2. Reject anonymised / globally-suspended (mirror login gates).
//  3. Verify current password against stored hash.
//  4. Reject same-as-current (no-op + audit-noise reduction).
//  5. Run breach check on new password.
//  6. Hash new password with Argon2id.
//  7. UpdateByID closure: Person.ChangePassword(newHash) — rotates
//     SecurityStamp + emits PasswordChangedEvent. Outbox subscriber
//     revokes every refresh-token family for this Person across
//     tenants (logout-all-sessions choreography).
type ChangePasswordHandler struct {
	persons        person.Repository
	breachChecker  passwordpolicy.Checker
}

// NewChangePasswordHandler wires the handler. breachChecker MUST be
// non-nil — a nil checker silently weakens security per security.md.
func NewChangePasswordHandler(persons person.Repository, breachChecker passwordpolicy.Checker) ChangePasswordHandler {
	if persons == nil {
		panic("command: NewChangePasswordHandler persons repository required")
	}
	if breachChecker == nil {
		panic("command: NewChangePasswordHandler breach checker required (use passwordpolicy.Noop only in tests)")
	}
	return ChangePasswordHandler{
		persons:       persons,
		breachChecker: breachChecker,
	}
}

// Handle executes the change-password flow.
func (h ChangePasswordHandler) Handle(ctx context.Context, cmd ChangePasswordCommand) error {
	if cmd.PersonID.IsZero() {
		return errors.New("change_password: person id required")
	}
	if cmd.CurrentPassword == "" {
		return ErrIncorrectCurrentPassword
	}
	if cmd.NewPassword == "" {
		return errors.New("change_password: new password required")
	}

	// 1. Load person — read path so we can verify the current
	// password BEFORE entering the UpdateByID transaction. Failing
	// inside the closure would still work but eats a tx for the
	// happy-path-rejection case.
	p, err := h.persons.GetByID(ctx, cmd.PersonID)
	if err != nil {
		if errors.Is(err, person.ErrNotFound) {
			// Surface as ErrIncorrectCurrentPassword — same as if
			// the password verify failed. Defeats user-id
			// enumeration via timing/error-shape probing.
			return ErrIncorrectCurrentPassword
		}
		return fmt.Errorf("change_password: load person: %w", err)
	}

	// 2. Gates that mirror login — anonymised / globally-suspended
	// Person can't change password.
	if p.IsAnonymised() {
		return ErrIncorrectCurrentPassword
	}
	if p.IsGloballySuspended() {
		return ErrIncorrectCurrentPassword
	}

	// 3. Verify current password.
	if err := argon2.Verify(cmd.CurrentPassword, p.PasswordHash().String()); err != nil {
		if errors.Is(err, argon2.ErrMismatch) || errors.Is(err, argon2.ErrFormat) {
			return ErrIncorrectCurrentPassword
		}
		return fmt.Errorf("change_password: verify current: %w", err)
	}

	// 4. Reject same-as-current (defeats accidental no-op + audit noise).
	if cmd.CurrentPassword == cmd.NewPassword {
		return ErrPasswordSameAsCurrent
	}

	// 5. Breach check.
	breached, err := h.breachChecker.IsBreached(ctx, cmd.NewPassword)
	if err != nil {
		return fmt.Errorf("change_password: breach check: %w", err)
	}
	if breached {
		return ErrPasswordBreached
	}

	// 6. Hash.
	newHashStr, err := argon2.Hash(cmd.NewPassword)
	if err != nil {
		return fmt.Errorf("change_password: hash new: %w", err)
	}
	newHash, err := person.NewPasswordHash(newHashStr)
	if err != nil {
		return fmt.Errorf("change_password: wrap new hash: %w", err)
	}

	// 7. UpdateByID — aggregate handles the SecurityStamp rotation +
	// event recording. Repository drains events into outbox in the
	// same tx.
	err = h.persons.UpdateByID(ctx, cmd.PersonID, func(p *person.Person) (bool, error) {
		if err := p.ChangePassword(newHash); err != nil {
			return false, err
		}
		// Pending password-reset (if any) is invalidated by the direct
		// change — no point letting an emailed token unlock a password
		// the user just rotated. Aggregate's CancelPasswordReset is
		// idempotent (no-op when no pending), so this is safe to
		// always call.
		if err := p.CancelPasswordReset("password-changed-directly"); err != nil {
			return false, err
		}
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("change_password: persist: %w", err)
	}
	return nil
}
