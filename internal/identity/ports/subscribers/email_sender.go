package subscribers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/ThreeDotsLabs/watermill/message"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/identity/app/command"
	"github.com/leadkart/leadkart-go/internal/identity/integrationevents"
)

// EmailSender is the subscriber that turns the V1 email-dispatch events
// emitted by the password-reset + email-change command handlers (per
// ADR 0057) into actual gateway.Send calls. Single responsibility:
// build the [email.Message] from the event payload + dispatch.
//
// Failure mode (Watermill canon — must-succeed):
//
//   - email.NewMessage validation failure → return error → Watermill
//     retries via the canonical retry middleware. The plaintext
//     token + expiry are bounded, so a stuck retry loop self-resolves
//     when the token expires (next request mints a fresh one).
//   - gateway.Send failure → same shape (return error, retry).
//   - Topic-mismatch (handler wired against the wrong event_type) →
//     short-circuit silently per the established subscriber pattern
//     (InvalidateSecurityStampCache canon — handlers ride the shared
//     Identity topic + filter by event_type header).
//
// Production wiring (cmd/worker) injects a real provider behind the
// [email.Gateway] interface; v0.2 stays Recorder-backed (no SMTP/SES
// integration shipped). The composition-root swap is the v0.3 cutover
// point per ADR 0057 §"v0.2 → v0.3 migration path".
type EmailSender struct {
	gateway email.Gateway
	from    email.Address
	appURL  string // base URL where the email links point (frontend deploy)
	log     *slog.Logger
}

// NewEmailSender wires the subscriber. gateway + from + appURL are
// required; nil/empty values panic (boundary check).
//
// appURL is the base URL the reset / confirmation links point at; the
// subscriber appends `/reset-password?token=...` or
// `/confirm-email-change?token=...`. v0.2 default
// "https://app.leadkart.example" pending real frontend deploy.
func NewEmailSender(gateway email.Gateway, from email.Address, appURL string, log *slog.Logger) *EmailSender {
	if gateway == nil {
		panic("subscribers: NewEmailSender gateway required (use email.Recorder in tests)")
	}
	if from.IsZero() {
		panic("subscribers: NewEmailSender from-address required")
	}
	if appURL == "" {
		panic("subscribers: NewEmailSender appURL required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &EmailSender{gateway: gateway, from: from, appURL: appURL, log: log}
}

// HandlePasswordResetEmail is the handler for
// `identity.person_password_reset_email_requested.v1`. Decodes the
// event payload + builds the reset-link email + dispatches via the
// gateway. Returns the gateway error so Watermill retries on transient
// failure.
func (h *EmailSender) HandlePasswordResetEmail(
	ctx context.Context, _ string, msg *message.Message,
) error {
	expected := integrationevents.PersonPasswordResetEmailRequestedV1{}.Topic()
	if msg.Metadata.Get(messaging.HeaderEventType) != expected {
		// Topic-mismatch on the shared identity topic — not for us.
		return nil
	}
	var evt integrationevents.PersonPasswordResetEmailRequestedV1
	if err := json.Unmarshal(msg.Payload, &evt); err != nil {
		return fmt.Errorf("subscribers: decode %s: %w", expected, err)
	}
	to, err := email.New(evt.Email)
	if err != nil {
		return fmt.Errorf("subscribers: hydrate to address %q: %w", evt.Email, err)
	}
	greeting := evt.RecipientName
	if greeting == "" {
		greeting = "there"
	}
	body := "Hi " + greeting + ",\n\n" +
		"You (or someone) requested a password reset for your LeadKart account. " +
		"To choose a new password, open the link below within " +
		command.PasswordResetTokenTTL.String() + ":\n\n" +
		h.appURL + "/reset-password?token=" + evt.PlaintextToken + "\n\n" +
		"If you did not request this, you can safely ignore this email — " +
		"the token will expire automatically."

	emailMsg, err := email.NewMessage(to, h.from, "Reset your LeadKart password", body)
	if err != nil {
		return fmt.Errorf("subscribers: build password-reset message: %w", err)
	}
	if err := h.gateway.Send(ctx, emailMsg); err != nil {
		// Wrap with the gateway sentinel so callers / SIEM can split
		// transient (retry) from validation (DLQ-after-N) per the
		// email.Gateway error contract.
		if errors.Is(err, email.ErrInvalidMessage) {
			return fmt.Errorf("subscribers: password-reset send: %w", err)
		}
		return fmt.Errorf("subscribers: password-reset send: %w", err)
	}
	h.log.InfoContext(ctx, "password reset email sent",
		"person_id", evt.PersonID.String(),
		"to", evt.Email)
	return nil
}

// HandleEmailChangeConfirmation is the handler for
// `identity.person_email_change_confirmation_requested.v1`. Mirrors the
// password-reset shape; the confirmation link goes to the NEW address
// per Auth0/Okta canon.
func (h *EmailSender) HandleEmailChangeConfirmation(
	ctx context.Context, _ string, msg *message.Message,
) error {
	expected := integrationevents.PersonEmailChangeConfirmationRequestedV1{}.Topic()
	if msg.Metadata.Get(messaging.HeaderEventType) != expected {
		return nil
	}
	var evt integrationevents.PersonEmailChangeConfirmationRequestedV1
	if err := json.Unmarshal(msg.Payload, &evt); err != nil {
		return fmt.Errorf("subscribers: decode %s: %w", expected, err)
	}
	to, err := email.New(evt.NewEmail)
	if err != nil {
		return fmt.Errorf("subscribers: hydrate to address %q: %w", evt.NewEmail, err)
	}
	greeting := evt.RecipientName
	if greeting == "" {
		greeting = "there"
	}
	body := "Hi " + greeting + ",\n\n" +
		"You requested to change your LeadKart account email to this address. " +
		"To confirm, open the link below within " +
		command.EmailChangeTokenTTL.String() + ":\n\n" +
		h.appURL + "/confirm-email-change?token=" + evt.PlaintextToken + "\n\n" +
		"If you did not make this request, ignore this email — the request " +
		"will expire automatically."

	emailMsg, err := email.NewMessage(to, h.from, "Confirm your new LeadKart email", body)
	if err != nil {
		return fmt.Errorf("subscribers: build email-change message: %w", err)
	}
	if err := h.gateway.Send(ctx, emailMsg); err != nil {
		return fmt.Errorf("subscribers: email-change send: %w", err)
	}
	h.log.InfoContext(ctx, "email change confirmation sent",
		"person_id", evt.PersonID.String(),
		"new_email", evt.NewEmail)
	return nil
}
