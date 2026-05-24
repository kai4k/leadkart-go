package command_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/platform/app/command"
	"github.com/leadkart/leadkart-go/internal/platform/domain/unverifiedcontact"
	"github.com/leadkart/leadkart-go/internal/platform/domain/verificationcall"
	"github.com/leadkart/leadkart-go/internal/platform/platformtest"
)

// seedNewContact inserts a fresh New-state contact.
func seedNewContact(t *testing.T, contacts *platformtest.FakeUnverifiedContactRepository) (unverifiedcontact.ID, unverifiedcontact.MembershipID) {
	t.Helper()
	agentID := unverifiedcontact.MembershipID(ids.NewV7().String())
	cID := unverifiedcontact.ID(ids.NewV7().String())
	c, err := unverifiedcontact.New(cID, sampleForm(t), agentID, nowFunc())
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := contacts.Add(context.Background(), c); err != nil {
		t.Fatalf("seed add: %v", err)
	}
	return cID, agentID
}

func TestLogVerificationCall_NoAnswerLeavesContactInCall(t *testing.T) {
	t.Parallel()

	contacts := platformtest.NewFakeUnverifiedContactRepository()
	calls := platformtest.NewFakeVerificationCallRepository()
	uow := platformtest.NewFakeUnitOfWork()

	cID, agentID := seedNewContact(t, contacts)

	h := command.NewLogVerificationCallHandler(uow, calls, contacts, nowFunc)
	out, err := h.Handle(context.Background(), command.LogVerificationCallCommand{
		ContactID: cID,
		Outcome:   verificationcall.OutcomeNoAnswer,
		Notes:     "Rang out",
		LoggedBy:  agentID,
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	if out.CallID == "" {
		t.Error("expected CallID")
	}
	// Contact promoted to InCall by the handler's StartCall promotion.
	loaded, _ := contacts.GetByID(context.Background(), cID)
	if loaded.State() != unverifiedcontact.StateInCall {
		t.Errorf("state=%q want in_call", loaded.State())
	}
}

func TestLogVerificationCall_BusyMarksContactBusyWithWindow(t *testing.T) {
	t.Parallel()

	contacts := platformtest.NewFakeUnverifiedContactRepository()
	calls := platformtest.NewFakeVerificationCallRepository()
	uow := platformtest.NewFakeUnitOfWork()

	cID, agentID := seedNewContact(t, contacts)

	cbStart := nowFunc().Add(time.Hour)
	cbEnd := cbStart.Add(30 * time.Minute)

	h := command.NewLogVerificationCallHandler(uow, calls, contacts, nowFunc)
	_, err := h.Handle(context.Background(), command.LogVerificationCallCommand{
		ContactID:             cID,
		Outcome:               verificationcall.OutcomeBusy,
		Notes:                 "Customer asked to call later",
		CallbackWindowStartAt: cbStart,
		CallbackWindowEndAt:   cbEnd,
		LoggedBy:              agentID,
	})
	if err != nil {
		t.Fatalf("handle: %v", err)
	}
	loaded, _ := contacts.GetByID(context.Background(), cID)
	if loaded.State() != unverifiedcontact.StateBusy {
		t.Errorf("state=%q want busy", loaded.State())
	}
	if !loaded.BusyCallbackAt().Equal(cbStart) {
		t.Errorf("BusyCallbackAt mismatch")
	}
}

func TestLogVerificationCall_ContactNotFound(t *testing.T) {
	t.Parallel()

	contacts := platformtest.NewFakeUnverifiedContactRepository()
	calls := platformtest.NewFakeVerificationCallRepository()
	uow := platformtest.NewFakeUnitOfWork()

	h := command.NewLogVerificationCallHandler(uow, calls, contacts, nowFunc)
	_, err := h.Handle(context.Background(), command.LogVerificationCallCommand{
		ContactID: unverifiedcontact.ID("01900000-0000-7000-8000-000000000999"),
		Outcome:   verificationcall.OutcomeNoAnswer,
		LoggedBy:  unverifiedcontact.MembershipID(ids.NewV7().String()),
	})
	if !errors.Is(err, command.ErrContactNotFound) {
		t.Errorf("expected ErrContactNotFound, got %v", err)
	}
}
