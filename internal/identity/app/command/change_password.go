package command

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/pg"
	"github.com/leadkart/leadkart-go/internal/identity/app/argon2"
	"github.com/leadkart/leadkart-go/internal/identity/domain/passwordpolicy"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/identity/domain/refreshtoken"
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

// ErrNewPasswordRequired surfaces when a password-mutating command
// (change-password or confirm-password-reset) is dispatched without
// a NewPassword. HTTP layer maps to 400 / invalid_password. Typed so
// callers can errors.Is on it instead of string-matching on a wrapped
// fmt.Errorf — Russ Cox "Working with Errors in Go 1.13" canon.
var ErrNewPasswordRequired = errors.New("change_password: new password required")

// ErrPersonIDRequired surfaces when a Person-targeted command is
// dispatched with a zero PersonID. Hitting this is a wiring bug
// (RequireAuth + Subject guard should have short-circuited at the
// HTTP boundary); HTTP layer still maps to 400 / invalid_password
// to avoid leaking server-state. Typed sentinel for errors.Is.
var ErrPersonIDRequired = errors.New("change_password: person id required")

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
//  7. In ONE unit of work: UpdateByID closure Person.ChangePassword(newHash)
//     — rotates SecurityStamp + emits PasswordChangedEvent — and then every
//     refresh-token family of the Person is revoked (reason "password_changed").
//     The access token carries no family id, so the caller's own session is
//     ended too: the BFF signs in again with the new password. The outbox
//     subscriber that reacts to the event stays as an idempotent backstop.
type ChangePasswordHandler struct {
	persons       person.Repository
	families      refreshtoken.Repository
	uow           pg.UnitOfWork
	breachChecker passwordpolicy.Checker
	now           func() time.Time
	gate          *argon2.Gate
	attempts      AttemptLimiter
}

// NewChangePasswordHandler wires the handler. breachChecker MUST be
// non-nil — a nil checker silently weakens security per security.md.
// `now` is the explicit time source per the clock-injection refactor.
// Nil → time.Now.
func NewChangePasswordHandler(persons person.Repository, families refreshtoken.Repository, uow pg.UnitOfWork, breachChecker passwordpolicy.Checker, now func() time.Time) ChangePasswordHandler {
	if persons == nil {
		panic("command: NewChangePasswordHandler persons repository required")
	}
	if families == nil {
		panic("command: NewChangePasswordHandler families repository required")
	}
	if uow == nil {
		panic("command: NewChangePasswordHandler unit of work required")
	}
	if breachChecker == nil {
		panic("command: NewChangePasswordHandler breach checker required (use passwordpolicy.Noop only in tests)")
	}
	if now == nil {
		now = time.Now
	}
	return ChangePasswordHandler{
		persons:       persons,
		families:      families,
		uow:           uow,
		breachChecker: breachChecker,
		now:           now,
	}
}

