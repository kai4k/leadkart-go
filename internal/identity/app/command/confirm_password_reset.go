package command

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/domain/membership"
	"github.com/leadkart/leadkart-go/internal/identity/domain/passwordpolicy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
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
	families      refreshtoken.Repository
	memberships   membership.Repository
	tenants       tenant.Repository
	uow           pg.UnitOfWork
	breachChecker passwordpolicy.Checker
	now           func() time.Time
	gate          *argon2.Gate
}

// NewConfirmPasswordResetHandler wires the handler. `now` is the
// explicit time source per the clock-injection refactor. Nil → time.Now.
func NewConfirmPasswordResetHandler(persons person.Repository, families refreshtoken.Repository, memberships membership.Repository, tenants tenant.Repository, uow pg.UnitOfWork, breachChecker passwordpolicy.Checker, now func() time.Time) ConfirmPasswordResetHandler {
	if persons == nil {
		panic("command: NewConfirmPasswordResetHandler persons repository required")
	}
	if families == nil {
		panic("command: NewConfirmPasswordResetHandler families repository required")
	}
	if memberships == nil {
		panic("command: NewConfirmPasswordResetHandler memberships repository required")
	}
	if tenants == nil {
		panic("command: NewConfirmPasswordResetHandler tenants repository required")
	}
	if uow == nil {
		panic("command: NewConfirmPasswordResetHandler unit of work required")
	}
	if breachChecker == nil {
		panic("command: NewConfirmPasswordResetHandler breach checker required")
	}
	if now == nil {
		now = time.Now
	}
	return ConfirmPasswordResetHandler{
		persons:       persons,
		families:      families,
		memberships:   memberships,
		tenants:       tenants,
		uow:           uow,
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
//  5. Check the new password against the password policy of the tenant of the
//     person's active membership (the default when there is none), size bound
//     first. Only a holder of a valid token reaches this step, so the answer
//     discloses nothing the token does not already prove.
//  6. Reject same-as-current (defeats no-op).
//  7. Run breach check on new password.
//  8. Hash new password.
//  9. UpdateByID closure: Person.ConfirmPasswordReset(presentedHash,
//     newHash). Aggregate enforces token mismatch + expiry checks +
//     rotates SecurityStamp + clears pending reset. In the SAME unit of
//     work every refresh family of the Person is revoked (reason
//     "password_reset"): whoever held the old credential is signed out
//     before the new password can be used.
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

	// The policy, before any hashing. A person has at most one active membership
	// (409 person_active_elsewhere), so the tenant is unambiguous; with none, or
	// when the tenant row is gone, the default applies.
	if err := h.checkPolicy(ctx, p.ID(), cmd.NewPassword); err != nil {
		return err
	}

	// Reject same-as-current. The aggregate doesn't enforce this in
	// ConfirmPasswordReset — applying the rule here keeps the audit
	// log free of "user reset to the same password" no-ops.
	switch err := h.gate.Verify(ctx, cmd.NewPassword, p.PasswordHash().String()); {
	case err == nil:
		return ErrPasswordSameAsCurrent
	case errors.Is(err, argon2.ErrBusy) || ctx.Err() != nil:
		return fmt.Errorf("confirm_password_reset: verify current: %w", err)
	}

	breached, err := h.breachChecker.IsBreached(ctx, cmd.NewPassword)
	if err != nil {
		return fmt.Errorf("confirm_password_reset: breach check: %w", err)
	}
	if breached {
		return ErrPasswordBreached
	}

	newHashStr, err := h.gate.Hash(ctx, cmd.NewPassword)
	if err != nil {
		return fmt.Errorf("confirm_password_reset: hash new: %w", err)
	}
	newHash, err := person.NewPasswordHash(newHashStr)
	if err != nil {
		return fmt.Errorf("confirm_password_reset: wrap new hash: %w", err)
	}

	now := h.now()
	if err := h.uow.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context) error {
		if err := h.persons.UpdateByID(ctx, p.ID(), func(loaded *person.Person) (bool, error) {
			if err := loaded.ConfirmPasswordReset(tokenHash, newHash, now); err != nil {
				return false, err
			}
			return true, nil
		}); err != nil {
			return err
		}
		return revokeAllFamilies(ctx, h.families, p.ID(), "password_reset", now)
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

// policyFor returns the password policy of the tenant of the person's active
// membership, or the default when the person has none (every membership ended)
// or the tenant row is gone. Any other read failure is an error, never a silent
// fallback to a weaker rule.
func (h ConfirmPasswordResetHandler) policyFor(ctx context.Context, id person.ID) (tenant.PasswordPolicy, error) {
	m, err := h.memberships.GetActiveForPerson(ctx, id)
	switch {
	case errors.Is(err, membership.ErrNotFound):
		return tenant.DefaultPasswordPolicy(), nil
	case err != nil:
		return tenant.PasswordPolicy{}, fmt.Errorf("confirm_password_reset: load active membership: %w", err)
	}
	tn, err := h.tenants.GetByID(ctx, m.TenantID())
	switch {
	case err == nil:
		return tn.Settings().PasswordPolicy(), nil
	case errors.Is(err, tenant.ErrNotFound):
		return tenant.DefaultPasswordPolicy(), nil
	default:
		return tenant.PasswordPolicy{}, fmt.Errorf("confirm_password_reset: load tenant: %w", err)
	}
}

func hashResetToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// WithArgonGate returns the handler with its password hashing and verification running under
// g (see [argon2.Gate]); a saturated gate refuses with [argon2.ErrBusy], which the
// HTTP layer answers 503. Without it the work is unbounded (tests only).
func (h ConfirmPasswordResetHandler) WithArgonGate(g *argon2.Gate) ConfirmPasswordResetHandler {
	h.gate = g
	return h
}

// checkPolicy checks plaintext against the policy [ConfirmPasswordResetHandler.policyFor] resolves.
func (h ConfirmPasswordResetHandler) checkPolicy(ctx context.Context, id person.ID, plaintext string) error {
	policy, err := h.policyFor(ctx, id)
	if err != nil {
		return err
	}
	return policy.Check(plaintext)
}
