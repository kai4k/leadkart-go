package command_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/crm/app/command"
	"github.com/leadkart/leadkart-go/internal/crm/domain/reminder"
)

func TestCancelReminder_HappyPath(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	reminders := newFakeReminders()
	leadID := seedLead(t, leads)
	create := command.NewCreateReminderHandler(leads, reminders, fixedTime, newTestReminderID)
	out, err := create.Handle(t.Context(), command.CreateReminderCommand{
		TenantID: testTenantID, LeadID: leadID,
		AssignedToMembershipID: testReminderAssignee,
		CreatedByMembershipID:  testReminderCreator,
		Type:                   reminder.TypeManual, DueAt: dueAt(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	h := command.NewCancelReminderHandler(reminders, fixedTime)
	if err := h.Handle(t.Context(), command.CancelReminderCommand{
		TenantID: testTenantID, ReminderID: out.ReminderID,
		CancelledByMembershipID: testReminderCreator, Reason: "lead pivot",
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, _ := reminders.GetByID(t.Context(), testTenantID, out.ReminderID)
	if got.State() != reminder.StateCancelled {
		t.Fatalf("state: %q", got.State())
	}
	if got.CancelReason() != "lead pivot" {
		t.Fatalf("CancelReason: %q", got.CancelReason())
	}
}

func TestCancelReminder_RequiresReason(t *testing.T) {
	t.Parallel()
	reminders := newFakeReminders()
	h := command.NewCancelReminderHandler(reminders, fixedTime)
	err := h.Handle(t.Context(), command.CancelReminderCommand{
		TenantID: testTenantID, ReminderID: reminder.ID("01923400-0000-7000-8000-cccccccc0001"),
		CancelledByMembershipID: testReminderCreator, Reason: "",
	})
	if err == nil {
		t.Fatal("want input-validation error on empty Reason")
	}
}

func TestCancelReminder_DoubleCancelIsConflict(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	reminders := newFakeReminders()
	leadID := seedLead(t, leads)
	create := command.NewCreateReminderHandler(leads, reminders, fixedTime, newTestReminderID)
	out, err := create.Handle(t.Context(), command.CreateReminderCommand{
		TenantID: testTenantID, LeadID: leadID,
		AssignedToMembershipID: testReminderAssignee,
		CreatedByMembershipID:  testReminderCreator,
		Type:                   reminder.TypeManual, DueAt: dueAt(),
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	h := command.NewCancelReminderHandler(reminders, fixedTime)
	if err := h.Handle(t.Context(), command.CancelReminderCommand{
		TenantID: testTenantID, ReminderID: out.ReminderID,
		CancelledByMembershipID: testReminderCreator, Reason: "first",
	}); err != nil {
		t.Fatalf("first cancel: %v", err)
	}
	err = h.Handle(t.Context(), command.CancelReminderCommand{
		TenantID: testTenantID, ReminderID: out.ReminderID,
		CancelledByMembershipID: testReminderCreator, Reason: "second",
	})
	if !errors.Is(err, command.ErrReminderTerminal) {
		t.Fatalf("want ErrReminderTerminal, got %v", err)
	}
}
