package subscribers_test

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/leadkart/leadkart-go/internal/common/email"
	"github.com/leadkart/leadkart-go/internal/identity/integrationevents"
	"github.com/leadkart/leadkart-go/internal/identity/ports/subscribers"
)

// recorderGateway captures the last email.Message sent.
type recorderGateway struct {
	sent []email.Message
}

func (r *recorderGateway) Send(_ context.Context, m email.Message) error {
	r.sent = append(r.sent, m)
	return nil
}

func newTestSender(t *testing.T, gw email.Gateway) *subscribers.EmailSender {
	t.Helper()
	from, err := email.New("no-reply@leadkart.test")
	if err != nil {
		t.Fatalf("from addr: %v", err)
	}
	return subscribers.NewEmailSender(gw, from, "https://app.example", silentLogES())
}

func silentLogES() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// Post-cqrs (ADR 0067): handlers receive the already-decoded typed event;
// topic routing + payload decode are the EventProcessor's job, so the old
// wrong-event-type skip tests are gone (the handler can never be called
// with a mismatched type).

func TestEmailSender_PasswordReset_Sends(t *testing.T) {
	t.Parallel()
	gw := &recorderGateway{}
	sender := newTestSender(t, gw)
	evt := integrationevents.PersonPasswordResetEmailRequestedV1{
		PersonID:       uuid.New(),
		Email:          "user@example.com",
		RecipientName:  "User",
		PlaintextToken: "tok-123",
	}
	if err := sender.HandlePasswordResetEmail(t.Context(), &evt); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(gw.sent) != 1 {
		t.Fatalf("want 1 sent, got %d", len(gw.sent))
	}
}

func TestEmailSender_EmailChange_Sends(t *testing.T) {
	t.Parallel()
	gw := &recorderGateway{}
	sender := newTestSender(t, gw)
	evt := integrationevents.PersonEmailChangeConfirmationRequestedV1{
		PersonID:       uuid.New(),
		NewEmail:       "new@example.com",
		RecipientName:  "User",
		PlaintextToken: "tok-xyz",
	}
	if err := sender.HandleEmailChangeConfirmation(t.Context(), &evt); err != nil {
		t.Fatalf("handle: %v", err)
	}
	if len(gw.sent) != 1 {
		t.Fatalf("want 1 sent, got %d", len(gw.sent))
	}
}
