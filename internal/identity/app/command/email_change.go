package command

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

// ----- RequestEmailChange ---------------------------------------------------

// RequestEmailChangeCommand initiates the email-change flow.
//
// PersonID arrives from the verified JWT Subject claim — never from
// the body. NewEmail is supplied by the caller. Per Auth0/Okta canon,
// the confirmation email is sent to the NEW address, not the current
// one — proves the user controls the destination before the change
// commits.
type RequestEmailChangeCommand struct {
	PersonID person.ID
	NewEmail email.Address
}

// EmailChangeTokenTTL — same 1h floor as password reset.
const EmailChangeTokenTTL = 1 * time.Hour

// ErrEmailChangeRejected surfaces for ANY request-side rejection:
// new-email collides with another Person, terminal Person state,
// already-pending change, etc. Confirms generic 4xx without leaking
// which arm tripped.
var ErrEmailChangeRejected = errors.New("request_email_change: rejected")

// ErrEmailAlreadyTaken — narrow surface for the "another user already
// owns this email" case. HTTP layer maps to 409. Distinct from the
// generic Rejected error so the UI can show a targeted message.
var ErrEmailAlreadyTaken = errors.New("request_email_change: email already in use")

// RequestEmailChangeHandler runs the request-side of the flow.
//
// Per ADR 0057: SYNCHRONOUS email-gateway dependency has moved out —
// the aggregate records BOTH the audit event AND the email-dispatch
// event; a Watermill subscriber delivers the confirmation link.
type RequestEmailChangeHandler struct {
	persons person.Repository
	now     func() time.Time
}

// NewRequestEmailChangeHandler wires the handler. `now` is the explicit
// time source per the clock-injection refactor. Nil → time.Now.
func NewRequestEmailChangeHandler(persons person.Repository, now func() time.Time) RequestEmailChangeHandler {
	if persons == nil {
		panic("command: NewRequestEmailChangeHandler persons repository required")
	}
	if now == nil {
		now = time.Now
	}
	return RequestEmailChangeHandler{persons: persons, now: now}
}

// Handle runs the request flow per ADR 0057:
//
//  1. Load Person by ID (caller's authenticated identity).
//  2. Reject anonymised / globally-suspended.
//  3. Reject if NewEmail equals current email (no-op).
//  4. Reject if NewEmail already belongs to another Person (409).
//  5. Mint ⟨plaintext, hash⟩ pair via crypto/rand + SHA-256.
//  6. UpdateByID: Person.RequestEmailChange(newEmail, plaintext, hash, ttl).
//     The aggregate records the AUDIT event AND the EMAIL-DISPATCH
//     event (plaintext) — outbox subscriber delivers.
func (h RequestEmailChangeHandler) Handle(ctx context.Context, cmd RequestEmailChangeCommand) error {
	if cmd.PersonID.IsZero() {
		return errors.New("request_email_change: person id required")
	}
	if cmd.NewEmail.IsZero() {
		return errors.New("request_email_change: new email required")
	}

	p, err := h.persons.GetByID(ctx, cmd.PersonID)
	switch {
	case errors.Is(err, person.ErrNotFound):
		return ErrEmailChangeRejected
	case err != nil:
		return fmt.Errorf("request_email_change: lookup: %w", err)
	}

	if p.IsAnonymised() || p.IsGloballySuspended() {
		return ErrEmailChangeRejected
	}
	if p.Email().String() == cmd.NewEmail.String() {
		return ErrEmailChangeRejected
	}

	// Globally-unique email check. Even though the aggregate's
	// ConfirmEmailChange path is what eventually attempts to write
	// the new email (and would surface ErrEmailTaken at that point),
	// catching it here means the user finds out BEFORE the link is
	// emailed — better UX, less wasted mail-provider quota.
	if other, err := h.persons.GetByEmail(ctx, cmd.NewEmail); err == nil && other != nil {
		return ErrEmailAlreadyTaken
	} else if err != nil && !errors.Is(err, person.ErrNotFound) {
		return fmt.Errorf("request_email_change: collision check: %w", err)
	}

	plaintext, hashHex, err := mintResetToken() // same crypto/rand + SHA-256 shape
	if err != nil {
		return fmt.Errorf("request_email_change: mint: %w", err)
	}
	tokenHash, err := person.NewEmailChangeTokenHash(hashHex)
	if err != nil {
		return fmt.Errorf("request_email_change: wrap hash: %w", err)
	}

	now := h.now()
	if err := h.persons.UpdateByID(ctx, cmd.PersonID, func(loaded *person.Person) (bool, error) {
		if err := loaded.RequestEmailChange(cmd.NewEmail, plaintext, tokenHash, EmailChangeTokenTTL, now); err != nil {
			return false, err
		}
		return true, nil
	}); err != nil {
		return fmt.Errorf("request_email_change: persist: %w", err)
	}
	return nil
}

// ----- ConfirmEmailChange ---------------------------------------------------

// ConfirmEmailChangeCommand carries the user-presented plaintext token.
type ConfirmEmailChangeCommand struct {
	RawToken string
}

// ErrEmailChangeTokenInvalid is the generic confirm-side rejection.
var ErrEmailChangeTokenInvalid = errors.New("confirm_email_change: token invalid or expired")

// ConfirmEmailChangeHandler runs the confirm flow.
type ConfirmEmailChangeHandler struct {
	persons person.Repository
	now     func() time.Time
}

// NewConfirmEmailChangeHandler wires the handler. `now` is the explicit
// time source per the clock-injection refactor. Nil → time.Now.
func NewConfirmEmailChangeHandler(persons person.Repository, now func() time.Time) ConfirmEmailChangeHandler {
	if persons == nil {
		panic("command: NewConfirmEmailChangeHandler persons repository required")
	}
	if now == nil {
		now = time.Now
	}
	return ConfirmEmailChangeHandler{persons: persons, now: now}
}

// Handle runs the confirm flow.
func (h ConfirmEmailChangeHandler) Handle(ctx context.Context, cmd ConfirmEmailChangeCommand) error {
	if cmd.RawToken == "" {
		return ErrEmailChangeTokenInvalid
	}
	hashHex := hashEmailChangeToken(cmd.RawToken)
	tokenHash, err := person.NewEmailChangeTokenHash(hashHex)
	if err != nil {
		return ErrEmailChangeTokenInvalid
	}

	p, err := h.persons.GetByEmailChangeTokenHash(ctx, tokenHash)
	switch {
	case errors.Is(err, person.ErrNotFound):
		return ErrEmailChangeTokenInvalid
	case err != nil:
		return fmt.Errorf("confirm_email_change: lookup: %w", err)
	}

	if p.IsAnonymised() || p.IsGloballySuspended() {
		return ErrEmailChangeTokenInvalid
	}

	now := h.now()
	if err := h.persons.UpdateByID(ctx, p.ID(), func(loaded *person.Person) (bool, error) {
		if err := loaded.ConfirmEmailChange(tokenHash, now); err != nil {
			return false, err
		}
		return true, nil
	}); err != nil {
		if errors.Is(err, person.ErrInvalid) {
			return ErrEmailChangeTokenInvalid
		}
		// Email-uniqueness collision at commit time (rare race —
		// another Person took the email between request + confirm).
		// Surface as the generic Rejected error rather than 409 to
		// avoid leaking the collision to the original requester.
		if errors.Is(err, person.ErrEmailTaken) {
			return ErrEmailChangeTokenInvalid
		}
		return fmt.Errorf("confirm_email_change: persist: %w", err)
	}
	return nil
}

func hashEmailChangeToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}
