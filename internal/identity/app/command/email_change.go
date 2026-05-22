package command

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/common/email"
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
type RequestEmailChangeHandler struct {
	persons      person.Repository
	emailGateway email.Gateway
	fromAddress  email.Address
}

// NewRequestEmailChangeHandler wires the handler.
func NewRequestEmailChangeHandler(
	persons person.Repository,
	emailGateway email.Gateway,
	fromAddress email.Address,
) RequestEmailChangeHandler {
	if persons == nil {
		panic("command: NewRequestEmailChangeHandler persons repository required")
	}
	if emailGateway == nil {
		panic("command: NewRequestEmailChangeHandler email gateway required")
	}
	if fromAddress.IsZero() {
		panic("command: NewRequestEmailChangeHandler from-address required")
	}
	return RequestEmailChangeHandler{
		persons:      persons,
		emailGateway: emailGateway,
		fromAddress:  fromAddress,
	}
}

// Handle runs the request flow.
//
//  1. Load Person by ID (caller's authenticated identity).
//  2. Reject anonymised / globally-suspended.
//  3. Reject if NewEmail equals current email (no-op).
//  4. Reject if NewEmail already belongs to another Person (409).
//  5. Mint ⟨plaintext, hash⟩ pair via crypto/rand + SHA-256.
//  6. UpdateByID: Person.RequestEmailChange(newEmail, hash, ttl).
//  7. Send confirmation email to the NEW address.
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

	if err := h.persons.UpdateByID(ctx, cmd.PersonID, func(loaded *person.Person) (bool, error) {
		if err := loaded.RequestEmailChange(cmd.NewEmail, tokenHash, EmailChangeTokenTTL); err != nil {
			return false, err
		}
		return true, nil
	}); err != nil {
		return fmt.Errorf("request_email_change: persist: %w", err)
	}

	// Confirmation goes to the NEW address — Auth0/Okta canon. The
	// CURRENT address gets a separate informational email after
	// confirmation (not implemented here; deferred to a downstream
	// Notifications subscriber when the integration event lands).
	msg, err := email.NewMessage(
		cmd.NewEmail,
		h.fromAddress,
		"Confirm your new LeadKart email",
		"You requested to change your LeadKart account email to this address. "+
			"To confirm, open the link below within "+EmailChangeTokenTTL.String()+":\n\n"+
			"https://app.leadkart.example/confirm-email-change?token="+plaintext+"\n\n"+
			"If you did not make this request, ignore this email — the request "+
			"will expire automatically.",
	)
	if err != nil {
		return fmt.Errorf("request_email_change: build message: %w", err)
	}
	if err := h.emailGateway.Send(ctx, msg); err != nil {
		return fmt.Errorf("request_email_change: send: %w", err)
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
}

// NewConfirmEmailChangeHandler wires the handler.
func NewConfirmEmailChangeHandler(persons person.Repository) ConfirmEmailChangeHandler {
	if persons == nil {
		panic("command: NewConfirmEmailChangeHandler persons repository required")
	}
	return ConfirmEmailChangeHandler{persons: persons}
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

	if err := h.persons.UpdateByID(ctx, p.ID(), func(loaded *person.Person) (bool, error) {
		if err := loaded.ConfirmEmailChange(tokenHash); err != nil {
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
