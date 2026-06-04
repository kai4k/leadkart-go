package command_test

import (
	"errors"
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/crm/app/command"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/crm/domain/reminder"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

const (
	testReminderAssignee = "01923400-0000-7000-8000-bbbbbbbb0001"
	testReminderCreator  = "01923400-0000-7000-8000-bbbbbbbb0002"
	testCallLogID        = "01923400-0000-7000-8000-bbbbbbbb0003"
)

func dueAt() time.Time {
	return time.Date(2026, 6, 3, 14, 0, 0, 0, time.UTC)
}

// ----- Manual reminder ------------------------------------------------------

func TestCreateReminder_ManualHappyPath(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	reminders := newFakeReminders()
	leadID := seedLead(t, leads)
	h := command.NewCreateReminderHandler(leads, reminders, fixedTime, newTestReminderID)
	out, err := h.Handle(t.Context(), command.CreateReminderCommand{
		TenantID: testTenantID, LeadID: leadID,
		AssignedToMembershipID: testReminderAssignee,
		CreatedByMembershipID:  testReminderCreator,
		Type:                   reminder.TypeManual,
		DueAt:                  dueAt(),
		Notes:                  "ping in a day",
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out.ReminderID.IsZero() {
		t.Fatal("ReminderID should be set")
	}
	if out.AlreadyExisted {
		t.Fatal("AlreadyExisted should be false on happy path")
	}
}

func TestCreateReminder_LeadNotFound(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	reminders := newFakeReminders()
	h := command.NewCreateReminderHandler(leads, reminders, fixedTime, newTestReminderID)
	_, err := h.Handle(t.Context(), command.CreateReminderCommand{
		TenantID: testTenantID, LeadID: crmlead.ID("01923400-0000-7000-8000-dddddddd0001"),
		AssignedToMembershipID: testReminderAssignee,
		CreatedByMembershipID:  testReminderCreator,
		Type:                   reminder.TypeManual,
		DueAt:                  dueAt(),
	})
	if !errors.Is(err, command.ErrLeadNotFound) {
		t.Fatalf("want ErrLeadNotFound, got %v", err)
	}
}

func TestCreateReminder_ManualRequiresCreator(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	reminders := newFakeReminders()
	leadID := seedLead(t, leads)
	h := command.NewCreateReminderHandler(leads, reminders, fixedTime, newTestReminderID)
	_, err := h.Handle(t.Context(), command.CreateReminderCommand{
		TenantID: testTenantID, LeadID: leadID,
		AssignedToMembershipID: testReminderAssignee,
		Type:                   reminder.TypeManual,
		DueAt:                  dueAt(),
		// no CreatedByMembershipID
	})
	if err == nil || !errors.Is(err, reminder.ErrInvalid) {
		t.Fatalf("want reminder.ErrInvalid wrapped, got %v", err)
	}
}

// ----- Callback reminder ----------------------------------------------------

func TestCreateReminder_CallbackHappyPath(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	reminders := newFakeReminders()
	leadID := seedLead(t, leads)
	h := command.NewCreateReminderHandler(leads, reminders, fixedTime, newTestReminderID)
	out, err := h.Handle(t.Context(), command.CreateReminderCommand{
		TenantID: testTenantID, LeadID: leadID,
		AssignedToMembershipID: testReminderAssignee,
		SourceCallLogID:        testCallLogID,
		Type:                   reminder.TypeCallback,
		DueAt:                  dueAt(),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out.ReminderID.IsZero() {
		t.Fatal("ReminderID should be set")
	}
}

func TestCreateReminder_CallbackDuplicateIsAcked(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	reminders := newFakeReminders()
	leadID := seedLead(t, leads)
	h := command.NewCreateReminderHandler(leads, reminders, fixedTime, newTestReminderID)
	cmd := command.CreateReminderCommand{
		TenantID: testTenantID, LeadID: leadID,
		AssignedToMembershipID: testReminderAssignee,
		SourceCallLogID:        testCallLogID,
		Type:                   reminder.TypeCallback,
		DueAt:                  dueAt(),
	}
	if _, err := h.Handle(t.Context(), cmd); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	out, err := h.Handle(t.Context(), cmd)
	if err != nil {
		t.Fatalf("second Handle: %v", err)
	}
	if !out.AlreadyExisted {
		t.Fatal("AlreadyExisted should be true on duplicate")
	}
}

// ----- Mature-lead reminder -------------------------------------------------

func TestCreateReminder_MatureLeadHappyPath(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	reminders := newFakeReminders()
	leadID := seedLead(t, leads)
	h := command.NewCreateReminderHandler(leads, reminders, fixedTime, newTestReminderID)
	out, err := h.Handle(t.Context(), command.CreateReminderCommand{
		TenantID: testTenantID, LeadID: leadID,
		AssignedToMembershipID: testReminderAssignee,
		Type:                   reminder.TypeMatureLead,
		DueAt:                  dueAt(),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if out.ReminderID.IsZero() {
		t.Fatal("ReminderID should be set")
	}
}

func TestCreateReminder_MatureLeadDuplicateIsAcked(t *testing.T) {
	t.Parallel()
	leads := newFakeLeads()
	reminders := newFakeReminders()
	leadID := seedLead(t, leads)
	h := command.NewCreateReminderHandler(leads, reminders, fixedTime, newTestReminderID)
	cmd := command.CreateReminderCommand{
		TenantID: testTenantID, LeadID: leadID,
		AssignedToMembershipID: testReminderAssignee,
		Type:                   reminder.TypeMatureLead,
		DueAt:                  dueAt(),
	}
	if _, err := h.Handle(t.Context(), cmd); err != nil {
		t.Fatalf("first Handle: %v", err)
	}
	out, err := h.Handle(t.Context(), cmd)
	if err != nil {
		t.Fatalf("second Handle: %v", err)
	}
	if !out.AlreadyExisted {
		t.Fatal("AlreadyExisted should be true on duplicate")
	}
}

// ----- Input validation -----------------------------------------------------

func TestCreateReminder_RejectsZeroTenantID(t *testing.T) {
	t.Parallel()
	h := command.NewCreateReminderHandler(newFakeLeads(), newFakeReminders(), fixedTime, newTestReminderID)
	_, err := h.Handle(t.Context(), command.CreateReminderCommand{
		TenantID: tenant.ID(""), LeadID: crmlead.ID("01923400-0000-7000-8000-bbbbbbbb0010"),
		AssignedToMembershipID: testReminderAssignee, Type: reminder.TypeManual, DueAt: dueAt(),
	})
	if err == nil {
		t.Fatal("want input-validation error on zero TenantID")
	}
}

func TestCreateReminder_RejectsZeroLeadID(t *testing.T) {
	t.Parallel()
	h := command.NewCreateReminderHandler(newFakeLeads(), newFakeReminders(), fixedTime, newTestReminderID)
	_, err := h.Handle(t.Context(), command.CreateReminderCommand{
		TenantID: testTenantID, LeadID: crmlead.ID(""),
		AssignedToMembershipID: testReminderAssignee, Type: reminder.TypeManual, DueAt: dueAt(),
	})
	if err == nil {
		t.Fatal("want input-validation error on zero LeadID")
	}
}

func TestCreateReminder_RejectsInvalidType(t *testing.T) {
	t.Parallel()
	h := command.NewCreateReminderHandler(newFakeLeads(), newFakeReminders(), fixedTime, newTestReminderID)
	_, err := h.Handle(t.Context(), command.CreateReminderCommand{
		TenantID: testTenantID, LeadID: crmlead.ID("01923400-0000-7000-8000-bbbbbbbb0010"),
		AssignedToMembershipID: testReminderAssignee, Type: reminder.Type("invented"), DueAt: dueAt(),
	})
	if err == nil {
		t.Fatal("want input-validation error on invalid Type")
	}
}

func TestCreateReminder_RejectsZeroDueAt(t *testing.T) {
	t.Parallel()
	h := command.NewCreateReminderHandler(newFakeLeads(), newFakeReminders(), fixedTime, newTestReminderID)
	_, err := h.Handle(t.Context(), command.CreateReminderCommand{
		TenantID: testTenantID, LeadID: crmlead.ID("01923400-0000-7000-8000-bbbbbbbb0010"),
		AssignedToMembershipID: testReminderAssignee, Type: reminder.TypeManual, DueAt: time.Time{},
	})
	if err == nil {
		t.Fatal("want input-validation error on zero DueAt")
	}
}
