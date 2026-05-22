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

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/identity/domain/person"
)

// RequestPasswordResetCommand initiates the forgot-password flow.
//
// Auth0 / Okta canon: ALWAYS return success (200) regardless of
// whether the email is registered — the existence of an account is a
// disclosure vector. Behavioural difference happens only on the
// downstream side-effect (email sent vs not).
type RequestPasswordResetCommand struct {
	Email email.Address
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
//
// Per ADR 0057: the SYNCHRONOUS email-gateway dependency has moved out
// of this handler. The aggregate now records BOTH the audit event AND
// the email-dispatch event (carrying the plaintext token); a Watermill
// subscriber consumes the dispatch event and delivers the email. This
// handler is now a thin orchestrator over the aggregate — TDL EDA canon.
type RequestPasswordResetHandler struct {
	persons person.Repository
}

// NewRequestPasswordResetHandler wires the handler.
func NewRequestPasswordResetHandler(persons person.Repository) RequestPasswordResetHandler {
	if persons == nil {
		panic("command: NewRequestPasswordResetHandler persons repository required")
	}
	return RequestPasswordResetHandler{persons: persons}
}

// Handle runs the request flow.
//
// Flow per security.md "Password reset" + Auth0/Okta canon + ADR 0057:
//
//  1. Lookup Person by email globally.
//  2. If not found / anonymised / globally-suspended: SUCCEED SILENTLY.
//     No event recorded, no email sent. Return nil so the wire-shape
//     is identical to the success path.
//  3. Mint a fresh ⟨plaintext, hash⟩ pair via crypto/rand + SHA-256.
//  4. UpdateByID closure: Person.RequestPasswordReset(plaintext, hash, ttl).
//     The aggregate records the AUDIT event AND the EMAIL-DISPATCH
//     event (carrying the plaintext). Both ride the same outbox tx.
//  5. Return — no synchronous send. The email subscriber drains the
//     outbox + delivers.
//
// The plaintext NEVER hits Postgres's persons table — only the hash.
// The dispatch-event payload carries plaintext through identity.outbox
// briefly (≤1s typical) for async delivery; see ADR 0057 for the
// security analysis of that window.
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
		if err := loaded.RequestPasswordReset(plaintext, tokenHash, PasswordResetTokenTTL); err != nil {
			return false, err
		}
		return true, nil
	}); err != nil {
		return fmt.Errorf("request_password_reset: persist: %w", err)
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
