package subscribers_test

import (
	"encoding/json"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/ThreeDotsLabs/watermill/message"
	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/common/messaging"
	"github.com/leadkart/leadkart-go/internal/identity/integrationevents"
	"github.com/leadkart/leadkart-go/internal/identity/ports/subscribers"
)

var fixedNow = time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

// emailSenderSilentLog — unit-only logger; the sibling integration
// test file declares its own silentLog under the integration tag.
func emailSenderSilentLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func mustAddr(t *testing.T, s string) email.Address {
	t.Helper()
	a, err := email.New(s)
	if err != nil {
		t.Fatalf("email.New(%q): %v", s, err)
	}
	return a
}

// makeMsg wraps an event payload into a Watermill *Message with the
// canonical event_type header — same shape the OutboxForwarder emits.
func makeMsg(t *testing.T, topic string, payload any) *message.Message {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	msg := message.NewMessage(uuid.NewString(), body)
	msg.Metadata.Set(messaging.HeaderEventType, topic)
	return msg
}

func TestEmailSender_HandlePasswordResetEmail_Sends(t *testing.T) {
	t.Parallel()

	rec := email.NewRecorder(time.Now)
	from := mustAddr(t, "no-reply@leadkart.test")
	sender := subscribers.NewEmailSender(rec, from, "https://app.leadkart.test", emailSenderSilentLog())

	personID := uuid.New()
	evt := integrationevents.PersonPasswordResetEmailRequestedV1{
		PersonID:       personID,
		Email:          "alice@example.test",
		PlaintextToken: "tok-ABC-123",
		ExpiresAtUTC:   fixedNow.UTC().Add(time.Hour),
		RecipientName:  "Alice",
		OccurredAtUTC:  fixedNow.UTC(),
	}
	msg := makeMsg(t, evt.Topic(), evt)

	if err := sender.HandlePasswordResetEmail(t.Context(), msg.UUID, msg); err != nil {
		t.Fatalf("HandlePasswordResetEmail: %v", err)
	}

	if got := rec.Count(); got != 1 {
		t.Fatalf("recorder count = %d, want 1", got)
	}
	got := rec.Sent()[0]
	if got.To().String() != "alice@example.test" {
		t.Errorf("to = %s, want alice@example.test", got.To())
	}
	if got.From().String() != from.String() {
		t.Errorf("from = %s, want %s", got.From(), from)
	}
	if !strings.Contains(got.Subject(), "Reset") {
		t.Errorf("subject = %q, want substring Reset", got.Subject())
	}
	if !strings.Contains(got.BodyText(), "tok-ABC-123") {
		t.Errorf("body missing plaintext token; got: %s", got.BodyText())
	}
	if !strings.Contains(got.BodyText(), "https://app.leadkart.test/reset-password?token=") {
		t.Errorf("body missing link; got: %s", got.BodyText())
	}
	if !strings.Contains(got.BodyText(), "Alice") {
		t.Errorf("body missing recipient name; got: %s", got.BodyText())
	}
}

func TestEmailSender_HandlePasswordResetEmail_TopicMismatch_NoOp(t *testing.T) {
	t.Parallel()

	rec := email.NewRecorder(time.Now)
	from := mustAddr(t, "no-reply@leadkart.test")
	sender := subscribers.NewEmailSender(rec, from, "https://app.leadkart.test", emailSenderSilentLog())

	// Build a message stamped with a DIFFERENT event_type — handler
	// must short-circuit silently (sibling subscribers ride the same
	// shared topic per the InvalidateSecurityStampCache canon).
	msg := makeMsg(t, "identity.something_else.v1", struct{}{})

	if err := sender.HandlePasswordResetEmail(t.Context(), msg.UUID, msg); err != nil {
		t.Fatalf("HandlePasswordResetEmail: %v", err)
	}
	if rec.Count() != 0 {
		t.Errorf("recorder count = %d, want 0 (topic-mismatch short-circuit)", rec.Count())
	}
}

func TestEmailSender_HandleEmailChangeConfirmation_Sends(t *testing.T) {
	t.Parallel()

	rec := email.NewRecorder(time.Now)
	from := mustAddr(t, "no-reply@leadkart.test")
	sender := subscribers.NewEmailSender(rec, from, "https://app.leadkart.test", emailSenderSilentLog())

	personID := uuid.New()
	evt := integrationevents.PersonEmailChangeConfirmationRequestedV1{
		PersonID:       personID,
		NewEmail:       "new@example.test",
		OldEmail:       "old@example.test",
		PlaintextToken: "ec-tok-XYZ-789",
		ExpiresAtUTC:   fixedNow.UTC().Add(time.Hour),
		RecipientName:  "Bob",
		OccurredAtUTC:  fixedNow.UTC(),
	}
	msg := makeMsg(t, evt.Topic(), evt)

	if err := sender.HandleEmailChangeConfirmation(t.Context(), msg.UUID, msg); err != nil {
		t.Fatalf("HandleEmailChangeConfirmation: %v", err)
	}

	if got := rec.Count(); got != 1 {
		t.Fatalf("recorder count = %d, want 1", got)
	}
	got := rec.Sent()[0]
	// Confirmation goes to the NEW address (Auth0/Okta canon).
	if got.To().String() != "new@example.test" {
		t.Errorf("to = %s, want new@example.test", got.To())
	}
	if !strings.Contains(got.Subject(), "Confirm") {
		t.Errorf("subject = %q, want substring Confirm", got.Subject())
	}
	if !strings.Contains(got.BodyText(), "ec-tok-XYZ-789") {
		t.Errorf("body missing plaintext token; got: %s", got.BodyText())
	}
	if !strings.Contains(got.BodyText(), "https://app.leadkart.test/confirm-email-change?token=") {
		t.Errorf("body missing link; got: %s", got.BodyText())
	}
}

func TestEmailSender_HandlePasswordResetEmail_EmptyRecipientName_FallsBack(t *testing.T) {
	t.Parallel()

	rec := email.NewRecorder(time.Now)
	from := mustAddr(t, "no-reply@leadkart.test")
	sender := subscribers.NewEmailSender(rec, from, "https://app.leadkart.test", emailSenderSilentLog())

	evt := integrationevents.PersonPasswordResetEmailRequestedV1{
		PersonID:       uuid.New(),
		Email:          "alice@example.test",
		PlaintextToken: "tok",
		ExpiresAtUTC:   fixedNow.UTC().Add(time.Hour),
		RecipientName:  "", // empty
		OccurredAtUTC:  fixedNow.UTC(),
	}
	msg := makeMsg(t, evt.Topic(), evt)

	if err := sender.HandlePasswordResetEmail(t.Context(), msg.UUID, msg); err != nil {
		t.Fatalf("HandlePasswordResetEmail: %v", err)
	}
	if rec.Count() != 1 {
		t.Fatalf("recorder count = %d, want 1", rec.Count())
	}
	if !strings.Contains(rec.Sent()[0].BodyText(), "Hi there") {
		t.Errorf("expected 'Hi there' fallback greeting; got: %s", rec.Sent()[0].BodyText())
	}
}
