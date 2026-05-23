package command_test

import (
	"context"
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/platform/app/command"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
	"github.com/leadkart/leadkart-go/internal/platform/platformtest"
)

// TestRejectUnverifiedContact_HappyPath — Lead Agent terminal-rejects
// a contact. Aggregate transitions to Rejected; outbox row landed
// via the mechanical mapper. C2 — review-pass.
func TestRejectUnverifiedContact_HappyPath(t *testing.T) {
	t.Parallel()

	contacts := platformtest.NewFakeUnverifiedContactRepository()

	// Seed: a fresh New-state contact.
	agentID := unverifiedcontact.MembershipID(ids.NewV7().String())
	cID := unverifiedcontact.ID(ids.NewV7().String())
	c, err := unverifiedcontact.New(cID, sampleForm(t), agentID, nowFunc())
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := contacts.Add(context.Background(), c); err != nil {
		t.Fatalf("seed add: %v", err)
	}

	h := command.NewRejectUnverifiedContactHandler(contacts, nowFunc)
	err = h.Handle(context.Background(), command.RejectUnverifiedContactCommand{
		ContactID:  cID,
		Reason:     "Obvious test data",
		RejectedBy: agentID,
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}

	loaded, _ := contacts.GetByID(context.Background(), cID)
	if loaded.State() != unverifiedcontact.StateRejected {
		t.Errorf("state=%q want rejected", loaded.State())
	}
	if loaded.RejectionReason() != "Obvious test data" {
		t.Errorf("reason=%q", loaded.RejectionReason())
	}
}

// TestRejectUnverifiedContact_ContactNotFound — missing contact
// surfaces as the typed sentinel the HTTP layer maps to 404. C2.
func TestRejectUnverifiedContact_ContactNotFound(t *testing.T) {
	t.Parallel()

	contacts := platformtest.NewFakeUnverifiedContactRepository()
	h := command.NewRejectUnverifiedContactHandler(contacts, nowFunc)

	err := h.Handle(context.Background(), command.RejectUnverifiedContactCommand{
		ContactID:  unverifiedcontact.ID("01900000-0000-7000-8000-000000000999"),
		Reason:     "anything",
		RejectedBy: unverifiedcontact.MembershipID(ids.NewV7().String()),
	})
	if !errors.Is(err, command.ErrContactNotFound) {
		t.Fatalf("expected ErrContactNotFound, got %v", err)
	}
}

// TestRejectUnverifiedContact_FromInCallState — when the contact is
// already InCall (a call log preceded the reject), the handler skips
// the StartCall promotion + goes straight to MarkRejected. C2.
func TestRejectUnverifiedContact_FromInCallState(t *testing.T) {
	t.Parallel()

	contacts := platformtest.NewFakeUnverifiedContactRepository()

	agentID := unverifiedcontact.MembershipID(ids.NewV7().String())
	cID := unverifiedcontact.ID(ids.NewV7().String())
	c, err := unverifiedcontact.New(cID, sampleForm(t), agentID, nowFunc())
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := c.StartCall(nowFunc()); err != nil {
		t.Fatalf("seed start call: %v", err)
	}
	if err := contacts.Add(context.Background(), c); err != nil {
		t.Fatalf("seed add: %v", err)
	}

	h := command.NewRejectUnverifiedContactHandler(contacts, nowFunc)
	err = h.Handle(context.Background(), command.RejectUnverifiedContactCommand{
		ContactID:  cID,
		Reason:     "Customer not interested",
		RejectedBy: agentID,
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	loaded, _ := contacts.GetByID(context.Background(), cID)
	if loaded.State() != unverifiedcontact.StateRejected {
		t.Errorf("state=%q want rejected", loaded.State())
	}
}
