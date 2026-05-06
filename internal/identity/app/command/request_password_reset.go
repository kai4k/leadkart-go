package command

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	commonemail "github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
	"github.com/leadkart/leadkart-go/internal/platform/email"
)

// RequestPasswordResetCommand initiates the forgot-password flow.
//
// Auth0 / Okta canon: ALWAYS return success (200) regardless of
// whether the email is registered — the existence of an account is a
// disclosure vector. Behavioural difference happens only on the
// downstream side-effect (email sent vs not).
type RequestPasswordResetCommand struct {
	Email commonemail.Address
}

// PasswordResetTokenTTL is the default validity window for a reset
// token. Per `security.md` "Password reset" + Auth0/Okta canon:
// 1h for high-traffic SaaS, up to 24h for low-traffic B2B. LeadKart
// uses 1h to limit the attack window on a stolen email account.
const PasswordResetTokenTTL = 1 * time.Hour

// passwordResetTokenBytes is the random source size. 32 bytes = 256
// bits — RFC 9700-recommended floor for opaque-token entropy.
const passwordResetTokenBytes = 32

// RequestPasswordResetHandler runs the request-side of the reset flow.
type RequestPasswordResetHandler struct {
	persons      person.Repository
	emailGateway email.Gateway

	// resetEmailFromAddress is the From: header on the outgoing reset
	// email; supplied by composition root from config.
	resetEmailFromAddress commonemail.Address
}

// NewRequestPasswordResetHandler wires the handler.
//
// emailGateway MUST NOT be nil — a nil gateway silently breaks the
// "we sent you a link" UX promise.
func NewRequestPasswordResetHandler(
	persons person.Repository,
	emailGateway email.Gateway,
	fromAddress commonemail.Address,
) RequestPasswordResetHandler {
	if persons == nil {
		panic("command: NewRequestPasswordResetHandler persons repository required")
	}
	if emailGateway == nil {
		panic("command: NewRequestPasswordResetHandler email gateway required (use email.Recorder in tests)")
	}
	if fromAddress.IsZero() {
		panic("command: NewRequestPasswordResetHandler from-address required")
	}
	return RequestPasswordResetHandler{
		persons:               persons,
		emailGateway:          emailGateway,
		resetEmailFromAddress: fromAddress,
	}
}

// Handle runs the request flow.
//
// Flow per security.md "Password reset" + Auth0/Okta canon:
//
//  1. Lookup Person by email globally.
//  2. If not found / anonymised / globally-suspended: SUCCEED SILENTLY.
//     No email sent, no aggregate write. Return nil so the wire-shape
//     is identical to the success path.
//  3. Mint a fresh ⟨plaintext, hash⟩ pair via crypto/rand + SHA-256.
//  4. UpdateByID closure: Person.RequestPasswordReset(hash, ttl).
//  5. Email gateway publishes the plaintext to the user's inbox.
//
// The plaintext NEVER hits Postgres — only the hash. Confirm flow
// hashes the user-supplied plaintext + looks up by hash via
// uq_persons_password_reset_hash unique index.
func (h RequestPasswordResetHandler) Handle(ctx context.Context, cmd RequestPasswordResetCommand) error {
	if cmd.Email.IsZero() {
		// Boundary-layer validation; HTTP layer should reject malformed
		// emails before reaching here. Surface as a generic 400-ish
		// error so the wire still leaks no presence info if it does.
		return errors.New("request_password_reset: email required")
	}

	p, err := h.persons.GetByEmail(ctx, cmd.Email)
	switch {
	case errors.Is(err, person.ErrNotFound):
		// Silent success — Auth0 / Okta canon. No email enumeration.
		return nil
	case err != nil:
		return fmt.Errorf("request_password_reset: lookup: %w", err)
	}

	// Anonymised / globally-suspended Persons cannot reset; surface as
	// silent success too (avoid leaking suspension state).
	if p.IsAnonymised() || p.IsGloballySuspended() {
		return nil
	}

	plaintext, hashHex, err := mintResetToken()
	if err != nil {
		return fmt.Errorf("request_password_reset: mint: %w", err)
	}
	tokenHash, err := person.NewPasswordResetTokenHash(hashHex)
	if err != nil {
		return fmt.Errorf("request_password_reset: wrap hash: %w", err)
	}

	if err := h.persons.UpdateByID(ctx, p.ID(), func(loaded *person.Person) (bool, error) {
		if err := loaded.RequestPasswordReset(tokenHash, PasswordResetTokenTTL); err != nil {
			return false, err
		}
		return true, nil
	}); err != nil {
		return fmt.Errorf("request_password_reset: persist: %w", err)
	}

	// Send the email. Failure to send IS surfaced — the user expects
	// "we sent you a link" UX. Aggregate state already persisted (the
	// pending reset is durable); a re-request supersedes naturally.
	msg, err := email.NewMessage(
		cmd.Email,
		h.resetEmailFromAddress,
		"Reset your LeadKart password",
		"You (or someone) requested a password reset for your LeadKart account. "+
			"To choose a new password, open the link below within "+
			PasswordResetTokenTTL.String()+":\n\n"+
			"https://app.leadkart.example/reset-password?token="+plaintext+"\n\n"+
			"If you did not request this, you can safely ignore this email — "+
			"the token will expire automatically.",
	)
	if err != nil {
		return fmt.Errorf("request_password_reset: build message: %w", err)
	}
	if err := h.emailGateway.Send(ctx, msg); err != nil {
		return fmt.Errorf("request_password_reset: send: %w", err)
	}
	return nil
}

// mintResetToken returns ⟨plaintext, hashHex⟩. base64url plaintext +
// SHA-256 hex hash mirrors the refreshmint shape — same crypto, same
// bytes-on-the-wire, same hash-only-storage discipline.
func mintResetToken() (plaintext, hashHex string, err error) {
	buf := make([]byte, passwordResetTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("rand: %w", err)
	}
	plaintext = base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(plaintext))
	hashHex = hex.EncodeToString(sum[:])
	return plaintext, hashHex, nil
}
