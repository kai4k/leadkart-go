package query_test

import (
	"testing"
	"time"

	"github.com/leadkart/leadkart-go/internal/common/ids"
	"github.com/leadkart/leadkart-go/internal/crm/app/query"
	"github.com/leadkart/leadkart-go/internal/crm/domain/crmlead"
	"github.com/leadkart/leadkart-go/internal/crm/domain/reminder"
	"github.com/leadkart/leadkart-go/internal/crm/domain/reminder/remindertest"
	"github.com/leadkart/leadkart-go/internal/identity/domain/tenant"
)

const (
	testTenantID = tenant.ID("01923400-0000-7000-8000-aaaaaaaa0002")
	leadIDA      = crmlead.ID("01923400-0000-7000-8000-aaaaaaaa0010")
	assigneeA    = "01923400-0000-7000-8000-aaaaaaaa0011"
	assigneeB    = "01923400-0000-7000-8000-aaaaaaaa0012"
)

func pinnedNow() time.Time { return time.Date(2026, 6, 2, 9, 0, 0, 0, time.UTC) }

func seedReminder(t *testing.T, r *remindertest.FakeRepository, assignee string, dueAt time.Time) reminder.ID {
	t.Helper()
	id := reminder.ID(ids.NewV7().String())
	rem, err := reminder.NewManualReminder(
		id, testTenantID, leadIDA, assignee, assigneeA,
		dueAt, "", pinnedNow(),
	)
	if err != nil {
		t.Fatalf("seed factory: %v", err)
	}
	if err := r.Add(t.Context(), rem); err != nil {
		t.Fatalf("seed Add: %v", err)
	}
	return id
}

func TestListPendingReminders_HappyPath(t *testing.T) {
	t.Parallel()
	reminders := remindertest.NewFakeRepository()
	seedReminder(t, reminders, assigneeA, pinnedNow().Add(time.Hour))
	seedReminder(t, reminders, assigneeA, pinnedNow().Add(2*time.Hour))
	seedReminder(t, reminders, assigneeB, pinnedNow().Add(3*time.Hour))
	h := query.NewListPendingRemindersHandler(reminders)
	page, err := h.Handle(t.Context(), query.ListPendingRemindersQuery{
		TenantID: testTenantID,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(page.Items) != 3 {
		t.Fatalf("items: %d (want 3)", len(page.Items))
	}
}

func TestListPendingReminders_SelfFilter(t *testing.T) {
	t.Parallel()
	reminders := remindertest.NewFakeRepository()
	seedReminder(t, reminders, assigneeA, pinnedNow().Add(time.Hour))
	seedReminder(t, reminders, assigneeB, pinnedNow().Add(2*time.Hour))
	h := query.NewListPendingRemindersHandler(reminders)
	page, err := h.Handle(t.Context(), query.ListPendingRemindersQuery{
		TenantID:   testTenantID,
		SelfFilter: assigneeA,
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("items: %d (want 1)", len(page.Items))
	}
	if page.Items[0].AssignedToMembershipID != assigneeA {
		t.Fatalf("assignee: %q (want %q)", page.Items[0].AssignedToMembershipID, assigneeA)
	}
}

func TestListPendingReminders_FilterByType(t *testing.T) {
	t.Parallel()
	reminders := remindertest.NewFakeRepository()
	seedReminder(t, reminders, assigneeA, pinnedNow().Add(time.Hour))
	// seed a callback reminder
	cb, err := reminder.NewCallbackReminder(
		reminder.ID(ids.NewV7().String()), testTenantID, leadIDA, assigneeA, assigneeA,
		"01923400-0000-7000-8000-aaaaaaaa0099",
		pinnedNow().Add(30*time.Minute), "", pinnedNow(),
	)
	if err != nil {
		t.Fatalf("seed callback: %v", err)
	}
	if err := reminders.Add(t.Context(), cb); err != nil {
		t.Fatalf("seed callback Add: %v", err)
	}
	h := query.NewListPendingRemindersHandler(reminders)
	page, err := h.Handle(t.Context(), query.ListPendingRemindersQuery{
		TenantID: testTenantID,
		Filter:   reminder.PendingFilter{Type: reminder.TypeCallback},
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(page.Items) != 1 {
		t.Fatalf("items: %d (want 1)", len(page.Items))
	}
	if page.Items[0].Type != reminder.TypeCallback.String() {
		t.Fatalf("type: %q", page.Items[0].Type)
	}
}

func TestListPendingReminders_RejectsZeroTenant(t *testing.T) {
	t.Parallel()
	reminders := remindertest.NewFakeRepository()
	h := query.NewListPendingRemindersHandler(reminders)
	_, err := h.Handle(t.Context(), query.ListPendingRemindersQuery{
		TenantID: tenant.ID(""),
	})
	if err == nil {
		t.Fatal("want input-validation error on zero TenantID")
	}
}
