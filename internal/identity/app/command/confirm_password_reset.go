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

// PasswordResetMaxAttempts is how many times one valid reset token may be
// presented with a password that is then refused (policy or breach list) before
// the token is invalidated: the holder of a link cannot probe the rules forever.
const PasswordResetMaxAttempts = 5

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
//  4. Reject a person who may not sign in (anonymised, deactivated, globally
//     suspended: [person.Person.MaySignIn], the predicate every path asks).
//  5. Validate the token's own state FIRST, before any password work: an expired
//     token is cleared (persisted) and answers ErrResetTokenInvalid whatever the
//     body is. Only then is the work authenticated (a valid token proves receipt of
//     the emailed link), so only a holder of a live token reaches 6-7.
//  6. Check the new password against the password policy of the tenant of the
//     person's active membership (the default when there is none), size bound
//     first. A refusal counts toward [PasswordResetMaxAttempts] for this token.
//     There is NO same-as-current check: with a valid token it would be a silent
//     oracle for the current password (change-password keeps it; its caller just
//     proved the current password).
//  7. Run breach check on new password (a refusal counts like a policy refusal).
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

	if !p.MaySignIn() {
		return ErrResetTokenInvalid
	}
	now := h.now()
	pending := p.PendingPasswordReset()
	if pending.IsZero() || !pending.Hash().Equal(tokenHash) {
		return ErrResetTokenInvalid
	}
	if !now.UTC().Before(pending.ExpiresAt()) {
		// Dead before any password work. The clear is persisted so the row stops
		// matching; a failure to persist is an infrastructure error, not an answer.
		if err := h.persons.UpdateByID(ctx, p.ID(), func(loaded *person.Person) (bool, error) {
			return loaded.ExpirePasswordReset(tokenHash, now), nil
		}); err != nil {
			return fmt.Errorf("confirm_password_reset: clear expired token: %w", err)
		}
		return ErrResetTokenInvalid
	}
	// The token matched a live pending reset: from here on this is authenticated work
	// (the holder proved they received the emailed token), which keeps a share of the
	// gate that anonymous load cannot take. Everything before this point costs no
	// hashing.
	ctx = argon2.WithAuthenticated(ctx)

	// The policy, before any hashing. A person has at most one active membership
	// (409 person_active_elsewhere), so the tenant is unambiguous; with none, or
	// when the tenant row is gone, the default applies.
	if err := h.checkPolicy(ctx, p.ID(), cmd.NewPassword); err != nil {
		return h.refuse(ctx, p.ID(), tokenHash, now, err)
	}

	breached, err := h.breachChecker.IsBreached(ctx, cmd.NewPassword)
	if err != nil {
		return fmt.Errorf("confirm_password_reset: breach check: %w", err)
	}
	if breached {
		return h.refuse(ctx, p.ID(), tokenHash, now, ErrPasswordBreached)
	}

	newHashStr, err := h.gate.Hash(ctx, cmd.NewPassword)
	if err != nil {
		return fmt.Errorf("confirm_password_reset: hash new: %w", err)
	}
	newHash, err := person.NewPasswordHash(newHashStr)
	if err != nil {
		return fmt.Errorf("confirm_password_reset: wrap new hash: %w", err)
	}

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

// refuse counts one refused password against the token, then answers cause. The
// count is persisted before the answer (a small transaction on the person row
// alone, so it adds no lock-order edge). The attempt that reaches
// [PasswordResetMaxAttempts] clears the token; a token that vanished meanwhile (a
// newer request, a concurrent confirm) answers "invalid" instead of cause.
func (h ConfirmPasswordResetHandler) refuse(ctx context.Context, id person.ID, tokenHash person.PasswordResetTokenHash, now time.Time, cause error) error {
	if err := h.persons.UpdateByID(ctx, id, func(loaded *person.Person) (bool, error) {
		if err := loaded.RecordRejectedPasswordResetAttempt(tokenHash, PasswordResetMaxAttempts, now); err != nil {
			return false, err
		}
		return true, nil
	}); err != nil {
		if errors.Is(err, person.ErrInvalid) {
			return ErrResetTokenInvalid
		}
		return fmt.Errorf("confirm_password_reset: count refused attempt: %w", err)
	}
	return cause
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
		return tn.Settings().EffectivePasswordPolicy(), nil
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
