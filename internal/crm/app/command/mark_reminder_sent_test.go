package command_test

import (
	"errors"
	"testing"

	"github.com/leadkart/leadkart-go/internal/crm/app/command"
	"github.com/leadkart/leadkart-go/internal/crm/domain/reminder"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

func TestMarkReminderSent_HappyPath(t *testing.T) {
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
	h := command.NewMarkReminderSentHandler(reminders, fixedTime)
	if err := h.Handle(t.Context(), command.MarkReminderSentCommand{
		TenantID: testTenantID, ReminderID: out.ReminderID, MarkedByMembershipID: testReminderCreator,
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	got, _ := reminders.GetByID(t.Context(), testTenantID, out.ReminderID)
	if got.State() != reminder.StateSent {
		t.Fatalf("state: %q", got.State())
	}
}

func TestMarkReminderSent_NotFound(t *testing.T) {
	t.Parallel()
	reminders := newFakeReminders()
	h := command.NewMarkReminderSentHandler(reminders, fixedTime)
	err := h.Handle(t.Context(), command.MarkReminderSentCommand{
		TenantID: testTenantID, ReminderID: reminder.ID("01923400-0000-7000-8000-eeeeeeee0001"), MarkedByMembershipID: testReminderCreator,
	})
	if !errors.Is(err, command.ErrReminderNotFound) {
		t.Fatalf("want ErrReminderNotFound, got %v", err)
	}
}

func TestMarkReminderSent_TerminalIsConflict(t *testing.T) {
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
	h := command.NewMarkReminderSentHandler(reminders, fixedTime)
	if err := h.Handle(t.Context(), command.MarkReminderSentCommand{
		TenantID: testTenantID, ReminderID: out.ReminderID, MarkedByMembershipID: testReminderCreator,
	}); err != nil {
		t.Fatalf("first mark sent: %v", err)
	}
	err = h.Handle(t.Context(), command.MarkReminderSentCommand{
		TenantID: testTenantID, ReminderID: out.ReminderID, MarkedByMembershipID: testReminderCreator,
	})
	if !errors.Is(err, command.ErrReminderTerminal) {
		t.Fatalf("want ErrReminderTerminal, got %v", err)
	}
}

func TestMarkReminderSent_RejectsZeroTenantID(t *testing.T) {
	t.Parallel()
	h := command.NewMarkReminderSentHandler(newFakeReminders(), fixedTime)
	err := h.Handle(t.Context(), command.MarkReminderSentCommand{
		TenantID: tenant.ID(""), ReminderID: reminder.ID("01923400-0000-7000-8000-ffffffff0001"), MarkedByMembershipID: testReminderCreator,
	})
	if err == nil {
		t.Fatal("want input-validation error on zero TenantID")
	}
}