// Handle executes the change-password flow.
func (h ChangePasswordHandler) Handle(ctx context.Context, cmd ChangePasswordCommand) error {
	if cmd.PersonID.IsZero() {
		return ErrPersonIDRequired
	}
	if cmd.CurrentPassword == "" {
		return ErrIncorrectCurrentPassword
	}
	if cmd.NewPassword == "" {
		return ErrNewPasswordRequired
	}

	// 0. Per-person attempt budget. The current password is checked here on a
	// route any signed-in session can reach, so a hijacked session could guess it
	// at the rate the global limiter allows (and burn an Argon2id verify per
	// guess). The check runs before any password work.
	if err := h.spendAttempt(cmd.PersonID); err != nil {
		return err
	}

	// 1-2. Load the person (a read, so the current password is verified BEFORE
	// the UpdateByID transaction) and refuse the states that mirror login.
	p, err := h.loadChangeable(ctx, cmd.PersonID)
	if err != nil {
		return err
	}

	// 3. Verify current password.
	if err := h.gate.Verify(ctx, cmd.CurrentPassword, p.PasswordHash().String()); err != nil {
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
	newHashStr, err := h.gate.Hash(ctx, cmd.NewPassword)
	if err != nil {
		return fmt.Errorf("change_password: hash new: %w", err)
	}
	newHash, err := person.NewPasswordHash(newHashStr)
	if err != nil {
		return fmt.Errorf("change_password: wrap new hash: %w", err)
	}

	// 7. One unit of work: the password write + stamp rotation (aggregate +
	// outbox event) and the revocation of every refresh family. A crash or a
	// failure between the two rolls both back; the old password stays valid
	// together with the old sessions, never the new password with the old sessions.
	now := h.now()
	err = h.uow.WithinTx(ctx, pg.TxScopePlatform, func(ctx context.Context) error {
		if err := h.persons.UpdateByID(ctx, cmd.PersonID, func(p *person.Person) (bool, error) {
			if err := p.ChangePassword(newHash, now); err != nil {
				return false, err
			}
			// Pending password-reset (if any) is invalidated by the direct
			// change — no point letting an emailed token unlock a password
			// the user just rotated. Aggregate's CancelPasswordReset is
			// idempotent (no-op when no pending), so this is safe to
			// always call.
			if err := p.CancelPasswordReset("password-changed-directly", now); err != nil {
				return false, err
			}
			return true, nil
		}); err != nil {
			return err
		}
		return revokeAllFamilies(ctx, h.families, cmd.PersonID, "password_changed", now)
	})
	if err != nil {
		return fmt.Errorf("change_password: persist: %w", err)
	}
	return nil
}

// WithArgonGate returns the handler with its password hashing and verification running under
// g (see [argon2.Gate]); a saturated gate refuses with [argon2.ErrBusy], which the
// HTTP layer answers 503. Without it the work is unbounded (tests only).
func (h ChangePasswordHandler) WithArgonGate(g *argon2.Gate) ChangePasswordHandler {
	h.gate = g
	return h
}

// AttemptLimiter is the per-key attempt budget the handler consults: Allow
// spends one attempt for key and, when the budget is exhausted, says how long
// until the next one. Consumer-defined; httpmw.KeyedLimiter implements it.
type AttemptLimiter interface {
	Allow(key string) (ok bool, retryAfter time.Duration)
}

// TooManyAttemptsError is returned when the person's attempt budget is spent.
// The HTTP layer answers 429 with Retry-After (rounded up to whole seconds).
type TooManyAttemptsError struct {
	RetryAfter time.Duration
}

func (e *TooManyAttemptsError) Error() string { return "change_password: too many attempts" }

// WithAttemptLimiter returns the handler checking the current password at most
// as often as l allows, per person (ADR 0068 D12). Nil = unlimited (tests only).
func (h ChangePasswordHandler) WithAttemptLimiter(l AttemptLimiter) ChangePasswordHandler {
	h.attempts = l
	return h
}

// spendAttempt takes one attempt from the person's budget, or refuses with
// [TooManyAttemptsError] carrying the wait.
func (h ChangePasswordHandler) spendAttempt(id person.ID) error {
	if h.attempts == nil {
		return nil
	}
	if ok, wait := h.attempts.Allow(id.String()); !ok {
		return &TooManyAttemptsError{RetryAfter: wait}
	}
	return nil
}

// loadChangeable loads the person and answers every refusal the way a wrong
// current password is answered, so ids and account states cannot be enumerated:
// no such person, anonymised, globally suspended.
func (h ChangePasswordHandler) loadChangeable(ctx context.Context, id person.ID) (*person.Person, error) {
	p, err := h.persons.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, person.ErrNotFound) {
			return nil, ErrIncorrectCurrentPassword
		}
		return nil, fmt.Errorf("change_password: load person: %w", err)
	}
	if p.IsAnonymised() || p.IsGloballySuspended() {
		return nil, ErrIncorrectCurrentPassword
	}
	return p, nil
}
